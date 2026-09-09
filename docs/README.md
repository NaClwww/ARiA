# ARiA 设计文档

通用 LLM 助手：核心以**可嵌入引擎库**交付，由宿主程序（测试/未来的语音、视频服务）驱动；产品入口（CLI/HTTP）**暂缓**，将来加入时只是事件流之上的薄壳。未来接语音/视频触发源。参考系 pi（badlogic）的极简 agent loop，强化 ctx 传播、插件化与非线性 context 规划。

## 架构总览

```
              ┌─────────────── 宿主（引擎的嵌入方）───────────────┐
  触发源：文本 / 语音 / 视频 / 定时 → 制造 Scope、Run/Queue/Interrupt │
  输出：渲染 / TTS / 落盘 ← Subscribe 事件流（durable+volatile）   │
              └──────────────────┬───────────────────────────────┘
                                 │
┌────────────────────────────────▼───────────────────────────────┐
│ runtime（context-build）                                        │
│   插件中介：Setup 唯一装配 · 构造注入 · 数据传递三类              │
│   窗口生成：Assembler 链（每轮折叠各来源 → []Message）            │
│   事件溯源持久化：durable 订阅 → 窄 Store 接口（Setup 注入实现）   │
│   调度/并发（G1 草稿，M2 前重论）                                 │
└───┬────────────────────────────────────────────▲───────────────┘
    │ 依赖                                       │ 只经契约消费
    ▼                                            │（不 import 实现）
┌──────────────────────────┐   实现契约  ┌───────┴────────────────┐
│ core（agent-loop）        │◀───────────│ plugins                 │
│ 单线程飞轮                │            │ Provider 适配            │
│ Run/Queue/Interrupt/     │            │ Tool / ToolSource       │
│   Subscribe 四入口        │            │   （含 MCP 桥，M4）      │
│ 槽1 Assembler/槽2 Guard   │            │ ContextSource            │
│ 事件总线 durable/volatile │            │ （记忆/RAG/画像/静态知识）│
└───────────┬──────────────┘            └───────────┬────────────┘
            └──────────────────┬─────────────────────┘
                               ▼
              ┌─────────────────────────────────────┐
              │ pkg：ctxx · message 规范模型 · slog    │
              └─────────────────────────────────────┘
```

**IO 通道全部上移到宿主层，内核零感知**：输入统一成多模态 Message 块（01 §2）经 Run/Queue 进入；输出统一走事件订阅。每接一个新通道（语音/视频/定时）= 加一个新宿主薄壳，core/runtime 一行不动。

**子系统（表情/动作/人格等派生决策）不经新机制，落在既有三条通道上**：

| 形态 | 例子 | 通道 |
|---|---|---|
| 派生型：从输出推导 | TTS 语调、Live2D 口型 | volatile 事件订阅（实时驱动） |
| 约定型：模型打标记 | 文本夹 `[happy]` | marker 约定 + Provider 装饰器剥除 |
| 决策型：模型显式选择 | `motion.perform("挥手")` | 普通 Tool（Guard 可见、durable 落痕） |
| 人格连续型：记得心情 | "现在有点不耐烦"进窗口 | 子系统同时注册为 ContextSource（Observe 写、Collect 注入） |
| 重决策型：另跑小模型 | 情感分类、蒸馏 | Observe 慢路径 / runtime 后台任务（G1），永不进飞轮 |

v1 起步建议：派生型 + 约定型最便宜；动作有语义分量时上决策型 Tool；人格子系统随 G2 记忆讨论定稿。

## 分层与文档地图

```
pkg ──── agent-loop（agent-core）──── context-build（runtime）──── plugins（依赖、etc）
基础件        单线程飞轮                  会话/调度/窗口/规划            实现：provider/tool/memory/MCP
```

