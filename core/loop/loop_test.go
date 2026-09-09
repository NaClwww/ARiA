package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// ---------- 测试基建 ----------

func testCtx() context.Context {
	ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "u1", SessionID: "s1"})
	ctx, _ = ctxx.EnsureTrace(ctx)
	return ctx
}

// drain 在 Run 返回后退订并收干缓冲区（emit 同步入队，Run 返回即全部可见）。
func drain(ch <-chan Event, cancel func()) []Event {
	cancel()
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// rebuild 按 02 §3.1 的事件溯源规则 1:1 还原 transcript。
func rebuild(events []Event) []message.Message {
	var out []message.Message
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case AgentStartData:
			out = append(out, d.InitialInput...)
		case UserMessageInjectedData:
			out = append(out, d.Message)
		case MessageEndData:
			out = append(out, d.Message)
		case ToolExecEndData:
			out = append(out, d.Result.ToMessage())
		}
	}
	return out
}

func assertSameMessages(t *testing.T, want, got []message.Message) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("message count: want %d got %d\nwant: %+v\ngot:  %+v", len(want), len(got), want, got)
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Role != g.Role || w.Text() != g.Text() || w.ToolCallID != g.ToolCallID ||
			w.Interrupted != g.Interrupted || len(w.ToolCalls) != len(g.ToolCalls) {
			t.Fatalf("message %d mismatch:\nwant: %+v\ngot:  %+v", i, w, g)
		}
		for j := range w.ToolCalls {
			if w.ToolCalls[j].ID != g.ToolCalls[j].ID || w.ToolCalls[j].Name != g.ToolCalls[j].Name ||
				string(w.ToolCalls[j].Args) != string(g.ToolCalls[j].Args) {
				t.Fatalf("message %d toolcall %d mismatch: want %+v got %+v", i, j, w.ToolCalls[j], g.ToolCalls[j])
			}
		}
	}
}

func assertEnd(t *testing.T, res RunResult, err error, want EndReason) RunResult {
	t.Helper()
	if err != nil && want != EndError && want != EndCancelled {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.EndReason != want {
		t.Fatalf("end reason: want %q got %q (err=%v)", want, res.EndReason, err)
	}
	return res
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	if t != nil {
		t.Helper()
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------- 场景 ----------

// 多轮工具循环 + 事件溯源 1:1 重建（A2 验收）。
func TestMultiTurnToolLoop(t *testing.T) {
	call := message.ToolCall{ID: "c1", Name: "get_weather", Args: mustJSON(t, map[string]string{"city": "北京"})}
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{call}},
		provider.FakeStep{Text: []string{"今天 22 度，晴。"}},
	)
	weather := tool.NewFake("get_weather", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: `{"temp":22}`}},
	}, 0)

	l, err := New(Config{Provider: fake, Tools: []tool.Tool{weather}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("查天气")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 2 {
		t.Fatalf("turns: want 2 got %d", res.Turns)
	}

	events := drain(ch, cancel)
	want := []message.Message{
		message.NewUser("查天气"),
		{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{call}},
		{Role: message.RoleTool, ToolCallID: "c1", Blocks: []message.Block{message.TextBlock{Text: `{"temp":22}`}}},
		message.NewAssistant("今天 22 度，晴。"),
	}
	assertSameMessages(t, want, rebuild(events)) // 事件溯源 == 内存历史
	if n, _, _ := weather.Calls(); n != 1 {
		t.Fatalf("tool exec count: want 1 got %d", n)
	}
}

// Run 前积压的 Queue 在起点注入。
func TestQueueBeforeRun(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"好的。"}})
	l, _ := New(Config{Provider: fake})
	l.Queue(message.NewUser("先说这个"))

	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("开场")})
	assertEnd(t, res, err, EndDone)

	events := drain(ch, cancel)
	want := []message.Message{
		message.NewUser("开场"),
		message.NewUser("先说这个"),
		message.NewAssistant("好的。"),
	}
	assertSameMessages(t, want, rebuild(events))
}

