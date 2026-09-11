# 03 · context-build —— runtime 层

> 当前共识仅覆盖三块：**插件中介（§1）、窗口生成·意图级（§2）、扩展机制（§3）**。
> 并发调度与上下文构建的详细设计已降级为草稿（notes/），M2/M3 前重论——见 [05](05-open-questions.md) G 组。
> runtime 内部分间方向：window/（Assembler 链+Planner）、persist/（事件溯源）、schedule/（G1）、ingress/（触发入口）、subsystem/（表情/动作/TTS 等事件消费者群）——都是子包，不升格为新层；目录结构 M2 落定。

## 1. 插件中介：依赖装配与数据传递

**原则：插件之间永不直接调用，runtime 是唯一枢纽（hub-and-spokes）**——插件只认识契约接口与 pkg 类型，互相不知道对方存在。可换性由此保证。

**依赖装配（v1 编译期插件）**：
- 类型化构造注入（如 `NewSQLiteSource(dsn)`、`NewOpenAIProvider(cfg)`），Setup 是唯一装配点，按拓扑序 init、逆序 Close；
- **Go 类型系统即依赖校验**——缺依赖的插件无法构造，编译期失败；无 Requires/Provides 元数据仪式；
- 不做字符串键服务注册表；二期跨进程插件才引入能力协商式注册，届时为附加而非替换。

**数据传递三类（「统一」的是契约类型，不是信封格式）**：

| 类别 | 例子 | 机制 |
|---|---|---|
| 契约内数据 | Unit 候选、ToolResult→Message、Episode | 类型化契约（pkg/message、Unit） |
| 大中间产物 | 20k 搜索结果、关键帧，供后续轮/其他工具使用 | **artifact + ref**：产出进外部存储、返回引用、按需取 |
| 身份与控制 | scope、预算、凭据 | ctx（01-pkg） |

明确反对万能信封总线（`map[string]any` / 统一 envelope）。二期跨进程插件的序列化信封是 MCP/gRPC **桥接层**的翻译职责，内部契约类型零改动。

**runtime 服务注入**：插件需要 runtime 能力（后台任务入队、事件订阅）时，以窄接口注入，不暴露 runtime 内部——枢纽单向依赖。

**能力供给：pkg 只放机制，能力走注入**（存储为例）：

- pkg = 编译期 import 的类型与纯函数；有状态、带驱动依赖、有生命周期者（存储、网络客户端）**不进 pkg**；
- 插件角色两分：**基础 = 能力提供者**（被构造注入），**业务 = 操作提供者**（呈现为 Tool/ContextSource 等契约，被编排/被 LLM 调用）。能力接口**定义在消费方**、基础实现隐式满足，双方零 import、只在 Setup 相见；≥2 个消费者才升格共享（三次原则）——如 Setup 开一个 `*sql.DB` 注入两家，换库只改 Setup。标准库接口（`database/sql`、`io/fs`、`slog`）优先，不发明公共 Storage 接口、不做全局单例；
- 存储分账：会话历史 = runtime 事件溯源订阅者（窄 Store 接口，Setup 注入实现）；记忆 = ContextSource 内部自足；大对象 = artifact+ref；凭据存放 = 宿主（05 B3），pkg 只经 ctxx 传播。

## 2. 窗口生成与上下文（意图级）

已定共识只有三句，细节不在此展开：

1. **ContextWindow 是一等可编程状态**：上层可以声明式地变更（注入/置顶/驱逐），变更在轮边界生效；窗口 ≠ 每轮的 transcript。
2. **非线性规划是方向**：窗口内容按重要性竞争选择，而不是对话时间线滑窗（importance ≠ recency）；「选择非线性、呈现线性稳定」。
3. **供给契约是 `ContextSource`**（定义与默认实现见 04 §4）：读路 `Collect`（组装链上同步、快路径，超时降级为本轮无检索）；写路 `Observe`（轮次落盘后异步驱动，幂等）；L1 会话全量原文由事件溯源落盘，不经源契约；namespace 隔离必须做在存储层 WHERE。

窗口 IO 一句话：**输入** = ContextSource 集合 + L1 原文 + 轮间窗口命令（注入/置顶/驱逐，轮边界生效）；**输出** = 每轮 Assembler 链折叠成 `[]Message` 进 core 槽 1 → Provider 请求；窗口自身不产生对外输出，对外一律走事件订阅。

