# 2026-09-11 · runtime 层设计与实时交互模型专题讨论

> 起因：M1（pkg+core）落地、speculate 已定稿后，开始设计 runtime 层其余部分。
> **已全部拍板并合入 03**：§5（运行时结构）、§6（ingress）、§7（旁路调用纪律），文档头部分间描述同步更新。
> **文档统一（2026-09-11 同日）**：README（架构图/分层地图/决策 5/里程碑 M2-M5/并发段）、01（Scope 说话人、session ctx）、02（多实例纪律、事件溯源/只写不恢复）、04（装配图去 Scheduler/SessionManager、Setup 签名、§4 引用）、05（G1 解除阻塞、G2/A2/C1/E 组、G3 事件词汇、讨论顺序）、notes 两草稿头部——全部对齐新结论。03 章节重排为 §1→§7 顺序。本记录保留完整备选与理由存档。

---

## 0. 结论总览（拍板状态）

| # | 议题 | 结论 | 状态 |
|---|---|---|---|
| — | runtime 重心排序 | ① 每轮组装链（压缩+感知叠加）→ ② persist 写路 → ③ speculate 接入 | 用户对齐确认 |
| D1 | 要不要 Session 总装对象 | 要：长寿命单例，持「记忆 + ingress 状态 + 投机槽」 | ✅ 拍板 |
| D2 | 并发调度 | v1 用互斥 + core.Queue 排队；schedule/ 子包推迟，G1 不提前定稿 | ✅ 拍板 |
| D3 | Input 调用形态 | 阻塞式（宿主经事件订阅拿增量，TTS 不等整句） | ✅ 拍板 |
| D4 | 窗口命令（注入/置顶/驱逐） | v1 不做，M3 随非线性窗口一起 | ✅ 拍板 |
| D5 | 目录结构 | `runtime/{agent,ingress,window,persist,speculate}` 五包 | ✅ 拍板 |
| D6 | 会话边界 | **不分会话**：一实例 = 一长寿命 Session；「新开 vs 继承」伪问题删除；边界由话题判终承担 | ✅ 拍板 |
| D7 | 成轮/判终判定 | v1 纯规则（静音+标点），判定器留可替换接口 | ✅ 拍板 |
| D8 | 抢话（B 打断 A） | 小轮应答：Interrupt + 固定话术垫话 + 主循环接管；小轮直连 Provider 不走 loop | ✅ 拍板（固定话术版） |
| D9 | 输入预测（speculate）优先级 | **降至最低：代码已就绪但 v1 不接线**，Session 不依赖；等语音链路跑通、收益可量化后再启用 | ✅ 拍板（2026-09-11） |
| D10 | M2 主线范围 | **只做一轮对话通路**：persist + window + agent 三包；ingress 随语音 M5、小轮/抢话、投机、多人归主全部推迟 | ✅ 拍板（2026-09-11） |

---

## 1. 组装模型（对齐确认，runtime 核心）

**每轮上下文 = 压缩记忆 + 当前感知（照片等）+ 新输入**，不是「历史攒长了才裁」。

- 压缩是**每轮的常规步骤**而非救火：历史永远以「上一轮压缩后的形态」存在，每轮往上叠新东西；上下文规模每轮归零再生长，延迟稳定——对实时场景友好；
- **间隙压缩**：压缩本身是一次 LLM 调用，不能放在下一轮组装的同步路径上。挪到轮次间隙异步做：第 N 轮结束后后台把该轮窗口压成记忆摘要；第 N+1 轮组装时摘要没压完就用上一轮的（慢一点但不出错）；
- **当前照片**：视觉快照作为 Message Block（ImageBlock）进组装链，与文本同一套规范模型（pkg/message 已备）。「只保留当前帧、旧帧价值由压缩摘要承载」——✅ 2026-09-28 拍板并落地（`plugins/vision/gowild`）：B 形态，每次 LLM 调用组装时现拉一帧（`GET /api/camera/frame`，2s 硬顶）追加在组装结果**最底部**（agent 槽 1 额外变换位，作用在窗口组装结果之上）；不进历史、不落盘、失败不注入（无占位、无旧帧兜底）——每请求恰好一张「此刻」帧，旧帧永不重发；
- 投机的开启条件与组装链联动：**成轮置信度高、猜测内容明确时才 Speculate**；数据不够（ASR 才出两字）就跳过。这是宿主侧判断，runtime 不需新机制，但 Session 文档须写明「投机是机会性的，可以有也可以没有」。

