# 2026-10-01 框架耦合体检（memory 开工前）

背景：长期记忆设计定稿（同日 [memory-design](2026-10-01-memory-design.md)）后、动工前，全仓扫一遍粘点，判断哪些值得在 P1/P2 前顺手收敛、哪些刻意不动。本文是决策记录，防止同一问题再议。

## 总判断

层间纪律零违规（`go list` 导入图核对：plugins 只引 core+pkg、runtime 只吃 core+pkg、core 不认识上层）。粘度不在层间，集中在五处；本次收敛三处小的，两处大的本来就归 P1/P2。

## 粘点清单（按粘度）

1. **`agent.Session`（runtime/agent/agent.go，553 行）**——全仓最敏感单点：三条订阅角色（窗口消费 / persist Recorder / SetOnCompress 直写）+ settle 编排 + Close 七步收尾时序。memory P2 的唯一高风险编辑点；对策已在 memory 设计定稿：Pump 做成自足的 `runtime/memory` 包，Session 只留构造/挂钩/Close 排空三行接线。
2. **事件词汇三个解释器**——`handleEvent`（进窗口）、`jsonl.marshalData`（落盘 wire）、未来的记忆 Pump 各自解释同一套 durable 事件。→ **本次已收敛**（见下）。
3. **装配重力井**——`ariahost.NewEngine` 是设计上的 Setup（能力在此长肉，正常）；粘的是 `cmd/aria-demo` 为第二个完整 Setup（resolvePersona/buildTools/说话人解析与 ariahost 成对重复，515 行）。本次只收敛说话人一件；demo 全量去重刻意不做（见「刻意不动」）。
4. **[host] 旋钮四处扩散**——每加一个旋钮动四处：flag 定义 / set 判断 / 赋值 / config 字段。→ **本次已表化**（见下）。
5. **说话人 = `[名字] ` 文本前缀**——约定横穿 Sink 标注、stdin 解析（宿主与 demo 各一份，语义还不一致：demo 只给非默认说话人加前缀，宿主所有人统一加）、人设声明、未来的记忆归属。→ **本次已收进 pkg/message**（见下）。

顺带记录两条不动代码的约束：**Store 有两条写入路径**（Recorder 总线订阅 + `SetOnCompress` 直写，靠 jsonl 行锁串行化）——P3 适配器的 run 幂等台账不得进同一 Store，自带小文件；`runtime/speculate` 已实现未接线（既定决策，非粘）。

正面清单（别人的范本，别学坏）：runtime/app 生命周期容器、internal/assemble 两张名字→零件表、core/loop/bus 每订阅者 FIFO、plugins/voice/gowild 内部窄接口缝（InputSink/Gate/audioSink/FollowGate）——复杂度高、耦合度低。

## 本次动了什么

1. **`pkg/message/speaker.go`**：`TagSpeaker` / `SplitSpeaker` 权威实现（前缀规则：名字非空、整个 `[...]` 段 ≤24 字符、空名/超长/未闭合视为正文——宁可漏认不误认）。ariahost 与 aria-demo 的三份副本删除，测试随迁（`speaker_test.go`）。
   **行为变化**：demo 默认说话人现在也带 `[user] ` 前缀进会话（与 host 的 Sink 统一——模型只见一种格式；终端打印仍显示剥离后的文本）。demo 落盘 initial_input 可见该变化。
2. **`core/loop.HistoryMessage(ev)`**：「哪些事件进历史」的唯一权威取数——MessageEnd（assistant）/ ToolExecEnd（工具结果转 tool 消息，含被拒）/ UserMessageInjected（轮间注入）；**AgentStart.InitialInput 明确不是历史**（整包组装结果，防重复提炼的结构前提）。`agent.handleEvent` 改共用它；`events_test.go` 钉住三正三负 + IsError 透传 + 指针形态不认。P2 的 Pump 直接用（memory 设计 §4 已同步注明「不得自带第二份解释」）。
3. **`cmd/aria-host` 的 `hostKnob` 表**：装配项的 flag 声明、帮助文本、写回收进同一条表项（泛型 `hostKnob[T]`），新增 [host] 旋钮 = 表里加一行；「显式给出才覆盖」语义不变（冒烟：`--speak-tool=false` 成功压掉本机 aria.toml 的 `speak_tool=true`）。

验证：`gofmt -l` 空、`go build ./...`、`go vet ./...`、`go test ./...` 全绿；aria-host `--fake --no-asr --no-tts --speak-tool=false` 与 aria-demo `--fake --record` 冒烟通过（含前缀统一与落盘格式核对）。注：`--no-tts` 与本机 `speak_tool=true` 互斥启动报错是既有行为（stash 旧代码复核对齐）。

## 刻意不动（防再议）

- **Session/Pump 接线、Window.Assemble 加 ctx**：分别是 P2/P1 的正题，单独提前无意义。
- **demo 全量去重**（persona/工具装配与 host 共用）：无近期收益，等真正失同步拖累开发再清。
- **Store 双写路**：不动代码，作为 P3 适配器设计约束（台账自带小文件）。
- **[名字] 前缀结构化**（身份进 Scope 字段而非文本）：维持 05 E4 挂起，等 ingress/名册。
