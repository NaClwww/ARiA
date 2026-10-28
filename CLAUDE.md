# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

ARiA（Autonomous Real-time Interactive Assistant）：可嵌入的 Go 实时交互 LLM 助手引擎（非 coding agent，重点是记忆管理与上下文管理）。代码注释、文档、提交信息都用中文，新写的内容保持一致。

## 常用命令

```sh
go build ./... && go vet ./...
go test -race ./...                                  # 全量；设计要求 -race 干净
go test -race ./runtime/window -run TestXxx -v       # 单个测试
go run ./cmd/aria-demo --fake                        # 终端薄壳，无网络冒烟（内置演示 provider）
go run ./cmd/aria-host --fake --no-asr --no-tts      # Gowild 宿主，纯 stdin 冒烟
go run ./cmd/aria-demo --print-config                # 打印生效配置（含覆盖来源）
./start.sh [flags]                                   # 真机启动 aria-host：加载 .env、adb forward 音箱端口
```

- 测试全部确定性：`core/provider.FakeProvider` 表驱动，无网络、无磁盘。provider 的 live 测试仅在设置 `DEEPSEEK_API_KEY` / `OPENAI_API_KEY` / `OPENCODE_API_KEY` 时运行，否则跳过。
- 没有 Makefile 与 lint 配置；`go vet` 即可。

## 分层与依赖规则

```
pkg ← core ← runtime ← plugins / internal / cmd
```

| 目录 | 职责 | 依赖规则 |
|---|---|---|
| `pkg/`（ctxx、message、modelcard） | ctx 类型化存取（Scope/Trace/Credentials/Budget/Options）、规范多模态消息模型、说话人前缀、模型元数据卡（models.dev） | 零依赖（只用标准库） |
| `core/`（loop、provider、tool） | agent-loop 单线程飞轮、事件总线、Provider/Tool 契约 | 只依赖 pkg |
| `runtime/` | context-build：`agent`（Session/会话切换/记忆调用）、`window`（每轮组装 + 压缩）、`persist`（durable 事件溯源）、`memory`（记忆服务接口与 memory_recall 工具）、`app`（宿主外设挂载容器）等 | 依赖 core+pkg；**不 import 任何具体插件包** |
| `plugins/` | 契约的具体实现：provider（deepseek/openai/opencode）、tool/basic、persist/jsonl、memory/hindsight、voice/vision（gowild 音箱） | 实现 core/runtime 契约，不反向依赖 |
| `internal/config` | 分层配置（viper） | — |
| `internal/assemble` | **配置 → 实现的唯一装配点**（Provider/Compressor/Memory/Session 参数），两个宿主共用 | 唯一允许同时认识 config 与具体插件的地方 |
| `internal/aria-host` | aria-host 的组件（引擎组装、渲染、stdin 插头、人设） | — |
| `cmd/` | 宿主薄壳：`aria-demo`（终端）、`aria-host`（Gowild 语音音箱场景）；`tools/gateway-smoke` 是 ASR 网关冒烟工具 | main 只是挂载清单 |

新增一种 provider/记忆后端/压缩策略 = 在 plugins 加实现 + 在 `internal/assemble` 对应装配函数加一个分支，core/runtime 不动。

## 关键架构约束（读多个文件才看得出的）

- **core 飞轮单线程**：一次 Run 的全部可变状态归一个 goroutine、零锁、零磁盘 I/O。入口只有 Run/Queue/Interrupt/Subscribe/Reconfigure（配置每轮开头现取，Reconfigure 下一轮生效，参照 pi）；core 只有两个钩子槽 Assembler（窗口组装/记忆注入唯一入口）与 ToolGuard。扩展一律是普通 Go 组合（链/装饰器），不引入 Hook 抽象。
- **事件分级**：durable（完整、可重放）与 volatile（可合并丢）。渲染/TTS/落盘/灯都是事件订阅者；事件不重放，所以宿主里**事件消费者先挂、输入插头最后挂，收尾严格逆序**（`runtime/app` 存在的理由，见其包注释）。
- **宿主拥有全部 IO**：引擎唯一输入口是 `Session.Input`（阻塞到本轮结算）；运行中插话用 `Queue`，空闲时返回 `ErrNoActiveRun`，宿主应退回 `Input`。一 Session 同时只跑一轮。
- **输入必须带主人**：说话人既进消息层（`[名字]` 前缀进历史）又进 Scope（`ctx.UserID`）；Scope 缺失 fail-closed。
- **组装层 = 骨架 + 可换零件**（`runtime/window`）：顺序与「压缩中不丢内容 / 失败退回原文」是骨架；压缩策略（provider / keeplast）是零件，经 `Session.SetCompressor` 热切换。压缩按上下文用量触发（`[compress]` 段）。
- **会话切换**：`idle_timeout_s` 无输入后清空窗口，新会话不带入上一会话的摘要与原文；会话 ID = `<id>-<开始时刻>`。
- **长期记忆**：`runtime/memory.Service` 接口（Stage/End/Start/Recall），会话首轮最多等 `recall_wait_ms` 召回；`[memory] engine=""` 表示完全不接入。
- **错误三分法**：ToolError 是内容（喂回模型）、ProviderError 退避重试、Fatal 终止。

## 配置

- 优先级：启动 flag（仅本进程）> `aria.override.toml`（机器写，gitignored）> `aria.toml`（人写，**程序永不重写**，注释安全）> 代码默认值。
- 写回只碰 override 层且原子写。热更新三件事解耦：`config.Watch`（只报「文件可能变了」）→ `Manager.Reload()`（比对内容、语法错误保留旧值）→ `OnChange` 上的 `assemble.Live.Apply`（唯一应用点：用宿主的 `build(cfg)`——与启动同一份装配函数——整份重新装配，交给 `Session.Reconfigure`；会话、上下文窗口、事件订阅不动）。Provider 由 `assemble.ProviderCache` 按 `[provider]` 段缓存。记忆服务、落盘、`[host]` 只在启动时生效（`restartOnly` 记「需重启」）。新增配置项只要在宿主的 `build` 里读它，就自动可热改。
- API key 永不落盘：配置只存环境变量名（`provider.api_key_env`，默认 `ARIA_API_KEY`），`start.sh` 会自动加载 `.env`。
- aria-host 的装配项基准在 `aria.toml [host]`；新增 [host] 旋钮 = 在 `cmd/aria-host/main.go` 的 hostKnob 表里加一行。

## 文档

`docs/` 是设计的权威来源，改动架构前先读：`docs/README.md`（总览、关键决策、里程碑）→ `01-pkg` / `02-agent-core` / `03-context-build` / `04-plugins` / `06-host-shell`；`05-open-questions.md` 是未决清单；`docs/memory/` 是长期记忆设计。约定：
- 讨论过程写 `docs/discussions/YYYY-MM-DD-主题.md`，定稿后才合入正式文档并在记录里标注「已合入 → 位置」；`docs/notes/` 是草稿。
- 代码行为变更时同步更新对应文档（历史提交通常是「实现」+「docs：同步…」成对出现）。