## 2. 会话模型：不分会话（D6）

- 一个伴侣实例 = 一个长寿命 Session，从启动活到关闭，**没有「新开」动作**；
- 「隔天继续聊」不是恢复会话，是同一会话继续——每轮组装本来就用压缩记忆，睡一夜和聊完一个话题机制相同；
- **对话边界由话题判终承担**（ingress 职责），不由会话承担；
- 代价与对策：多人共享记忆（群聊语义，桌面伴侣场景是特性）；「私聊模式」未来作为 Assembler 规则（「带 X 标签的话题在 X 在场时才召回」），不推翻模型；
- 被否方案：按人/按时间间隔切会话——把语音连续性问题错误地推给会话生命周期，且和「人格连续型」方向冲突。

## 3. 输入形成层 ingress（对齐确认）

Miloco（小米开源全屋智能，github.com/XiaoMi/xiaomi-miloco）参照：感知有闸门（Silero VAD 守语音场）、身份融合判定+名册沉淀、感知引擎高频廉价与 LLM 分层、记忆从感知蒸馏。映射到 ARiA：

**ingress = 成轮状态机**：把零碎原始感知加工成「一条完整、有主人的用户输入」，才交给 Session。

```
VAD 信号    ─┐
ASR 增量    ─┼─▶ ① 分段：VAD 静音切出「一口气」
人脸事件    ─┤   ② 成轮：说完了吗（规则：静音阈值 + 标点收尾）
             │   ③ 归主：谁说的（查名册 → 挂到输入）
             ┘   ④ 判终：话题结束了吗（→ 触发间隙压缩归档）
                    ↓
        Session.Input(msg, speaker)
```

分层纪律：

| 东西 | 归属 | 理由 |
|---|---|---|
| 成轮状态机、判终、归主 | runtime/ingress | 通用策略 |
| VAD / ASR / 人脸识别模型 | 插件（能力提供者）或宿主 | 有驱动依赖、可替换（03 §1） |
| 「输入必须带主人（UserID）」 | runtime 硬约束，拒绝无主输入 | 身份贯穿预算/凭据/记忆 |
| 名册（脸→用户）存储 | 插件，窄接口注入 | 同 persist 能力注入规矩 |

**多人共享一个 Session**（D6 推论）：Session 不绑 UserID，每条输入绑。说话人落两处——Message 层（speaker 标签进历史，LLM 知道谁在说话）+ Scope 层（当次 Input 的 ctx UserID = 说话人，预算凭据跟着走）。

## 4. 抢话（D8）：小轮应答

B 在 A 的回答进行中插嘴：

```
1. B 的话在 ingress 成轮
2. Interrupt() 打断当前回答（A 的半截回答以 Interrupted 形态留历史，core 已有行为）
3. 小轮：对 B 轻量应答（「听到了，稍等」量级）
4. 小轮结束后 B 的完整输入作为下一轮主输入进主循环
```

- **小轮 = 旁路调用**：直连 Provider 调 Stream，不走 loop——与 speculate 同构（预测是「提前替用户跑」，小轮是「提前替主循环垫话」）；
- **v1 固定话术**（不调 LLM）：回应延迟预算 200–300ms，LLM 最乐观 300ms 起步；即时感价值 ≫ 应答聪明程度。「先固定后 LLM」是真升级不是返工，接口为 `func(input) reply` 可替换函数；
- **小轮消息进下一次主 Run 的 input** → 落盘、进 transcript、被压缩，全白捡，persist 无需新路；
- 维持「只有一个 loop」红线。

**旁路调用纪律**（speculate 与小轮共享）：无工具、无 durable、结果经下一次 Run 的 input 转正。⚠️ 有第三个实例（如主动寒暄）再抽公共结构，遵守三次原则，v1 两者各自站着。

**⚠️ 回声自打断防线**：伴侣播 TTS 时自己的声音被收音 → VAD 误判插嘴 → 自杀式打断。AEC/播放中不收音是宿主职责，但 runtime 提供安全绳：**发声中降敏**——TTS 播放中新成轮不触发打断、只进队列。便宜，挡住必然发生的线上问题。

## 5. 结构拍板（D1–D5）

