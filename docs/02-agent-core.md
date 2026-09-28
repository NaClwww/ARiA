# 02 · agent-core —— agent loop（单线程飞轮）

> 依赖：仅 pkg。core 定义接口（Provider/Tool/Assembler/ToolGuard），实现在 plugins，组装在 runtime/cmd。core 零磁盘 I/O、零业务。

## 1. 飞轮模型

一个 `Loop` 实例的一次 `Run`，全部可变状态——messages、轮次、预算消耗、队列消费位置——只归**一个 goroutine** 所有。飞轮内部零锁（Budget 原子扣减除外）。

**并发安全入口只有四个**：

| 入口 | 语义 |
|---|---|
| `Run(parent, input)` | 阻塞执行一次完整运行；同实例禁止并发 Run |
| `Queue(msg)` | steering 输入，**轮间**注入 |
| `Interrupt()` | 取消当前 LLM 调用/工具执行/Guard 等待，保留部分输出，可续 |
| `Subscribe(buf)` | 事件订阅（channel + 分级丢弃；退订/停滞断开时关闭 channel） |

**core 没有也不需要投机/预测入口**：`Run` 无状态（每次由调用方传完整历史），「用预测输入提前生成、确认后复用」整体归 runtime（`runtime/speculate`，见 03 §4）——core 不为预测增加任何 API。**该能力已实现但优先级最低、v1 不接入**（2026-09-11）。

**三方纪律**：runtime 不得直接读写 loop 状态，只经入口；loop 不做磁盘 I/O（持久化 = runtime 订阅 durable 事件，事件溯源）；工具/Provider 的 I/O 阻塞飞轮 goroutine 允许，但必须尊重 ctx 取消。多实例（多伴侣）= 多飞轮并行；单实例内 v1 一 Session 同时只跑一轮（03 §5）。

## 2. 一次 Run 的展开

```go
func (l *Loop) Run(parent context.Context, input []Message) (RunResult, error) {
    runCtx, cancel := context.WithCancel(parent); defer cancel()
    l.reset(input); l.emit(AgentStart)
    for turn := 0; ; turn++ {
        if end := l.preFlight(turn); end != nil { return l.finish(*end) } // 预算/轮次
        msgs := l.assemble(runCtx)        // 槽 1：Assembler（默认恒等透传）
        resp := l.streamLLM(runCtx, msgs) // 流式；取消时保留部分 assistant 消息
        l.append(resp.Message); l.consumeUsage(resp.Usage)
        if len(resp.ToolCalls) == 0 && l.queue.Empty() { break }
        for _, tc := range resp.ToolCalls {          // 顺序执行
            if d := l.guard(runCtx, tc); d.Denied { l.append(d.AsToolResult()); continue } // Guard 决议与被拒结果均落 durable 事件
            res := l.execTool(runCtx, tc)            // WithTimeout；取消→部分结果
            l.append(res)
        }
        l.drainQueue()                    // steering 输入轮间注入（每条 emit UserMessageInjected）
    }
    return l.finish(EndDone)
}
```

轮（turn）= 恰好一次 LLM 调用 + 其后顺序执行的全部工具；`RunResult.Turns` 按实际发起的 LLM 调用计（重试不增加；preflight 终止为 0）。`MaxTurns`：`0` = 未设置走默认 100，负数 = 无限制（「loop until done」+ 默认有闸）。

## 3. 事件体系

### 3.1 词汇与分级

| 事件 | 载荷 | 级别 |
|---|---|---|
| AgentStart | Scope、RunID、InitialInput []Message | durable |
| UserMessageInjected | Message（Queue/steering 注入的用户输入） | durable |
| TurnStart / TurnEnd | 轮次与窗口摘要 / 本轮 Usage 快照 | durable |
| MessageStart | Role、MessageID | durable |
| MessageUpdate | 文本/思考增量 | **volatile** |
| MessageEnd | 完整 assistant Message（Blocks、ToolCalls、Usage、Interrupted） | durable |
| ToolGuardDecision | CallID、Decision（Allow/Deny/Rewrite）、Reason | durable |
| ToolExecStart / ToolExecEnd | ToolCall / 完整 ToolResult（Guard 拒绝也发 End 并标明被拒） | durable |
| Progress | 工具进度（MCP progress 映射） | **volatile** |
| AgentEnd | RunResult（EndReason、总 usage、Error） | durable |

**durable**：载荷完整、存活订阅者内不丢——runtime 靠它完整重建会话历史（事件溯源是「loop 零磁盘」的前提）。**volatile**：仅渲染用，缓冲满即合并/替换；MessageEnd 永远带全文，丢增量无损。

**事件溯源重建**：runtime 按序消费 durable 事件即可 1:1 还原会话——AgentStart → 初始输入；UserMessageInjected → 轮间注入；MessageEnd → assistant 消息；ToolExecEnd → tool 消息（含被拒结果）；ToolGuardDecision → 审计轨迹。**这是 core 侧保证的能力**：runtime 的 persist 因此只需订阅落盘（v1 只写不恢复，03 §5；跨天续聊由压缩摘要持久化支撑）。

