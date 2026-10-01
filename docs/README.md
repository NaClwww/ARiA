# ARiA 设计文档

可嵌入的 Go **实时交互 LLM 助手引擎**（2026-09-09 方向重排：非 coding agent，聚焦记忆管理与上下文管理）：核心以**可嵌入引擎库**交付，由宿主程序驱动——宿主拥有全部 IO 通道（文本/语音/视频），引擎对通道类型零感知；产品入口（CLI/HTTP）**暂缓**，将来加入时只是事件流之上的薄壳。参考系 pi（badlogic）的极简 agent loop，强化 ctx 传播、插件化与上下文组装。

## 架构总览

```
              ┌─────────────── 宿主（引擎的嵌入方）───────────────┐
  触发源：文本 / 语音 / 视频 / 定时 → 制造 Scope、Run/Queue/Interrupt │
  输出：渲染 / TTS / 落盘 ← Subscribe 事件流（durable+volatile）   │
              └──────────────────┬───────────────────────────────┘
                                 │
┌────────────────────────────────▼───────────────────────────────┐
│ runtime（context-build）                                       │
│   Agent/Session：装配零件盒 + 长寿命单 Session（不分会话）     │
│   窗口组装：每轮 = 压缩记忆 + 当前感知 + 新输入（间隙异步压缩）│
│   ingress：成轮状态机（分段→成轮→归主→判终）                   │
│   事件溯源持久化：durable 订阅 → 窄 Store（v1 只写不恢复）     │
│   小轮垫话：loop 外轻量调用（无工具/无 durable；投机暂缓）     │
└───┬────────────────────────────────────────────▲───────────────┘
    │ 依赖                                       │ 只经契约消费
    ▼                                            │（不 import 实现）
┌──────────────────────────┐             ┌───────┴───────────────┐
│core（agent-loop）        │◀────────────│plugins                │
│单线程飞轮                │             │Provider 适配          │
│Run/Queue/Interrupt/      │             │Tool / ToolSource      │
│  Subscribe 四入口        │             │  （含 MCP 桥，M4）    │
│槽1 Assembler/槽2 Guard   │             │ContextSource          │
│事件总线 durable/volatile │             │（记忆/RAG/画像/知识） │
└───────────┬──────────────┘             └──────────┬────────────┘
             └──────────────────┬───────────────────┘
                                ▼
             ┌─────────────────────────────────────┐
             │pkg：ctxx · message 规范模型 · slog  │
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
基础件        单线程飞轮                  Agent/Session·组装·ingress      实现：provider/tool/memory/MCP
                                         ·持久化·小轮垫话·扩展
```

| 层 | 文档 | 职责一句话 | 依赖规则 |
|---|---|---|---|
| pkg | [01-pkg.md](01-pkg.md) | ctx 传播基础件 + 规范消息模型 | 零依赖 |
| agent-core | [02-agent-core.md](02-agent-core.md) | agent loop 飞轮、事件总线、steering、core 接口 | 只依赖 pkg |
| context-build（runtime） | [03-context-build.md](03-context-build.md) | 插件中介、窗口组装（每轮压缩+感知叠加）、运行时结构（Agent/Session）、ingress 成轮、小轮垫话、扩展机制；投机暂缓（03 §4）、非线性规划在 notes/ 草稿 | 依赖 core+pkg；只经契约消费插件 |
| plugins | [04-plugins.md](04-plugins.md) | provider 适配器、工具源、MCP 桥、记忆实现、依赖清单 | 实现 core/runtime 契约，不反向依赖 |
| 宿主薄壳 | [06-host-shell.md](06-host-shell.md) | 薄壳三职责、输入插头契约、配置分层与操作面板（aria-web） | 消费 runtime/core 公开 API，不反向依赖 |
| 未决 | [05-open-questions.md](05-open-questions.md) | 尚未讨论完的问题清单（收敛中） | —— |