- **Agent** = Setup 装配产物：拢 Provider/Tools/Store/名册，提供 `NewSession`；
- **Session** = 长寿命单例容器：记忆 + ingress 状态 + 投机槽；`Input(ctx, msg) (RunResult, error)` 阻塞式；
- **persist** v1 只写不恢复：`Append(ctx, sessionID, ev)` 窄接口定义在 runtime/persist，SQLite 等实现放 plugins 经 Setup 注入；「跨天续聊」由压缩摘要持久化支撑，事件重放推到真有需求再说；
- **window** v1 = 组装链（§1 模型）；非线性 Planner 仍 M3；
- **并发**：一 Session 同时只跑一轮，新输入走 core.Queue 排队；schedule/ 不建；
- **core / pkg 零改动**——本轮全部设计在 runtime 内消化。

## 待定决策清单

- （无未决；D1–D9 全部拍板。照片保鲜策略默认「只留当前帧」待实现前顺口确认。）

## D9 · 输入预测降级（2026-09-11 追加拍板）

用户定调：「输入预测先放着，感觉很难实现。优先级降到最低」——成轮置信度判定、猜测内容生成、与语音链路的耦合都不轻，先做有确定收益的部分。

落地口径：**代码保留不删**（runtime/speculate 已实现且有测试，删了浪费），**v1 不接线**——Session 不持投机槽的实际使用，组装链不含「命中投机 → 拼接预生成答案」这条路径，ingress 的投机闸门（03 §6）不实现。03 §4 加降级横幅、§5/§6/§7 同步标注，05 新增 G0 条（不阻塞任何里程碑），README 架构图与 M2 里程碑改为「runtime 四包 + speculate 暂不接入」。

被否的备选：直接删除 speculate 代码——否决理由：设计已定稿、实现已完成、零维护成本（不接线即不运行），删除反而丢失已验证的机制。

## D10 · M2 主线范围与实现（2026-09-11 追加拍板）

用户定调：「我们先只做主线吧」——先把**一轮对话的完整通路**跑通，边缘机制全部推迟。

**做**（三包，已实现并通过 gofmt/vet/test/race/重复测试）：

| 包 | 内容 |
|---|---|
| `runtime/window` | 每轮组装（压缩记忆 + 近轮 + 新输入）；`Settle` 在轮间隙异步压缩；`KeepLast` 兜底 + `ProviderCompressor`（LLM 摘要）；压缩失败退回未压缩（不丢内容）；压缩在途的 inflight 消息仍参与组装 |
| `runtime/persist` | `Store` 窄接口（定义在消费方）+ `Recorder.Consume`（durable 落盘、volatile 跳过、写失败冒泡） |
| `runtime/agent` | `Agent`（零件盒：Provider/Tools/Compressor/Store/Assembler/Guard）+ `NewSession`；`Session.Input` 阻塞且互斥、注入 Scope、等 AgentEnd 结算后返回；内部两条订阅（窗口结算 + 落盘） |

**不做**：ingress 成轮状态机（随语音 M5，纯文本宿主直接给完整 Message）、小轮/抢话（03 §7）、投机（D9/G0）、多人归主与名册、窗口命令。

**实现中确认的两条设计细节**（回写 03 §5）：
1. 组装结果作为 `Run` 的 input 进 core（不是把窗口塞进 core 槽 1 重跑）——因为 Run 内的多轮 LLM 调用共享同一份 assembly，槽 1 只保留给宿主的额外变换，否则第二转会重复前置记忆；
2. 压缩在途的消息必须继续参与组装（inflight），否则「用户趁压缩时说话」会让这些消息在上下文里短暂消失——这是被测试抓出来的真实缺陷。

## D11 · 主线代码审查与修复（2026-09-11）

用两个独立 subagent 分别从「并发/生命周期」与「语义/设计一致性」两个角度审查 `runtime/{window,persist,agent}`，两者**独立收敛到同一批缺陷**且各带可复现测试。已全部修复并加回归测试（含反向验证：把旧行为放回去，新测试立刻失败）：

