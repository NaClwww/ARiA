package speculate

import (
	"context"
	"testing"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

func testCtx() context.Context {
	return ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "u1", SessionID: "s1"})
}

func noScopeCtx() context.Context { return context.Background() }

// 确认命中：返回预生成答案，输入已定型为 user role。
func TestConfirmReturnsPredictedAnswer(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"预测答案"}})
	p, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	base := []message.Message{message.NewUser("之前的问题"), message.NewAssistant("之前的回答")}

	if err := p.Speculate(testCtx(), base, Input{ID: "u1", Revision: 1, Message: message.NewUser("猜测输入")}, nil); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Confirm("u1", 1)
	if !ok {
		t.Fatal("confirm did not hit")
	}
	if got.Input.Role != message.RoleUser || got.Input.Text() != "猜测输入" {
		t.Fatalf("input: %+v", got.Input)
	}
	if got.Msg.Text() != "预测答案" {
		t.Fatalf("msg: %q", got.Msg.Text())
	}
	// 确认后槽已清空，重复确认失败
	if _, ok := p.Confirm("u1", 1); ok {
		t.Fatal("confirm must clear the slot")
	}
}

// Replace 语义：新 Speculate 顶掉旧预测，旧 revision 确认被拒绝。
func TestNewSpeculationReplacesOldAndStaleConfirmFails(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Text: []string{"旧猜测"}},
		provider.FakeStep{Text: []string{"新猜测"}},
	)
	p, _ := New(fake)

	if err := p.Speculate(testCtx(), nil, Input{ID: "u1", Revision: 1, Message: message.NewUser("旧")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Speculate(testCtx(), nil, Input{ID: "u1", Revision: 2, Message: message.NewUser("新")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Confirm("u1", 1); ok {
		t.Fatal("stale revision confirm must fail")
	}
	got, ok := p.Confirm("u1", 2)
	if !ok || got.Msg.Text() != "新猜测" {
		t.Fatalf("confirm rev2: ok=%v msg=%q", ok, got.Msg.Text())
	}
}

// 预测输出带工具调用时不可复用：工具被剥除（预测轮不执行工具）。
func TestToolCallsStrippedFromPrediction(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{
		Calls: []message.ToolCall{{ID: "c1", Name: "search"}},
	})
	p, _ := New(fake)
	if err := p.Speculate(testCtx(), nil, Input{ID: "u1", Revision: 1, Message: message.NewUser("搜")}, nil); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Confirm("u1", 1)
	if !ok {
		t.Fatal("confirm failed")
	}
	if len(got.Msg.ToolCalls) != 0 {
		t.Fatalf("tool calls must be stripped: %+v", got.Msg.ToolCalls)
	}
}

// 取消后无结果；Scope 缺失 fail-closed。
func TestCancelAndScopeRequired(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"x"}, ChunkDelay: 50 * time.Millisecond})
	p, _ := New(fake)

	if err := p.Speculate(noScopeCtx(), nil, Input{ID: "u1", Revision: 1, Message: message.NewUser("x")}, nil); err == nil {
		t.Fatal("scope missing must fail-closed")
	}

	if err := p.Speculate(testCtx(), nil, Input{ID: "u1", Revision: 1, Message: message.NewUser("x")}, nil); err != nil {
		t.Fatal(err)
	}
	if !p.Cancel("u1", 1) {
		t.Fatal("cancel did not match")
	}
	if _, ok := p.Confirm("u1", 1); ok {
		t.Fatal("cancelled speculation must not confirm")
	}
	// Confirm 未命中后槽已清空，后续控制命令不再匹配
	if p.Cancel("u1", 1) {
		t.Fatal("control commands must not match a cleared slot")
	}
}
