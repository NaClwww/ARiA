package loop

import (
	"testing"

	"aria/pkg/message"
)

// HistoryMessage 是「哪些事件进历史」的权威定义：窗口结算、记忆 Pump 共用。
// 这里钉住三正三负——尤其 AgentStart.InitialInput 是整包组装结果，不是历史。
func TestHistoryMessage(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want bool
		role message.Role
	}{
		{
			name: "assistant 消息",
			ev:   Event{Kind: KindMessageEnd, Data: MessageEndData{Message: message.NewAssistant("好")}},
			want: true, role: message.RoleAssistant,
		},
		{
			name: "工具结果（含被拒）转 tool 消息",
			ev: Event{Kind: KindToolExecEnd, Data: ToolExecEndData{
				Result: message.ToolResult{CallID: "c1", Blocks: []message.Block{message.TextBlock{Text: "42"}}, IsError: true},
			}},
			want: true, role: message.RoleTool,
		},
		{
			name: "轮间注入的用户输入",
			ev:   Event{Kind: KindUserMessageInjected, Data: UserMessageInjectedData{Message: message.NewUser("[nacl] 早")}},
			want: true, role: message.RoleUser,
		},
		{
			name: "AgentStart 的 InitialInput 不是历史（整包组装结果）",
			ev:   Event{Kind: KindAgentStart, Data: AgentStartData{InitialInput: []message.Message{message.NewUser("hi")}}},
			want: false,
		},
		{name: "轮边界不是消息", ev: Event{Kind: KindTurnEnd, Data: TurnEndData{}}, want: false},
		{name: "流开始不是消息", ev: Event{Kind: KindMessageStart, Data: MessageStartData{Role: message.RoleAssistant}}, want: false},
	}
	for _, c := range cases {
		m, ok := HistoryMessage(c.ev)
		if ok != c.want {
			t.Errorf("%s: ok = %v，想要 %v", c.name, ok, c.want)
			continue
		}
		if ok && m.Role != c.role {
			t.Errorf("%s: role = %v，想要 %v", c.name, m.Role, c.role)
		}
	}

	// 工具结果的 IsError 必须带进 tool 消息（历史里失败要可辨）。
	m, ok := HistoryMessage(Event{Kind: KindToolExecEnd, Data: ToolExecEndData{
		Result: message.ToolResult{CallID: "c1", IsError: true},
	}})
	if !ok || !m.IsError || m.ToolCallID != "c1" {
		t.Errorf("工具结果转消息丢字段：%+v", m)
	}

	// 指针形态不认（core 只发值形态）：认了会与 handleEvent 语义分叉。
	if _, ok := HistoryMessage(Event{Kind: KindMessageEnd, Data: &MessageEndData{}}); ok {
		t.Error("指针形态不应被认作历史消息")
	}
}
