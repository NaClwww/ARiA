# 04 · plugins —— 插件层与依赖

> 实现 core/runtime 暴露的契约；不反向依赖 runtime 内部；装配只发生在库的唯一 Setup 函数（§5）。
> 「插件」= Go 接口 + 编译期注册（代码解耦）；跨语言/运行时热插拔二期经 MCP 或 gRPC 子进程。

## 1. 契约一览（全部只有 5 个接口、10 个方法）

| 契约 | 定义 | 引入 | 实现（本文） |
|---|---|---|---|
| `Provider.Stream(ctx, req)` | core/02 §5 | M1 | §2 各家适配器 |
| `Tool.Def/Exec` | core/02 §5 | M1 | §3 内置工具 |
| `Assembler.Assemble` | core/02 §4 | M1 | 默认恒等透传；窗口组装实现 M3 落地（03 §2、notes 草稿） |
| `ToolSource.Name/Tools/Close` | 本文 §3 | M2 | §3 工具源；MCP 桥（M4）= 一种 ToolSource，实现时再细化 |
| `ContextSource.Name/Collect/Observe` | runtime/03 §2 | M3 | §4 默认记忆源、RAG 源 |

**插件可见的数据类型只有三个**：`Message`（含 Block/ToolCall/ToolResult/Usage）、`Episode`（一轮完整出入）、`Unit`（窗口候选单元：Kind/Content/Importance/Ref/Meta）。Window 内部图结构（版本、COW、访问计数）runtime 私有；原 MemoryItem 降为默认源的内部行结构。

插件间约定（详见 03 §1）：依赖 = 类型化构造注入（**Go 类型系统即校验**——缺依赖无法构造，编译期失败，无元数据仪式）；数据 = 契约类型 / artifact+ref / ctx，**插件之间不直接调用、无万能信封**；扩展行为 = Setup 时普通组合（链帮助函数/装饰器，见 03 §3），无 Hook 注册抽象。

## 2. Provider 适配器

- **openai-compatible**（M1）：一个适配器覆盖 DeepSeek/GLM/Qwen/Ollama 的 OpenAI 端点；规范形互转（pkg/message ↔ OpenAI 格式）；流式 SSE 解析 → PartDelta/MessageComplete。
- **anthropic / gemini**（M1 后补齐）：官方 Go SDK 或自写 HTTP，同样只做哑管道翻译。
- 硬性义务（core/02 §5）：取消时以 `MessageComplete(interrupted=true)` 返回已收内容；Usage 必须上报（Budget 扣减依赖）；限流/超时错误标记 Retryable（错误三分法）。
- 限流包装 `LimitedProvider` 属 runtime（机制草稿 notes/concurrency-draft.md，M2 前重论），适配器本身不管限流。

## 3. 工具：ToolSource 与内置工具

```go
type ToolSource interface {
    Name() string
    Tools(ctx context.Context) ([]Tool, error)  // 可动态刷新（MCP tools/list 会变）
    Close(ctx context.Context) error
}
```

- **builtin 源**（M2）：`web_search`、`web_fetch`、`shell`（超时+输出截断）、`fs_read/write`；**memory 源**（M4）：`memory.recall` / `history.load` / `artifact.open`——pull 路工具，属未定稿草稿（notes/context-planning-draft.md，M4 前定）；
- Registry：接受多个 ToolSource，工具名命名空间化防冲突，支持运行中 refresh（invalidation 后重 list）；
- 工具实现义务：尊重 ctx 取消（长任务返回部分结果）；结果走 content blocks（可带 image 等）；错误返回 `ToolResult{IsError:true}`（是内容不是故障）；经 ToolGuard 审批后执行（core 保证）。

## 4. ContextSource 实现（默认：SQLite 记忆源）

窗口生成契约——一切给 ContextWindow 供给内容的来源（记忆、RAG 语料、用户画像、静态知识包）实现同一个接口：

```go
type ContextSource interface {
    Name() string
    Collect(ctx context.Context, q Query) ([]Unit, error)  // 快路径：组装链上同步，FTS/BM25 毫秒级
    Observe(ctx context.Context, ep Episode) error         // 慢路径：后台 worker 执行，必须幂等
}
```

- **调用方是 runtime**（03 §2）：Collect 在组装链上每轮驱动；Observe 在轮次落盘后异步驱动（重试/幂等/死信机制草稿 notes/concurrency-draft.md，M2 前重论）；
- **默认实现 = SQLite 记忆源**（modernc.org/sqlite，纯 Go）：semantic store + FTS5 检索（namespace 进 WHERE 硬隔离；三期 sqlite-vec 向量 + 混合召回 + rerank，只换内部检索，契约不变）；蒸馏是**源内部管线**（构造注入 Provider，episode → 候选事实 → 去重/supersede → 内部写入），策略走配置——换存储实现 ≠ 换蒸馏；
- **RAG 知识库 = 另一个 ContextSource**：corpus 建索引走 Batch 任务写自己的存储，Collect 只读；
- **L1 会话全量原文不在此契约**：由 runtime 事件溯源落盘（机制草稿 notes/concurrency-draft.md），`history.load` 直读 L1。

## 5. 注册与装配

库暴露**唯一装配函数**（如 `aria.Setup(cfg) (*Runtime, error)`）——唯一知道所有具体插件的地方；当前消费者是嵌入方与集成测试，未来产品入口（CLI/HTTP 薄壳）只是新的嵌入方。core 与 runtime 不 import 任何具体插件包。

```
Setup: ctxx(Scope/Trace/Budget) → Providers(openai…) → ToolSources(builtin, mcp…)
→ Window/Planner → SessionManager + Scheduler + PersistActor
→ Loop Deps{Provider, Tools, Assembler: Planner, Guard} → 嵌入方可用的 Runtime 句柄
```

配置优先级：装配参数 → 环境变量 → 配置文件 → 默认（pi 同款）。

## 6. 依赖清单（go.mod）

| 依赖 | 用途 | 层 |
|---|---|---|
| `golang.org/x/sync`（semaphore, errgroup） | 准入信号量、fan-out | runtime |
| `golang.org/x/time/rate` | RPM/TPM 限流 | runtime |
| `modernc.org/sqlite` | 存储（无 CGO） | plugins/memory |
| `modelcontextprotocol/go-sdk` | MCP 客户端 | plugins/tool/mcp |
| 各 provider 官方 Go SDK（按需）或自写 HTTP | 适配 | plugins/provider |

原则：能标准库不引第三方；新依赖必须落在表内登记（防止依赖蔓延）。Go 1.24+。产品入口暂缓：bubbletea（CLI）、net/http+SSE（服务）不在当前依赖内，入口启用时再登记。

## 7. 二期：跨语言/热插拔插件

gRPC/MCP 子进程插件 = 远端 ToolSource/Recall 实现，接口不变、装配方式不变；沙箱与生命周期管理届时单独设计。