目录名映射：`pkg/`、`core/`（= agent-loop）、`runtime/`（= context-build）、`plugins/`、`cmd/`（薄壳）、`internal/`（宿主侧共享件，如 config）。装配点 = 库暴露的唯一 Setup 装配函数（当前消费者是嵌入方/集成测试/薄壳）——core 与 runtime 不 import 任何具体插件包。

## 关键决策速览

1. **ctx 是脊柱**：Scope/Trace/Credentials/Budget/Options 经 `pkg/ctxx` 类型化存取，业务层贯穿到底层；run ctx 父级是 session 而非触发请求；Scope 缺失 fail-closed。
2. **core = 单线程飞轮**：一次 Run 的全部可变状态归一个 goroutine，无锁；入口只有 Run/Queue/Interrupt/Subscribe；loop 零磁盘 I/O。
3. **事件分级**：durable（载荷完整、存活订阅者内不丢、可重放重建会话）与 volatile（可合并丢）；一切 UI/服务/持久化都是事件消费者。
4. **core 只有两个钩子槽**：Assembler（窗口组装/记忆注入唯一入口）+ ToolGuard（审批）；其余一切在 runtime 层组合。
5. **ContextWindow 是一等可编程状态，非线性规划是方向**：选择非线性、呈现线性稳定（importance ≠ recency）。v1 窗口 = **每轮组装**（压缩记忆 + 当前感知 + 新输入，间隙异步压缩，03 §5）；非线性 Planner 细节未定稿——草稿 notes/context-planning-draft.md，M3 前重论（05 G2）。
6. **MCP 经 mcp-to-tool 桥**变成原生 Tool；插件 v1 = 编译期接口注册，跨语言二期 MCP/gRPC 子进程。
7. **错误三分法**：ToolError 是内容（喂回模型）、ProviderError 退避重试、Fatal 终止。
8. **插件契约极简（5 接口 10 方法）**：Provider / Tool / Assembler / ToolSource / **ContextSource**（Name/Collect/Observe——窗口生成契约：记忆、RAG、画像、静态知识都是给窗口供给 Unit 的源；Collect 快路径同步、Observe 慢路径异步幂等；蒸馏是源内部管线而非契约）。依赖 = 类型化构造注入，**Go 类型系统即校验**；数据 = 契约类型 / artifact+ref / ctx，插件间不直接调用、无万能信封。
9. **扩展 = 普通 Go 组合，无 Hook 抽象（03 §3）**：core 运行中不可变——读 = 事件订阅，改 = 挂点上的链/装饰器（ChainAssembler/ChainGuard + Tool/Provider 装饰 + 窗口命令），控制 = Interrupt/Queue；tool hook 三层分工：工具内部 / 全局装饰 / Guard 决策；core 只见折叠后的单个实现。
10. **一切构件确定性可测**：FakeProvider 表驱动，无网络无磁盘，`-race` 干净。
11. **宿主薄壳与配置分层（2026-09-14）**：薄壳只做三件事——制造 Scope、订阅事件流、**输入插头**（把各路输入加工成「一句完整的话 + 谁说的」）。配置**启动读一次、运行期只写回**：web 是操作面（内存即真相）、文件是持久化载体；生效优先级 = 启动 flag（进程内）> override 文件（网页保存目标，机器写）> base 文件（人写，程序**永不重写**、注释安全）> 默认值。写回只碰 override 层（viper 写回必丢注释），原子写；外部手改走 `Reload()`/重启。详见 [06](06-host-shell.md)。
12. **组装层 = 骨架 + 可换零件（2026-09-14）**：`runtime/window` 的身份是**组装层**（每轮该给模型看什么由它拼），压缩只是它的一个零件位。排列顺序与「压缩中不丢内容 / 失败退回原文」是骨架；压缩策略是**可点菜的零件**（已实现：`internal/assemble` 装配表 + `Session.SetCompressor` 热切换；将来两段式 compact 就是表里加一行）。详见 [03](03-context-build.md) §5。

**当前实现全景（2026-09-14）**：

