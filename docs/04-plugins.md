# 04 · plugins —— 插件层与依赖

> 实现 core/runtime 暴露的契约；不反向依赖 runtime 内部；装配只发生在库的唯一 Setup 函数（§5）。
> 「插件」= Go 接口 + 编译期注册（代码解耦）；跨语言/运行时热插拔二期经 MCP 或 gRPC 子进程。

## 1. 契约一览（全部只有 5 个接口、12 个方法）

| 契约 | 定义 | 引入 | 实现（本文） |
|---|---|---|---|
| `Provider.Stream(ctx, req)` / `Limits(model)` / `CountTokens(model, msgs)` | core/02 §5 | M1（Limits / CountTokens 2026-10-01） | §2 各家适配器 |
| `Tool.Def/Exec` | core/02 §5 | M1 | §3 内置工具 |
| `Assembler.Assemble` | core/02 §4 | M1 | 恒等兜底；runtime/window 每轮组装（03 §5）在 core 槽 1 上折叠 |
| `ToolSource.Name/Tools/Close` | 本文 §3 | M2 | §3 工具源；MCP 桥（M4）= 一种 ToolSource，实现时再细化 |
| `ContextSource.Name/Collect/Observe` | runtime/03 §2 | M3 | §4 RAG 源 |

**插件可见的数据类型只有三个**：`Message`（含 Block/ToolCall/ToolResult/Usage）、`Episode`（一轮完整出入）、`Unit`（窗口候选单元：Kind/Content/Importance/Ref/Meta）。Window 内部图结构（版本、COW、访问计数）runtime 私有。

插件间约定（详见 03 §1）：依赖 = 类型化构造注入（**Go 类型系统即校验**——缺依赖无法构造，编译期失败，无元数据仪式）；数据 = 契约类型 / artifact+ref / ctx，**插件之间不直接调用、无万能信封**；扩展行为 = Setup 时普通组合（链帮助函数/装饰器，见 03 §3），无 Hook 注册抽象。

## 2. Provider 适配器

- **openai-compatible**（M1）：一个适配器覆盖 DeepSeek/GLM/Qwen/Ollama 的 OpenAI 端点；规范形互转（pkg/message ↔ OpenAI 格式）；流式 SSE 解析 → PartDelta/MessageComplete。
- **anthropic / gemini**（M1 后补齐）：官方 Go SDK 或自写 HTTP，同样只做哑管道翻译。
- 硬性义务（core/02 §5）：取消时以 `MessageComplete(interrupted=true)` 返回已收内容；Usage 必须上报（Budget 扣减依赖）；限流/超时错误标记 Retryable（错误三分法）。
- 限流包装 `LimitedProvider` 属 runtime（并发调度草稿 notes/concurrency-draft.md，有需要再定），适配器本身不管限流。

## 3. 工具：ToolSource 与内置工具

```go
type ToolSource interface {
    Name() string
    Tools(ctx context.Context) ([]Tool, error)  // 可动态刷新（MCP tools/list 会变）
    Close(ctx context.Context) error
}
```

- **builtin 源**（M2）：`web_search`、`web_fetch`、`shell`（超时+输出截断）、`fs_read/write`。`artifact_open` 已由 runtime 提供（03 §5 工具结果截断）。
- Registry：接受多个 ToolSource，工具名命名空间化防冲突，支持运行中 refresh（invalidation 后重 list）；
- 工具实现义务：尊重 ctx 取消（长任务返回部分结果）；结果走 content blocks（可带 image 等）；错误返回 `ToolResult{IsError:true}`（是内容不是故障）；经 ToolGuard 审批后执行（core 保证）。
- **超长结果由 runtime 统一处理**：Agent 装配时对工具套上 `toolkit.Truncate`（03 §5），全文进 artifact、只把预览+引用喂回模型——插件只需如实返回结果，不必自己截断（`shell` 之类的内部截断仍可保留，属工具自身语义）。

## 4. ContextSource 实现

窗口生成契约——一切给 ContextWindow 供给内容的来源（记忆、RAG 语料、用户画像、静态知识包）实现同一个接口：

```go
type ContextSource interface {
    Name() string
    Collect(ctx context.Context, q Query) ([]Unit, error)  // 快路径：组装链上同步，FTS/BM25 毫秒级
    Observe(ctx context.Context, ep Episode) error         // 慢路径：后台 worker 执行，必须幂等
}
```

- **调用方是 runtime**（03 §5）：Collect 在每轮组装链上同步驱动；Observe 在该轮事件写入磁盘后异步驱动（幂等；重试与死信细节随并发调度草稿确定）；
- 写路修订中（2026-10-02）：Observe 按轮驱动改为按压缩批次暂存、会话结束提交；记忆服务使用独立接口 Stage / End / Start / Recall，ContextSource 的写路随之修订，见 [memory/options.md](memory/options.md)「记忆服务接口」；
- **RAG 知识库 = 另一个 ContextSource**：corpus 建索引走 Batch 任务写自己的存储，Collect 只读；
- **L1 会话全量原文不在此契约**：由 runtime 事件溯源落盘（runtime/persist，v1 只写不恢复）；`history.load` 是 M4 pull 工具，直读 L1。

## 5. 注册与装配

库暴露**唯一装配函数**（如 `aria.Setup(cfg) (*Agent, error)`，03 §5）——唯一知道所有具体插件的地方；消费者是嵌入方与集成测试，未来产品入口（CLI/HTTP 薄壳）只是新的嵌入方。core 与 runtime 不 import 任何具体插件包。

```
Setup: ctxx(Scope/Trace/Budget) → Providers(openai…) → ToolSources(builtin, mcp…)
→ ContextSources(memory…) + Store(persist 实现)
→ Agent{Provider, Tools, Compressor, Store} = 装配零件盒（03 §5）
→ NewSession → Session{压缩记忆(Window) + Loop + persist 订阅}
→ 每轮：window.Assemble → Run → AgentEnd 结算进窗口
```

配置优先级：装配参数 → 环境变量 → 配置文件 → 默认（pi 同款）。

## 6. 依赖清单（go.mod）

| 依赖 | 用途 | 层 |
|---|---|---|
| `golang.org/x/sync`（semaphore, errgroup） | 准入信号量、fan-out | runtime |
| `golang.org/x/time/rate` | RPM/TPM 限流 | runtime |
| `modelcontextprotocol/go-sdk` | MCP 客户端 | plugins/tool/mcp |
| 各 provider 官方 Go SDK（按需）或自写 HTTP | 适配 | plugins/provider |

原则：能标准库不引第三方；新依赖必须落在表内登记（防止依赖蔓延）。Go 1.24+。产品入口暂缓：bubbletea（CLI）、net/http+SSE（服务）不在当前依赖内，入口启用时再登记。

## 7. 二期：跨语言/热插拔插件

gRPC/MCP 子进程插件 = 远端 ToolSource/Recall 实现，接口不变、装配方式不变；沙箱与生命周期管理届时单独设计。
