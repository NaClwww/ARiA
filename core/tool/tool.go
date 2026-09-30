// Package tool 定义 core 消费的 Tool 契约（docs/02 §5）。
// 实现在 plugins（builtin/MCP 桥），组装在 runtime 的 Setup。
package tool

import (
	"context"
	"encoding/json"

	"aria/pkg/message"
)

type Def struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// Call / Result 直接复用规范类型，core 不另造词汇。
type Call = message.ToolCall
type Result = message.ToolResult

type Tool interface {
	Def() Def
	// Exec 必须尊重 ctx 取消；被取消时返回 IsError 结果（工具侧无
	// Provider 那样的 partial 义务——A3 默认：取消即错误结果喂回模型）。
	Exec(ctx context.Context, call Call) Result
}

// ToolSource 是工具的集合来源（04 §3）；MCP 桥（M4）是其一种实现。
type ToolSource interface {
	Name() string
	Tools() []Tool
	Close(ctx context.Context) error
}

// Stopper 是 Tool 的可选能力：Exec 成功返回后，loop 结束当前 run——同批次
// 剩余调用不再执行（补 stopped 结果，配对完整），也不再发起下一轮 provider
// 调用。用于「模型显式收尾」型工具（如 speak 模式的 stop）：该说的已说完、
// 无事可做，省掉一段必然无人听取的结尾正文。steering 输入（用户插话）优先
// 于收尾——模型转去回答新输入，想停可再调一次。
type Stopper interface {
	Tool
	StopsLoop() bool
}