| 级别 | 缺陷 | 修复 |
|---|---|---|
| P0 | `Input` 在宿主 ctx 取消时提前返回，未结算轮次被下一次 `beginTurn` 覆盖 → **整轮历史永久丢失且此后各轮错位**（两份审查各自复现 200/200） | `Input` 只在结算后返回（会话关闭除外）；取消仍由 Run 自身收场（EndCancelled）后正常结算 |
| P0 | `Close` 不唤醒在途 `Input`（永久阻塞）、关闭后仍能起新 Run | `closed` 通道；Input 入口拒绝 + 等待分支；Close 顺序改为「唤醒 → Interrupt 在途轮 → 等 Input 退出 → 退订排空 → 取消压缩 → 收尾 ctx」 |
| P1 | `beginTurn` 持有调用方消息切片别名（`-race` 实锤） | 入口 `msg.Clone()` |
| P1 | 压缩 ctx 丢失 Scope/Credentials/Trace（`ProviderCompressor` 会拿空身份发请求） | 压缩继承触发轮 ctx 的值（`Detached` 保 value、窗口自持 cancel 供 Close 取消） |
| P1 | 落盘失败后订阅者不退订：死订阅者积压到总线 4096 上限、durable「不丢」静默失效 | 消费者退出即 `punsub`；`Session.Err()` 暴露首个后台错误 |
| P1 | `Close` 先 cancel 导致已产出的 durable 事件整段不落盘 | Close 改为先退订（总线投递完剩余事件）再取消 |
| P1 | run ctx 父级是触发请求 ctx（违反 01 §1.2） | run ctx 值取自当次调用、生存期挂会话长活 ctx（`AfterFunc`） |
| P1 | Session 固定 Scope 覆盖宿主 Scope（违反 01 R2 与 §6「当次说话人」） | `SessionID` 锚定会话，`UserID`/`AgentID` 允许当次覆盖 |
| P2 | Compressor 返回空输出被当成功 → 已结算内容静默清零 | 空输出按失败处理（窗口退回未压缩） |
| P2 | 窗口 Failure 路径无上限、压缩不可取消、`Window()` 暴露写方法 | 失败回退加 `fallbackCap`；窗口持压缩 cancel 供 Close；`Window()` 改为只读 `History()` |
| P2 | 压缩失败路径/输出未深拷贝、`ProviderCompressor` 不读 ctx Options | 全部补 clone；Options 从 ctx 兜底（结构体字段优先） |

**未做（记录在案）**：压缩调用的 usage 未计入 Budget/日志（P2，随 M3 记忆源一起做）；`KeepLast` 丢弃更早内容无日志（在 `Config.Compressor` 注释中说明需显式接摘要实现）。

## D12 · 第二轮审查（对抗性验证，2026-09-11）

以「尝试证伪修复」为目标的第二轮审查，找到 5 个仍可触发的缺陷（其中 2 个可致 `Close` 永久挂死、1 个静默丢 durable），全部修复并加回归测试（含反向验证：移除修复后新测试必失败）：

| 级别 | 缺陷 | 修复 |
|---|---|---|
| P1（后果 P0） | **`Close` 的 `Interrupt` 会被 core.Run 入口的 `interrupt.Store(false)` 抹掉**——Close 与 Input 并发时，若 Interrupt 落在 Run 入口之前，在途轮不再收敛，而 Close 又在等它 → 互等死锁（自然时序下命中率与 `Assemble` 耗时正相关，大窗口 12/12） | Close 在 `Interrupt` 之后**直接取消会话 ctx**：在途 run ctx 经 `AfterFunc` 随之取消——终止保证不依赖 `Interrupt`（core 的 interrupt 是转向语义，会被 Run 重置） |
| P1（后果 P0） | **window `Settle` 与 `Close` 竞态**：cancel 在压缩 goroutine 内才注册，Settle 返回后、注册前到达的 Close 看不到它 → 等一个无法取消的压缩（GOMAXPROCS=1 下 3/3） | cancel 在 `Settle` 持锁时创建并注册，`compress` 只消费 |
| P1（后果 P0） | **`Close` 等落盘消费者，而消费者的 ctx 要等 Close 第 7 步才取消**：任何在 Close 时恰好阻塞在 `Append` 的 Store（哪怕完全尊重 ctx）都会让 Close 挂死 | 落盘用独立 ctx；Close 有界等待（`CloseGrace`，默认 2s），超时取消落盘并 warn（无视 ctx 的 Store 也不再拖死 Close） |
| P1 | **总线积压/停滞断开落盘订阅者 → durable 静默丢失**（实测 5780/6000 丢失，`Err()==nil`） | 消费者发现「流关闭但会话未关」→ `ErrStreamClosed`；窗口侧同样处理，并新增 `dead` 通道唤醒在途 Input（否则永久等待） |
| P2 | **`Queue` 在 Close 后静默丢弃**（无返回值、无事件、不进窗口） | `Queue` 返回 error：关闭/降级后拒绝 |