### 3.2 总线纪律

emit 在飞轮 goroutine 内同步入队但绝不阻塞飞轮：每订阅者**一条内部 FIFO + 唯一投递协程**（唯一写者，也是唯一关闭者——退订/停滞断开时关闭 channel，消费者可 `range` 收尾）。volatile 满则丢（MessageEnd 永远带全文）；durable 严格按 emit 顺序投递、满则积压（防御上限后断开），订阅者停滞超阈值（默认 5s）判定故障断开（durable 的「不丢」以订阅者存活为界）。事件载荷按订阅者深拷贝，消费者无法篡改共享底层。复杂度关死在 `bus.go`。

## 4. 两个钩子槽（core 仅有的扩展点）

```go
// 槽 1：窗口组装 —— 记忆/上下文注入的唯一入口
type Assembler interface{ Assemble(ctx context.Context, s State) []Message } // 默认恒等
// 槽 2：工具守卫 —— 审批/审计/改写
type ToolGuard interface{ Check(ctx context.Context, tc ToolCall) Decision } // Allow/Deny/Rewrite
```

Guard 阻塞飞轮等人类审批是**正确**语义（批准前什么都不该跑），但等待挂可取消 ctx：`Interrupt()` 能解除 Guard 等待（按拒绝收场，剩余调用补结果）。Rewrite 只允许改 name/args，**ToolCall ID 由 core 强制保留**（否则 assistant 调用与 tool 结果失配，provider 重放会拒收）；决议事件记录 original 与 effective 双份。记忆体系（ContextSource 契约）在 runtime/plugins，core 无感知。runtime 在这两个槽上如何组织扩展（链折叠、装饰器、读通道）见 03 §3。

## 5. Provider / Tool 接口

```go
type Provider interface {
    Stream(ctx context.Context, req Request) (<-chan Event, error)
    // Request: 组装后消息 + ToolDefs + Options(ctxx 读出)
    // Provider Event: PartDelta / MessageComplete(含 Usage) / Error(分类)
}
type Tool interface {
    Def() ToolDef                                        // name, description, JSON schema
    Exec(ctx context.Context, call Call) Result
}
```

- Provider 是哑管道，事件词汇由 loop 翻译；**部分结果语义是硬性义务**：ctx 取消时必须以 `MessageComplete(interrupted=true)` 收尾返回已收内容，不得只回 error——这是 Interrupt 可续的前提。
- 适配器见 04-plugins；限流包装（LimitedProvider）属 runtime，core 无感知。

## 6. Steering 语义

| 动作 | 行为 |
|---|---|
| `Queue` | 运行中入队（轮间：工具执行完、下次 LLM 调用前追加为 user 消息）；空闲/已收敛时返回 `ErrNoActiveRun`——入队与收敛判定同锁原子 |
| `Interrupt` | cancel 当前调用 → 保留部分 assistant 消息 → 队列有输入则续轮，否则 EndInterrupted |
| parent 取消 | 硬终止 EndCancelled，不尝试续跑 |

Interrupt（转向）≠ parent 取消（关机）：loop 内部持有 runCtx cancel，恢复点先查 `parent.Err()`。

## 7. 预算与错误

- 检查点：每次 LLM 调用前 preFlight 查 `ctxx.Budget`；MessageComplete 后扣减；超限 EndReason=BudgetExhausted，可配置允许最后一轮无工具收尾。
- **错误三分法**：ToolError → 转成 toolResult 喂回模型（内容不是故障）；ProviderError{Retryable} → 指数退避重试；FatalError（预算/轮次/parent 取消）→ 终止。

## 8. 可测试性（core 的验收标准）

单线程 = 给定相同 Provider/Tool 应答序列行为完全确定：FakeProvider（脚本化应答、模拟取消/限流）+ FakeTool（注错、慢执行）表驱动覆盖——多轮工具循环、queue 注入时序、interrupt 部分保留与续跑、预算终止、guard 拒绝后模型看到错误自行改道、volatile 合并丢增量但 durable 完整。全部无网络无磁盘，`-race` 干净。

## 9. 演进预留（v1 不做）

- **并行工具执行**：多个 toolCalls 时扇出 goroutine、channel 收集、回填仍收敛飞轮 goroutine；
- **子运行**：工具的实现再起一个 Loop（pi 用「agent 经 bash 启动自己」回避，我们暂不引入）。

## 10. M1 范围与验收

`pkg/ctxx` + `pkg/message` + `core/provider`（接口+fake）+ `core/tool` + `core/loop`（bus/steering/全套测试）；OpenAI 兼容适配器在 `plugins/provider/openai`（live 测试无 key 自动 skip）。
验收：§8 场景全绿；集成测试驱动真实 OpenAI 兼容 provider 跑通多轮工具调用；日志全带 trace_id；`-race` 干净。
