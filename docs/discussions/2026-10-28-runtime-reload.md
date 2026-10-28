# 2026-10-28 · runtime 重载：插件热替换怎么做

**背景**：配置热更新（文件监听 → `Manager.Reload` → `assemble.Live`）落地后，第一版只热换了 Provider（`provider.Swappable` 包一层）、压缩器和几个会话计时；工具表、`max_turns`、`tool_timeout_ms`、人设、记忆服务、落盘都要重启。问题：runtime 层的插件怎么热替换？

## 备选方案

| 方案 | 做法 | 否决 / 采用理由 |
|---|---|---|
| A · 逐项 setter | 每个插件加一个 Set 方法，`Live` 逐项比对、逐项调用 | 否决：每加一种可热改的配置都要改 runtime + `Live` 两处；工具表、轮数在 core loop 构造时定死，仍要动 core |
| B · 整体重建 Agent + Session，迁移窗口 | 重载 ≈ 重启，再把上下文窗口导入新 Session | 否决：事件总线在 loop 里，宿主的渲染 / TTS / 灯 / 落盘订阅全断、切换瞬间的事件丢失；宿主各处持有的 `*Session` 指针失效；窗口没有导入接口，且除消息外还有在途压缩、召回、要点提取进度、轮数与计时；记忆服务会重新 Start |
| C · Session 外壳不动，轮间重建 loop | 总线移出 loop 由 Session 持有，两轮之间按新配置新建 loop | 可行，但要迁移残留插话队列、处理 Interrupt 与替换 loop 的竞态 |
| **D · 参照 pi：配置每轮现取 + 宿主装配函数复用（采用）** | 见下 | 改动最小、没有迁移问题 |

## pi 的做法（pi-mono `44f6851`，2026-10-28）

- **agent 包**：`Agent` 持有一份可变状态（`model` / `thinkingLevel` / `tools` / `messages`）；loop 是无状态函数 `runAgentLoop(context, config, emit, signal)`，每次运行由 `createContextSnapshot()` + `createLoopConfig()` 从状态现取。换模型 / 工具 = 给 `agent.state.*` 赋值，下一次运行生效；订阅挂在 `Agent.subscribe` 上，不受影响。运行中还可经 `prepareRequest` / `prepareNextTurn` 在下一次请求前替换 model / context。
- **coding-agent 的 `/reload`**（`AgentSession.reload()`）：流式生成或压缩中拒绝；给旧扩展运行时发 `session_shutdown(reason: "reload")` 并作废 → 重读 settings、重置 API provider、重新加载扩展 / 技能 / 提示词 → `_buildRuntime` 新建扩展运行时、重建工具注册表与 system prompt、写回 `agent.state.tools` → 发 `session_start(reason: "reload")`。Agent 与消息不动；已启用的工具与 CLI flag 值带到新运行时。
- **换模型**（`setModel`）：赋值 `agent.state.model`，并往会话日志写一条 `model_change`。

## 采用的设计（D）

1. **core**：`Loop` 新增第五个入口 `Reconfigure(cfg, note)`——校验后放进「待生效」位；`Run` 在**每一轮开头**取最新配置作为本轮快照（轮内不变），有新配置时发 durable `reconfigured` 事件（对应 pi 的 `model_change`）。空闲时 = 下一次 Run 生效；运行中 = 下一次 LLM 请求前生效（对应 pi 的 `prepareNextTurn`）。`Loop` 仍是长寿命对象，持有总线、队列、打断位——这是与 pi 的差别，也是订阅者不受影响的原因。破掉的规则：02 §1「四个入口」「配置构造时定死」。
2. **runtime**：`Session.Reconfigure(agent.Config, note)`——用新配置建零件盒，loop 侧交给 `Loop.Reconfigure`，窗口侧（人设、压缩器、近轮数）与会话侧（压缩预留、闲置时长、召回等待、Provider 限额来源）直接替换。artifact 存放处沿用，已发出的引用继续可读。Memory / Store / 会话标识生成忽略（见下）。
3. **宿主**：把启动时的装配写成一个 `build(cfg) → agent.Config` 函数（对应 pi 的 `_buildRuntime`），启动和 `assemble.Live` 共用；Provider 由 `assemble.ProviderCache` 按 `[provider]` 段缓存。`provider.Swappable` 与 `Live` 里的逐项 setter 随之删除。效果：**在 `build` 里读到的配置都能热改**，新增配置项不用再写热更新代码。

已验证：`--fake` 运行中改工具表 / 人设 / `max_turns`，下一轮生效且落盘有 `reconfigured`；opencode-go 真实服务运行中改 `[provider]`，Provider 重建后模型仍记得上一轮的内容。

## 待定决策清单

- [ ] 记忆服务与落盘的热替换（05 F6）：候选是 `Reconfigure` 暂存、会话切换时替换（参照 pi 的 shutdown / start 事件对）。
- [ ] 是否像 pi 那样在运行中 / 压缩中**拒绝**重载：当前选择是接受并在下一轮生效（不拒绝），在途压缩用旧策略跑完。若真实使用中发现「下一次 LLM 请求前就换」造成困扰，改为只在 Run 之间生效只需改 `refreshConfig` 的调用点。

已合入 → 02 §1 / §3.1、06 §3、05 F6、README 关键决策 2 与 11。
