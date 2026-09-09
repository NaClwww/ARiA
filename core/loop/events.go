package loop

import (
	"time"

	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// Kind 是事件词汇（docs/02 §3.1，A2 定稿版）。
// durable：载荷完整、存活订阅者内不丢，runtime 靠它 1:1 重建会话；
// volatile：仅渲染用，缓冲满即丢（MessageEnd 永远带全文，丢增量无损）。
type Kind string

const (
	KindAgentStart          Kind = "agent_start"
	KindUserMessageInjected Kind = "user_message_injected"
	KindTurnStart           Kind = "turn_start"
	KindTurnEnd             Kind = "turn_end"
	KindMessageStart        Kind = "message_start"
	KindMessageUpdate       Kind = "message_update" // volatile
	KindMessageEnd          Kind = "message_end"
	KindToolGuardDecision   Kind = "tool_guard_decision"
	KindToolExecStart       Kind = "tool_exec_start"
	KindToolExecEnd         Kind = "tool_exec_end"
	KindProgress            Kind = "progress" // volatile；M4 MCP progress 映射
	KindAgentEnd            Kind = "agent_end"
)

// Durable 报告事件级别。两个 volatile 词汇之外全部 durable。
func (k Kind) Durable() bool {
	return k != KindMessageUpdate && k != KindProgress
}

type Event struct {
	Kind  Kind
	RunID string
	Turn  int
	At    time.Time
	Data  any // 按 Kind 断言为下列 *Data 类型
}

type AgentStartData struct {
	Scope        ctxx.Scope
	InitialInput []message.Message
}

type UserMessageInjectedData struct{ Message message.Message }

type TurnStartData struct {
	Turn          int
	WindowSummary string // v1 留空；窗口摘要待 G2
}

type TurnEndData struct {
	Turn  int
	Usage message.Usage
}

type MessageStartData struct {
	Role      message.Role
	MessageID string
}

type MessageUpdateData struct {
	MessageID    string
	TextDelta    string
	ThoughtDelta string
}

type MessageEndData struct {
	Message message.Message
	Usage   message.Usage
}

// ToolGuardDecisionData 是每次 Guard 决议的 durable 审计轨迹。
type ToolGuardDecisionData struct {
	Call      tool.Call
	Action    GuardAction
	Reason    string
	Rewritten *tool.Call // 仅 Rewrite
}

type ToolExecStartData struct{ Call tool.Call }

// ToolExecEndData 的 Denied=true 表示 Guard 拒绝（未执行，02 §3.1：被拒也发 End）。
type ToolExecEndData struct {
	Call   tool.Call
	Result tool.Result
	Denied bool
}

type ProgressData struct {
	CallID  string
	Percent float64
	Note    string
}

type AgentEndData struct {
	Result RunResult
	Err    error
}