Unit 分级、打分公式、内存层级、pull 工具、可观测与评测——**未定稿**：草稿见 [notes/context-planning-draft.md](notes/context-planning-draft.md)，M3 前重论（05 G2）。

## 4. 预测输入（speculate/）——「两个队列」的最终形态

场景：语音/输入预测用户内容，提前开始生成；猜错则立即打断、用真实输入替换。**定稿结论：core 不参与预测**——预测轮没有工具、没有 Guard、没有 durable 事实，Loop 的价值都不在场，因此 `runtime/speculate` 直接持 Provider 调 `Stream`：

- **单槽**：同时只有一个活动预测（`Input.ID + Revision` 拒绝迟到控制命令）；新 `Speculate` 顶掉旧的 = Replace 语义；
- **确认前无事实**：预测不写 durable 事件、不进 transcript、不执行工具；输出带 tool calls 时剥除（不可复用）；
- **确认后复用 tokens**：`Confirm` 返回预生成的 `{Input, Msg, Usage}`，宿主把 `[..., 确认输入, 预生成答案]` 作为下一次 `Run` 的 input 传入——飞轮把预生成答案当作已有历史继续（利用了 `Run` 无状态、每次传完整历史的既有语义，core 零改动）；
- **猜错**：`Cancel`/新 `Speculate` 直接取消旧流——预测无副作用，取消即回滚，不存在需要恢复的状态。

```
Host(语音识别中) → Speculate(base=transcript快照, 猜测输入)
识别完成, 猜对  → Confirm → Run([...base, 确认输入, 预生成答案])
识别完成, 猜错  → Speculate(rev+1, 真实输入)  // 旧流被取消，立即重跑
用户放弃        → Cancel                      // 无 durable 痕迹
```

实现：`runtime/speculate/speculate.go`（~180 行，零 core 依赖、仅 Provider + pkg）。


## 3. 扩展机制：读与改（hook 语义，无 hook 抽象）

**问题定义**：core 是单线程飞轮，进入 core 的数据运行中不可变。扩展需要读/改/控——都经 core 已有的少数缝 + 事件 + steering 完成，**无 hook 注册抽象，扩展就是 Setup 时的普通 Go 组合**。

**挂点全景**（位置 | 机制 | 语义）：

| # | 挂点 | 机制 | 读/改/控 |
|---|---|---|---|
| 1 | 组装结果 → LLM 请求 | Assembler 链（core 槽 1，`ChainAssembler` 折叠） | 改 |
| 2 | LLM 响应 → 追加历史 | Provider 装饰器（变换 MessageComplete）| 改+读 |
| 3 | tool call → 执行 | Guard 链（core 槽 2，`ChainGuard` 折叠） | 改（拒/放/改写） |
| 4 | 工具结果 → 追加历史 | Tool 装饰器 | 改（脱敏/截断） |
| 5 | 全部工具统一包装 | Registry 全局装饰（`func(Tool) Tool`） | 改 |
| 6 | 运行过程观察 | 事件订阅（durable/volatile 分级） | 读 |
| 7 | 流程干预 | `Loop.Interrupt / Queue` | 控 |
| 8 | runtime 生命周期 | 事件订阅（词汇：session created/resumed/closed、task started/failed、window command applied——待补全） | 读 |

**读到的内容想改？** 不直接改——发窗口命令或 Interrupt（命令闭环）；改窗口必走 Handle（轮边界 + Role 权限），无旁路。

**tool hook 三层分工**（写死，防头疼源头）：
1. **工具内部**——重试/缓存/进度，是实现细节不是 hook；
2. **全局装饰**——registry 统一包装：脱敏、截断、审计、计时；
3. **调用决策**——Guard 链：拒绝/放行/改写参数，含人类审批等待（挂 ctx）。

**红线**：hook 不得持有/修改 core 内部状态（core 只外泄快照）；同步缝内禁止无 ctx 长阻塞；读不反压飞轮；扩展之间不互相调用。

**开放**：挂点 2/4 以装饰器表达还是升格 core 一等槽（B→C 升级不破坏契约，倾向先装饰器）；tool hook 具体场景盘点——见 05 G3。

## 测试与里程碑

所有已定构件保持确定性测试标准（表驱动、无网络无磁盘、`-race` 干净）；里程碑总表见 [README](README.md)。