```
输入插头（可换、可并存）
    │  手打（回车=说完）· ASR（静音=说完）· 网页（发送=说完）· 定时
    │  交付：一句完整的话 + 谁说的
    ▼
Session.Input ─▶ 组装层 window ─▶ core 飞轮 ─▶ 事件流 ─▶ 渲染 / TTS / 落盘
                  ├ 骨架：顺序 / 压缩中不丢内容 / 失败退回原文
                  └ 零件位：压缩策略（配置点菜，可热换）
```

已实现：pkg + core（M1）、runtime 主线三包 + artifact/toolkit（M2）、JSONL 落盘、终端薄壳 `cmd/aria-demo`、配置分层 `internal/config`。
未实现：`cmd/aria-web` 面板、压缩零件注册表与热切换、ASR 插头、认主（插件层协商）、打断/小轮（03 §7）、长期记忆源（设计定稿 2026-10-01，见 [discussions/2026-10-01-memory-design.md](discussions/2026-10-01-memory-design.md)）。

**runtime 并发（2026-09-11 定稿）**：v1 一 Session 同时只跑一轮，并发新输入由会话互斥串行、轮间插话走 `Queue`（仅运行中接受，2026-09-28）——**不建 schedule/ 子包，M2 不被调度设计阻塞**。notes/concurrency-draft.md 的完整草案（actor 单写者 / 多维准入 / 后台任务 / 限流）仍是草稿，原则方向大概率保留，有真实并发场景再回来定稿（05 G1）。

## 里程碑

| 阶段 | 内容 |
|---|---|
| M1 | pkg + core：飞轮跑通多轮工具调用（OpenAI 兼容 provider，集成测试验收） |
| M2 | runtime 主线三包（agent/window/persist）+ 内置工具源 + 嵌入装配 API（ingress 随语音 M5、speculate 暂不接入） |
| M2+ | 宿主薄壳与配置：`cmd/aria-demo`（已跑通）、`internal/config` 分层同步（已落地）、`cmd/aria-web` 面板（待做）；进度见 [06](06-host-shell.md) §6 |
| M3 | ContextSource 默认记忆源 + 非线性窗口 Planner（细节届时已重论定稿） |
| M4 | MCP 桥 |
| M5 | 语音/视频触发源（ingress 成轮状态机 + VAD/ASR/人脸插件化接入；认主实现由插件层协商） |

产品入口（CLI/HTTP）不与里程碑绑定，任何时候可作为薄壳加入（事件流的订阅者 + Scope 制造者）。

## 跑起来（M2 demo 薄壳）

`cmd/aria-demo` 是第一个宿主薄壳（2026-09-14）：**配置文件驱动**（flag 只作进程内覆盖），stdin 一行 = 一句已成轮的话语（模拟 ASR 文字流输出侧），Input 调用点即未来 ingress（03 §6）与 ASR 插头（06 §2）的接入缝；回答流式打印、工具轨迹进 stderr、durable 事件可选落 JSONL。

```sh
go run ./cmd/aria-demo --fake                  # 无网络冒烟（内置演示 provider + now/lorem 工具）
go run ./cmd/aria-demo                         # 按 aria.toml 连真实服务（key 走配置里的 api_key_env）
go run ./cmd/aria-demo --print-config          # 看生效配置（含「哪项被覆盖、基准值是多少」）
```

交互：`[名字] 开头` 切换说话人（多人共享一个 Session）；`/reload` 重读配置文件；Ctrl+C 打断当前回答（steering）；`/quit` 退出。落盘实现 = `plugins/persist/jsonl`（格式带版本号，读路/重放随恢复需求再做）。

配置见仓库根 [`aria.toml`](../aria.toml)（带注释样例）：生效优先级 = 启动 flag > `aria.override.toml`（网页保存目标）> `aria.toml`（人写，程序永不重写）> 默认值；分层理由与热调边界见 [06 §3](06-host-shell.md)。

## 未决问题

**讨论尚未收敛**——未决问题全集见 [05-open-questions.md](05-open-questions.md)，按里程碑标注优先级；每定案一项即回写对应层文档并从清单删去，该文件趋空 = 设计完成。