// 轮间注入：首轮无工具调用但队列有输入 → 续轮回答。
func TestQueueMidRun(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Text: []string{"a", "b", "c", "d"}, ChunkDelay: 30 * time.Millisecond},
		provider.FakeStep{Text: []string{"收到补充。"}},
	)
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)

	go func() {
		time.Sleep(20 * time.Millisecond) // 首轮流式中途
		l.Queue(message.NewUser("等等，补充一点"))
	}()

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("开场")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 2 {
		t.Fatalf("turns: want 2 got %d", res.Turns)
	}
	got := rebuild(drain(ch, cancel))
	if len(got) != 4 || got[2].Text() != "等等，补充一点" || got[3].Text() != "收到补充。" {
		t.Fatalf("unexpected transcript: %+v", got)
	}
}

// Interrupt 中途打断：保留部分输出，队列空 → EndInterrupted。
func TestInterruptPartial(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{
		Text: []string{"一", "二", "三", "四", "五"}, ChunkDelay: 40 * time.Millisecond,
	})
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)

	go func() {
		time.Sleep(90 * time.Millisecond) // 已流出约 2 片
		l.Interrupt()
	}()

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("讲个长的")})
	assertEnd(t, res, err, EndInterrupted)

	events := drain(ch, cancel)
	var last message.Message
	for _, ev := range events {
		if d, ok := ev.Data.(MessageEndData); ok {
			last = d.Message
		}
	}
	if !last.Interrupted {
		t.Fatal("last assistant message should be Interrupted")
	}
	full := "一二三四五"
	if got := last.Text(); got == "" || got == full {
		t.Fatalf("partial text: got %q, want non-empty proper prefix", got)
	}
}

// Interrupt 后队列有输入 → 转向续轮。
func TestInterruptThenQueueResumes(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Text: []string{"一", "二", "三", "四", "五"}, ChunkDelay: 40 * time.Millisecond},
		provider.FakeStep{Text: []string{"改说这个。"}},
	)
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)

	go func() {
		time.Sleep(30 * time.Millisecond)
		l.Queue(message.NewUser("别说了，说这个"))
		time.Sleep(60 * time.Millisecond)
		l.Interrupt()
	}()

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("讲个长的")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 2 {
		t.Fatalf("turns: want 2 got %d", res.Turns)
	}
	got := rebuild(drain(ch, cancel))
	if len(got) != 4 || got[2].Text() != "别说了，说这个" || got[3].Text() != "改说这个。" {
		t.Fatalf("unexpected transcript: %+v", got)
	}
}

// 工具执行中 Interrupt：被取消的工具以错误结果收场（A3 默认），
// assistant 部分输出与工具结果都保留在历史中。
func TestInterruptDuringTool(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "slow"}}},
		provider.FakeStep{Text: []string{"不该到达"}},
	)
	slow := tool.NewFake("slow", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: "done"}},
	}, 300*time.Millisecond)
	l, _ := New(Config{Provider: fake, Tools: []tool.Tool{slow}, MaxTurns: 5})
	ch, cancel := l.Subscribe(1024)

	go func() {
		time.Sleep(40 * time.Millisecond)
		l.Interrupt()
	}()

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("跑")})
	assertEnd(t, res, err, EndInterrupted)

	got := rebuild(drain(ch, cancel))
	if len(got) != 3 {
		t.Fatalf("messages: want 3 got %d: %+v", len(got), got)
	}
	tr := got[2]
	if tr.Role != message.RoleTool || tr.ToolCallID != "c1" || !strings.Contains(tr.Text(), "canceled") {
		t.Fatalf("aborted tool result: %+v", tr)
	}
}

// 预算超限在下一轮 preFlight 收敛（02 §7）。
func TestBudgetExhausted(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "noop"}},
			Usage: message.Usage{Out: 100}},
		provider.FakeStep{Text: []string{"不该到达"}},
	)
	noop := tool.NewFake("noop", message.ToolResult{}, 0)
	ctx := ctxx.WithBudget(testCtx(), ctxx.NewBudget(10, 0))
	l, _ := New(Config{Provider: fake, Tools: []tool.Tool{noop}, MaxTurns: 5})
	ch, cancel := l.Subscribe(1024)

	res, err := l.Run(ctx, []message.Message{message.NewUser("跑")})
	assertEnd(t, res, err, EndBudgetExhausted)
	drain(ch, cancel)
}