**降级语义**（本轮新增的明确契约）：后台故障不中断当前轮（对话继续），但**不让会话无声降级**——`Session.Err()` 暴露首个错误，此后 `Input`/`Queue` 返回 `ErrStreamClosed`。此前实现会在窗口结算链已死的情况下继续接受输入，属于隐患。
**已修被证伪的旧修复**：第一轮的 P1-2「`Session.Err()` 暴露」在总线断开路径上没有覆盖（Consume 返回 nil），本轮补齐。
**仍未做**：Store 无视 ctx 且永久阻塞时，落盘 goroutine 会被放弃（Close 已在 CloseGrace 内返回，goroutine 泄漏到 Append 返回为止）——这是「不阻塞关停」与「不丢数据」的取舍，选前者，已注释说明。

## D13 · 工具结果截断与 artifact（2026-09-11，用户点名）

用户需求：「完善 tool call：超过一定字符截断，并将 tool call 结果保存到一个地方给 agent 调用」。落点即 03 §1 早就写下的 **artifact + ref**：

- `runtime/artifact`：`Store` 窄接口（Put/Get，定义在消费方）+ `Memory` 默认实现（FIFO 淘汰，`NewMemory(n)`）+ `OpenTool`（`artifact.open`，按 ref 分段读取，字符/rune 计、UTF-8 安全，错误是内容不是 panic）；
- `runtime/toolkit.Truncate`：结果截断装饰器（03 §3 挂点 4/5）。超限 → 全文入库、模型只看到「提示 + 预览 + 可直接照抄的读取调用」；
- `agent` 装配：默认 `ToolResultLimit = 4000`（负数关闭），自动注册 `artifact.open`（宿主同名工具则跳过并告警），对除读取工具外的所有工具套装饰器；`Agent.Artifacts()` 暴露存放处。

**为什么这样切**：截断是**统一的策略**而不是每个插件的义务（此前 `shell` 之类各自截断），所以放 registry 全局包装；存档是**能力**，所以走注入的窄接口（内存/文件/对象存储可换）。三条纪律写死在装饰器里：非文本块不动、存档失败不截断（宁可长不可丢）、不包装读取工具自身（否则大块永远读不完）。

`artifact.open` 的归属从 04 的「M4 memory 源 pull 工具」前移：读取存档是 runtime 职责，不是记忆源的（04 §3 已同步）。

## D14 · 上下文分层与标签（2026-09-11，用户提议 XML 分块）

用户提议「用 XML 区分不同的块防止提示词注入」。讨论后的定位：**标签是概率手段，不是安全边界**——模型没有数据/指令硬边界，真正的边界是 role 结构（不可信内容永不进 system）+ 凭据不进上下文 + Guard 审批（危险动作要人类确认），标签是第四层。但值得做：它提高「数据被当数据」的概率，并让注入更可见。

**审查发现的自身违例**：`ProviderCompressor` 把压缩摘要产出为 **system 消息**——记忆内容来自对话（不可信），放 system 是给最高指令权重，与目标相反。已改为非 system 的 `<memory>` 块。

**参考 coding agent 的通行做法**（Anthropic 上下文工程文章 + aider repo map 文档）：① 作者手写的规则/人设（CLAUDE.md / AGENTS.md / .cursor/rules）进指令位——因为它们可信；② 检索/派生内容进数据位，且能 JIT 就不预塞（aider 的 repo map 每次带但预算 1k token、按依赖图排序）；③ 长期记忆常放上下文之外（文件式笔记、memory tool），上下文里只留引用；④ 压缩摘要替换历史起点（Claude Code 保留摘要 + 最近 5 个文件）。

**定稿规格**（已落 03 §5）：顺序固定 `[system] → [<memory>] → 近轮 → [<context source>] → 当前感知 → 当前输入`；不可信内容永不进 system；标签配 system 声明（`window.TagPolicyInstruction`）；内容里标签形态的 `<` 必须转义（`EscapeContent`，保留 `a < b`）；词汇小而定死（`memory`/`context`，新增走评审）。实现：`runtime/window/present.go` + `Window.SetSystem` + `agent.Config.SystemPrompt`。

## 遗留观察（不阻塞）

- OpenAI adapter `toWire` 值形态 type switch 静默剥掉指针 Block（openai.go:285-300）——已知未修，五小改动，用户点头即修；
- G1（并发调度）维持草稿状态，M2 不重论（D2 已用互斥绕过）；
- 「主动寒暄」若出现 = 旁路调用第三实例，届时抽公共结构。