| 层 | 文档 | 职责一句话 | 依赖规则 |
|---|---|---|---|
| pkg | [01-pkg.md](01-pkg.md) | ctx 传播基础件 + 规范消息模型 | 零依赖 |
| agent-core | [02-agent-core.md](02-agent-core.md) | agent loop 飞轮、事件总线、steering、core 接口 | 只依赖 pkg |
| context-build（runtime） | [03-context-build.md](03-context-build.md) | 插件中介、窗口生成（意图级）、扩展机制；调度与规划细节在 notes/ 草稿 | 依赖 core+pkg；只经契约消费插件 |
| plugins | [04-plugins.md](04-plugins.md) | provider 适配器、工具源、MCP 桥、记忆实现、依赖清单 | 实现 core/runtime 契约，不反向依赖 |
| 未决 | [05-open-questions.md](05-open-questions.md) | 尚未讨论完的问题清单（收敛中） | —— |

目录名映射：`pkg/`、`core/`（= agent-loop）、`runtime/`（= context-build）、`plugins/`。装配点 = 库暴露的唯一 Setup 装配函数（当前消费者是嵌入方/集成测试；未来入口只是新的嵌入方）——core 与 runtime 不 import 任何具体插件包。

## 关键决策速览

1. **ctx 是脊柱**：Scope/Trace/Credentials/Budget/Options 经 `pkg/ctxx` 类型化存取，业务层贯穿到底层；run ctx 父级是 session 而非触发请求；Scope 缺失 fail-closed。
2. **core = 单线程飞轮**：一次 Run 的全部可变状态归一个 goroutine，无锁；入口只有 Run/Queue/Interrupt/Subscribe；loop 零磁盘 I/O。
3. **事件分级**：durable（载荷完整、存活订阅者内不丢、可重放重建会话）与 volatile（可合并丢）；一切 UI/服务/持久化都是事件消费者。
4. **core 只有两个钩子槽**：Assembler（窗口组装/记忆注入唯一入口）+ ToolGuard（审批）；其余一切在 runtime 层组合。
5. **ContextWindow 是一等可编程状态，非线性规划是方向**：选择非线性、呈现线性稳定（importance ≠ recency）；细节未定稿——草稿 notes/context-planning-draft.md，M3 前重论（05 G2）。
6. **MCP 经 mcp-to-tool 桥**变成原生 Tool；插件 v1 = 编译期接口注册，跨语言二期 MCP/gRPC 子进程。
7. **错误三分法**：ToolError 是内容（喂回模型）、ProviderError 退避重试、Fatal 终止。
8. **插件契约极简（5 接口 10 方法）**：Provider / Tool / Assembler / ToolSource / **ContextSource**（Name/Collect/Observe——窗口生成契约：记忆、RAG、画像、静态知识都是给窗口供给 Unit 的源；Collect 快路径同步、Observe 慢路径异步幂等；蒸馏是源内部管线而非契约）。依赖 = 类型化构造注入，**Go 类型系统即校验**；数据 = 契约类型 / artifact+ref / ctx，插件间不直接调用、无万能信封。
9. **扩展 = 普通 Go 组合，无 Hook 抽象（03 §3）**：core 运行中不可变——读 = 事件订阅，改 = 挂点上的链/装饰器（ChainAssembler/ChainGuard + Tool/Provider 装饰 + 窗口命令），控制 = Interrupt/Queue；tool hook 三层分工：工具内部 / 全局装饰 / Guard 决策；core 只见折叠后的单个实现。
10. **一切构件确定性可测**：FakeProvider 表驱动，无网络无磁盘，`-race` 干净。

runtime 并发调度的详细设计已降级草稿（notes/concurrency-draft.md；原则方向：内存状态走 actor、信号量只守外部资源），M2 前重论（05 G1）。

## 里程碑

| 阶段 | 内容 |
|---|---|
| M1 | pkg + core：飞轮跑通多轮工具调用（OpenAI 兼容 provider，集成测试验收） |
| M2 | 内置工具源 + 嵌入装配 API（并发调度届时已重论定稿，最小实现） |
| M3 | ContextSource 默认记忆源 + 窗口基线（Planner 细节届时已重论定稿） |
| M4 | MCP 桥 |
| M5 | 语音/视频触发源 |

产品入口（CLI/HTTP）不与里程碑绑定，任何时候可作为薄壳加入（事件流的订阅者 + Scope 制造者）。

## 未决问题

**讨论尚未收敛**——未决问题全集见 [05-open-questions.md](05-open-questions.md)，按里程碑标注优先级；每定案一项即回写对应层文档并从清单删去，该文件趋空 = 设计完成。