// Guard 拒绝：模型看到拒绝内容自行改道（02 §8）。
func TestGuardDeny(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "rm_rf"}}},
		provider.FakeStep{Text: []string{"好吧，那我不删了。"}},
	)
	danger := tool.NewFake("rm_rf", message.ToolResult{}, 0)
	l, _ := New(Config{
		Provider: fake, Tools: []tool.Tool{danger},
		Guard: denyGuard{},
	})
	ch, cancel := l.Subscribe(1024)

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("删库")})
	assertEnd(t, res, err, EndDone)

	if n, _, _ := danger.Calls(); n != 0 {
		t.Fatalf("denied tool must not execute, got %d execs", n)
	}
	events := drain(ch, cancel)
	got := rebuild(events)
	if len(got) != 4 || got[2].Role != message.RoleTool || !strings.Contains(got[2].Text(), "denied") {
		t.Fatalf("unexpected transcript: %+v", got)
	}
	sawDecision, sawDeniedEnd := false, false
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case ToolGuardDecisionData:
			sawDecision = d.Action == GuardDeny
		case ToolExecEndData:
			sawDeniedEnd = d.Denied
		}
	}
	if !sawDecision || !sawDeniedEnd {
		t.Fatalf("audit events: decision=%v deniedEnd=%v", sawDecision, sawDeniedEnd)
	}
}

// Guard 改写：执行改写后的调用（B3 默认语义）。
func TestGuardRewrite(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{
			ID: "c1", Name: "search", Args: mustJSON(t, map[string]string{"q": "机密"}),
		}}},
		provider.FakeStep{Text: []string{"完成。"}},
	)
	search := tool.NewFake("search", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: "ok"}},
	}, 0)
	l, _ := New(Config{
		Provider: fake, Tools: []tool.Tool{search},
		Guard: rewriteGuard{},
	})
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("搜")})
	assertEnd(t, res, err, EndDone)

	if _, args, _ := search.Calls(); string(args) != `{"q":"公开"}` {
		t.Fatalf("rewritten args not executed: %s", args)
	}
	drain(ch, cancel)
}

// parent 取消 = 硬终止（EndCancelled，不尝试续跑）。
func TestParentCancel(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{
		Text: []string{"一", "二", "三"}, ChunkDelay: 40 * time.Millisecond,
	})
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)

	parent, stop := context.WithCancel(testCtx())
	go func() {
		time.Sleep(50 * time.Millisecond)
		stop()
	}()
	res, err := l.Run(parent, []message.Message{message.NewUser("跑")})
	assertEnd(t, res, err, EndCancelled)
	drain(ch, cancel)
}

// R4：Scope 缺失 fail-closed。
func TestScopeRequired(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"x"}})
	l, _ := New(Config{Provider: fake})
	if _, err := l.Run(context.Background(), nil); !errors.Is(err, ErrNoScope) {
		t.Fatalf("want ErrNoScope, got %v", err)
	}
}

// 同实例禁止并发 Run。
func TestConcurrentRunRejected(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"慢"}, ChunkDelay: 100 * time.Millisecond})
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)

	done := make(chan struct{})
	var first error
	go func() {
		defer close(done)
		_, first = l.Run(testCtx(), []message.Message{message.NewUser("a")})
	}()
	time.Sleep(30 * time.Millisecond)
	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("b")}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
	<-done
	if first != nil {
		t.Fatalf("first run failed: %v", first)
	}
	drain(ch, cancel)
}

// 未产出内容前的 Retryable 错误退避重试（02 §7）。
func TestRetryableStreamError(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Err: errors.New("503"), Retryable: true},
		provider.FakeStep{Text: []string{"恢复后成功"}},
	)
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 1 {
		t.Fatalf("turns: want 1 got %d", res.Turns)
	}
	drain(ch, cancel)
}

// 不可重试错误 → EndError。
func TestFatalStreamError(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Err: errors.New("bad request")})
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndError)
	drain(ch, cancel)
}

// ---------- Guard 测试桩 ----------

type denyGuard struct{}

func (denyGuard) Check(_ context.Context, _ message.ToolCall) Decision {
	return Decision{Action: GuardDeny, Reason: "危险操作"}
}

type rewriteGuard struct{}

func (rewriteGuard) Check(_ context.Context, tc message.ToolCall) Decision {
	return Decision{
		Action:    GuardRewrite,
		Rewritten: message.ToolCall{ID: tc.ID, Name: tc.Name, Args: mustJSON(nil, map[string]string{"q": "公开"})},
	}
}
