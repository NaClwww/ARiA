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
	// KindWindowCompressed 由 runtime 窗口在间隙压缩后发出（不是飞轮事件：
	// 无 RunID/Turn，发生在 run 之间），durable——压缩何时发生、压了多少，
	// 落盘可审计（否则只能从 token 量间接推断）。
	KindWindowCompressed Kind = "window_compressed"
	KindAgentEnd         Kind = "agent_end"
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

// WindowCompressedData 是间隙压缩的一次报告：输入（旧记忆+本批）与输出的
// 规模、失败原因（空=成功）、失败退回时按上限裁掉的消息数。
type WindowCompressedData struct {
	InMessages      int    `json:"in_messages"`
	InChars         int    `json:"in_chars"` // 文本+思考块字符数（信息量粗估）
	OutMessages     int    `json:"out_messages"`
	OutChars        int    `json:"out_chars"`
	Err             string `json:"err,omitempty"`
	FallbackDropped int    `json:"fallback_dropped,omitempty"` // 压缩失败退回时裁掉的消息数
}

type AgentEndData struct {
	Result RunResult
	Err    error
}

// cloneEvent 给每订阅者一份独立快照，防止任一消费者篡改共享底层
// （Blocks / RawMessage / 音视频字节）影响其他订阅者或持久化真值。
func cloneEvent(ev Event) Event {
	switch d := ev.Data.(type) {
	case AgentStartData:
		d.InitialInput = cloneMessages(d.InitialInput)
		ev.Data = d
	case UserMessageInjectedData:
		d.Message = d.Message.Clone()
		ev.Data = d
	case MessageEndData:
		d.Message = d.Message.Clone()
		ev.Data = d
	case ToolGuardDecisionData:
		d.Call = d.Call.Clone()
		if d.Rewritten != nil {
			r := d.Rewritten.Clone()
			d.Rewritten = &r
		}
		ev.Data = d
	case ToolExecStartData:
		d.Call = d.Call.Clone()
		ev.Data = d
	case ToolExecEndData:
		d.Call = d.Call.Clone()
		d.Result = d.Result.Clone()
		ev.Data = d
	case *MessageEndData:
		c := *d
		c.Message = c.Message.Clone()
		ev.Data = &c
	case *ToolExecEndData:
		c := *d
		c.Call = c.Call.Clone()
		c.Result = c.Result.Clone()
		ev.Data = &c
	}
	return ev
}
