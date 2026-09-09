# 01 · pkg —— 基础件层

零依赖的公共基础件，供所有上层使用。只放「与 agent 语义无关、但层层都要用」的东西。

## 1. ctxx —— ctx 传播基础件

全项目唯一合法的 ctx 值存取点（私有 key 结构体，禁止裸 `context.WithValue` 字符串键）。

### 1.1 传播内容

| 字段 | 类型 | 首次写入 | 消费方 |
|---|---|---|---|
| Scope | `Scope{UserID, SessionID, AgentID, Namespace}` | 触发层 | 记忆隔离（SQL WHERE 条件）、日志、计费聚合 |
| Trace | `Trace{TraceID, SpanID}` | 触发层（缺失自动生成） | 全链路日志 |
| Credentials | 按 provider 的 key / MCP token | 业务层 | Provider 路由、MCP 桥透传 |
| Budget | token/费用上限 + 原子扣减 | 业务层 | core 预算检查点 |
| Options | model 覆盖、temperature 等 | 业务层 | core → Provider |

### 1.2 生命周期（与各层的契约，此处仅声明规则）

- run ctx 的父级是 session 的长活上下文（由 runtime 持有，机制 M2 前定稿），**不是触发请求**——视频/语音长会话的前提；
- 派生只允许收窄（cancel/timeout/deadline）；
- 分离任务（异步落盘等）用 `context.WithoutCancel` 保留值、脱离取消，且必须记日志。

### 1.3 不变量（review checklist）

- **R1** 所有插件/基础件接口第一参数 `context.Context`；
- **R2** 派生不改写 Scope；
- **R3** 消息体、工具结果永远不走 ctx（数据向上走事件）；
- **R4** 值只经 ctxx 类型化存取；**Scope 缺失 → 拒绝执行（fail-closed）**；
- **R5** 不得用 `context.Background()` 吞取消。

### 1.4 配套

slog 自定义 Handler：从 ctx 提取 Trace/Scope，每行日志自动带 trace_id/session_id。Budget 附原子扣减接口（全项目唯一允许的原子操作之一）。

## 2. message —— 规范消息模型

Provider 无关的规范形（canonical form），第一天就是**多模态 content blocks**（为视频帧/语音预留，适配成本最低的决策点）：

```go
type Role string

const (
    RoleSystem    Role = "system"
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
)

type Message struct {
    ID          string
    Role        Role
    Blocks      []Block     // Text / Image / Audio / File / Thought
    ToolCalls   []ToolCall  // 仅 assistant：本轮发起的工具调用
    ToolCallID  string      // 仅 tool：本条结果响应哪个 ToolCall.ID
    Interrupted bool        // steering 打断时保留的部分输出标记
}

type Block interface{ isBlock() }        // Text / Image / Audio / File / Thought
type ToolCall struct{ ID, Name string; Args json.RawMessage }
type ToolResult struct{ CallID string; Blocks []Block; IsError bool }
type Usage struct{ In, Out, Cached int; Cost float64 }
```

历史形态唯一约定（canonical transcript）：

- assistant 发起的调用挂在自身 `ToolCalls` 字段上，重放给 provider 时原样携带——缺了它多轮工具循环在真实 provider 上直接 400；
- 工具结果进入历史只有一条规则：`Message{Role: tool, ToolCallID: result.CallID, Blocks: result.Blocks}`（即 `ToolResult.ToMessage()`）。

一次工具往返的完整历史示例：

```go
Message{Role: system,    Blocks: [Text{"You are ARiA..."}]}
Message{Role: user,      Blocks: [Text{"查今天天气"}]}
Message{Role: assistant, Blocks: [Thought{...}], ToolCalls: [{ID: "call_1", Name: "get_weather", ...}]}
Message{Role: tool,      ToolCallID: "call_1", Blocks: [Text{`{"temp":22}`}]}
Message{Role: assistant, Blocks: [Text{"今天 22 度，晴"}]}
```

各 provider 适配器做 规范形 ↔ 厂商格式 互转（pi 的 convertToLlm 模式）。ToolResult 双标志（IsError + 内容块）支撑错误三分法：工具失败是喂回模型的内容，不是系统故障。`ThoughtBlock` 为 GLM/R1/o 系 reasoning 预留（原 05 A1）；默认不入窗，入窗策略随 G2（[notes/context-planning-draft.md](notes/context-planning-draft.md)）重论定稿。

## 3. 测试要求

与其他层同标准：表驱动、无网络无磁盘、`-race` 干净；Budget 并发扣减有专门竞争测试。
