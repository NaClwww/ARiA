package loop

import (
	"context"
	"testing"
	"time"

	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/message"
)

// funcTool 是 Exec 由测试给出的工具。
type funcTool struct {
	name string
	fn   func()
}

func (f funcTool) Def() tool.Def { return tool.Def{Name: f.name} }
func (f funcTool) Exec(_ context.Context, c tool.Call) tool.Result {
	f.fn()
	return tool.Result{CallID: c.ID, Blocks: []message.Block{message.TextBlock{Text: "ok"}}}
}

func reconfiguredEvents(evs []Event) []ReconfiguredData {
	var out []ReconfiguredData
	for _, ev := range evs {
		if d, ok := ev.Data.(ReconfiguredData); ok {
			out = append(out, d)
		}
	}
	return out
}

// 空闲时 Reconfigure：下一次 Run 的第一轮起用新 provider 与工具表，生效时发一次 durable
// reconfigured（在 agent_start 之后、turn_start 之前）；没有新配置的 Run 不发。
func TestReconfigureBetweenRuns(t *testing.T) {
	a := provider.NewFake(provider.FakeStep{Text: []string{"a"}})
	b := provider.NewFake(provider.FakeStep{Text: []string{"b"}}, provider.FakeStep{Text: []string{"b2"}})
	l, err := New(Config{Provider: a})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("1")}); err != nil {
		t.Fatal(err)
	}

	if err := l.Reconfigure(Config{Provider: b, Tools: []tool.Tool{funcTool{name: "x", fn: func() {}}}, MaxTurns: 7}, "model=b"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(64)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("2")})
	assertEnd(t, res, err, EndDone)
	evs := drain(ch, cancel)
	if b.Left() != 1 || a.Left() != 0 {
		t.Fatalf("calls: a left %d, b left %d; want a 0, b 1", a.Left(), b.Left())
	}
	rc := reconfiguredEvents(evs)
	if len(rc) != 1 || rc[0].Note != "model=b" || len(rc[0].Tools) != 1 || rc[0].Tools[0] != "x" || rc[0].MaxTurns != 7 {
		t.Fatalf("reconfigured events = %+v", rc)
	}
	if evs[0].Kind != KindAgentStart || evs[1].Kind != KindReconfigured || evs[2].Kind != KindTurnStart {
		t.Fatalf("event order: %s, %s, %s", evs[0].Kind, evs[1].Kind, evs[2].Kind)
	}

	ch, cancel = l.Subscribe(64)
	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("3")}); err != nil {
		t.Fatal(err)
	}
	if rc := reconfiguredEvents(drain(ch, cancel)); len(rc) != 0 {
		t.Fatalf("unexpected reconfigured without change: %+v", rc)
	}
}

// 运行中 Reconfigure（工具执行期间）：本轮不受影响，下一轮（下一次 LLM 请求）起用新配置。
func TestReconfigureMidRun(t *testing.T) {
	call := message.ToolCall{ID: "c1", Name: "swap", Args: []byte(`{}`)}
	a := provider.NewFake(provider.FakeStep{Calls: []message.ToolCall{call}}, provider.FakeStep{Text: []string{"a-done"}})
	b := provider.NewFake(provider.FakeStep{Text: []string{"b-done"}})
	var l *Loop
	swap := funcTool{name: "swap", fn: func() {
		if err := l.Reconfigure(Config{Provider: b}, "mid"); err != nil {
			t.Error(err)
		}
	}}
	l, err := New(Config{Provider: a, Tools: []tool.Tool{swap}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(64)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("go")})
	assertEnd(t, res, err, EndDone)
	evs := drain(ch, cancel)
	if a.Left() != 1 || b.Left() != 0 {
		t.Fatalf("a left %d (want 1: second step unused), b left %d (want 0)", a.Left(), b.Left())
	}
	msgs := rebuild(evs)
	if got := msgs[len(msgs)-1].Text(); got != "b-done" {
		t.Fatalf("final reply = %q, want from new provider", got)
	}
	if rc := reconfiguredEvents(evs); len(rc) != 1 || rc[0].Note != "mid" {
		t.Fatalf("reconfigured events = %+v", rc)
	}
}

// 非法配置报错且不替换；连续两次 Reconfigure 只有最后一次生效（一次事件）。
func TestReconfigureInvalidAndCoalesced(t *testing.T) {
	a := provider.NewFake(provider.FakeStep{Text: []string{"a"}})
	b := provider.NewFake(provider.FakeStep{Text: []string{"b"}})
	c := provider.NewFake(provider.FakeStep{Text: []string{"c"}})
	l, err := New(Config{Provider: a, ToolTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Reconfigure(Config{}, "no provider"); err == nil {
		t.Fatal("want error without provider")
	}
	dup := funcTool{name: "d", fn: func() {}}
	if err := l.Reconfigure(Config{Provider: b, Tools: []tool.Tool{dup, dup}}, "dup"); err == nil {
		t.Fatal("want error on duplicate tool")
	}
	if err := l.Reconfigure(Config{Provider: b}, "b"); err != nil {
		t.Fatal(err)
	}
	if err := l.Reconfigure(Config{Provider: c}, "c"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(64)
	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("1")}); err != nil {
		t.Fatal(err)
	}
	rc := reconfiguredEvents(drain(ch, cancel))
	if c.Left() != 0 || b.Left() != 1 || len(rc) != 1 || rc[0].Note != "c" {
		t.Fatalf("b left %d, c left %d, events %+v", b.Left(), c.Left(), rc)
	}
}
