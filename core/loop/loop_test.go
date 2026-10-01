package loop

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
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
	for ev := range ch {
		out = append(out, ev)
	}
	return out
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

// Queue 只在运行中接受：未开始 / 已返回一律拒绝（ErrNoActiveRun）。
// 空闲入队会被下一次 Run 在其新输入之后排空——「后说的在前」（06 §2）。
func TestQueueRejectedWhenIdle(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"好的。"}})
	l, _ := New(Config{Provider: fake})
	if err := l.Queue(message.NewUser("x")); err == nil {
		t.Fatal("Run 之前 Queue 应被拒绝")
	}

	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("开场")})
	assertEnd(t, res, err, EndDone)
	drain(ch, cancel)

	if err := l.Queue(message.NewUser("y")); err == nil {
		t.Fatal("Run 返回后 Queue 应被拒绝")
	}
}

// 白盒构造的残留队列由下一次 Run 在起点注入：新输入在前、残留消息在后。
// 各出口已在 finish 统一封口并注入，正常路径不产生残留；本用例只验证起点注入逻辑。
func TestStrandedQueueDrainedAtNextRunStart(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"好的。"}})
	l, _ := New(Config{Provider: fake})
	l.mu.Lock()
	l.queue = append(l.queue, message.NewUser("先说这个")) // 白盒：模拟错误路径滞留
	l.mu.Unlock()

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

// 本轮内已被 Queue 接受的消息在错误出口由 finish 注入：UserMessageInjected 先于
// AgentEnd 发出，Run 返回后 Queue 拒收，下一次 Run 的 transcript 不再含该消息。
func TestQueuedMessageInjectedOnErrorExit(t *testing.T) {
	var l *Loop
	var calls atomic.Int32
	probe := probeFunc(func(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
		ch := make(chan provider.StreamEvent, 1)
		if calls.Add(1) == 1 {
			if err := l.Queue(message.NewUser("等一下")); err != nil {
				t.Errorf("运行中 Queue 应被接受：%v", err)
			}
			ch <- provider.ErrorEvent{Err: errors.New("bad request")}
		} else {
			ch <- provider.MessageComplete{Message: message.NewAssistant("好的。")}
		}
		close(ch)
		return ch, nil
	})
	l, _ = New(Config{Provider: probe})
	ch, cancel := l.Subscribe(1024)

	res, err := l.Run(testCtx(), []message.Message{message.NewUser("开场")})
	assertEnd(t, res, err, EndError)
	if err := l.Queue(message.NewUser("迟到")); err == nil {
		t.Fatal("Run 返回后 Queue 应被拒绝")
	}
	res, err = l.Run(testCtx(), []message.Message{message.NewUser("第二轮")})
	assertEnd(t, res, err, EndDone)

	events := drain(ch, cancel)
	var order []Kind
	for _, ev := range events {
		if ev.Kind == KindUserMessageInjected || ev.Kind == KindAgentEnd {
			order = append(order, ev.Kind)
		}
	}
	if wantOrder := []Kind{KindUserMessageInjected, KindAgentEnd, KindAgentEnd}; !slices.Equal(order, wantOrder) {
		t.Fatalf("事件顺序：want %v got %v", wantOrder, order)
	}
	assertSameMessages(t, []message.Message{
		message.NewUser("开场"),
		message.NewUser("等一下"),
		message.NewUser("第二轮"),
		message.NewAssistant("好的。"),
	}, rebuild(events))
}

// 工具 panic 转为 IsError 结果返回模型，Run 照常续轮收敛。
func TestToolPanicBecomesErrorResult(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "boom"}}},
		provider.FakeStep{Text: []string{"工具出错了。"}},
	)
	l, err := New(Config{Provider: fake, Tools: []tool.Tool{panicTool{}}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("试试")})
	assertEnd(t, res, err, EndDone)
	// transcript：[user, assistant(tool_calls), tool 结果, assistant]
	got := rebuild(drain(ch, cancel))
	if len(got) != 4 {
		t.Fatalf("transcript: %+v", got)
	}
	if tr := got[2]; tr.Role != message.RoleTool || tr.ToolCallID != "c1" || !tr.IsError ||
		!strings.Contains(tr.Text(), "boom") {
		t.Fatalf("panic 应转为 IsError 结果：%+v", tr)
	}
}

// panicTool 的 Exec 恒定 panic。
type panicTool struct{}

func (panicTool) Def() tool.Def { return tool.Def{Name: "boom"} }
func (panicTool) Exec(context.Context, tool.Call) tool.Result {
	panic("boom")
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
		if err := l.Queue(message.NewUser("等等，补充一点")); err != nil {
			t.Errorf("运行中 Queue 应被接受：%v", err)
		}
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

// 坏 transcript（孤立 tool 结果）在发送边界被拒：EndError、错误带 ID、
// provider 未被调用——配对完整性由 message.ValidateToolPairing 把关，
// runtime 各层不再对形状悄悄缝补。
func TestInvalidTranscriptFailsLoud(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"不该被调用"}})
	l, _ := New(Config{Provider: fake})
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{
		message.NewUser("hi"),
		{Role: message.RoleTool, ToolCallID: "ghost", Blocks: []message.Block{message.TextBlock{Text: "孤儿"}}},
	})
	assertEnd(t, res, err, EndError)
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error should name the orphan call ID: %v", err)
	}
	drain(ch, cancel)
	if fake.Left() != 1 {
		t.Fatalf("provider must not be called, %d steps left", fake.Left())
	}
}

// 空输入直接拒绝：一次 Run 至少从一条消息起步——发空 messages 给 provider
// 只会得到一条含糊的 400，不如在前置检查就响亮失败（与 ErrNoScope 同类）。
func TestRunRejectsEmptyInput(t *testing.T) {
	l, _ := New(Config{Provider: provider.NewFake()})
	if _, err := l.Run(testCtx(), nil); err == nil {
		t.Fatal("empty input should be rejected")
	}
	if _, err := l.Run(testCtx(), []message.Message{}); err == nil {
		t.Fatal("empty (non-nil) input should be rejected")
	}
}

// 工具名违反 OpenAI 名字规范（如带点号）在装配期拒绝——部分网关严格校验，
// 违规请求会换回一条含糊的 400。
func TestInvalidToolNameRejected(t *testing.T) {
	fake := provider.NewFake()
	for _, name := range []string{"", "artifact.open", "带中文", "with space"} {
		if _, err := New(Config{Provider: fake, Tools: []tool.Tool{
			tool.NewFake(name, message.ToolResult{}, 0),
		}}); err == nil {
			t.Fatalf("tool name %q should be rejected", name)
		}
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
		if err := l.Queue(message.NewUser("别说了，说这个")); err != nil {
			t.Errorf("运行中 Queue 应被接受：%v", err)
		}
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

func TestBusDurableFIFOCloneAndClose(t *testing.T) {
	b := newBusWithTimeout(time.Second)
	ch, unsubscribe := b.add(1)
	payload := &MessageEndData{Message: message.Message{
		Role:   message.RoleAssistant,
		Blocks: []message.Block{message.ImageBlock{Data: []byte{1}}},
	}}
	for i := 0; i < 8; i++ {
		b.emit(Event{Kind: KindMessageEnd, Turn: i, Data: payload})
	}
	payload.Message.Blocks[0].(message.ImageBlock).Data[0] = 9
	unsubscribe()

	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 8 {
		t.Fatalf("durable events: want 8 got %d", len(events))
	}
	for i, ev := range events {
		if ev.Turn != i {
			t.Fatalf("FIFO order at %d: turn=%d", i, ev.Turn)
		}
		d, ok := ev.Data.(*MessageEndData)
		if !ok || d.Message.Blocks[0].(message.ImageBlock).Data[0] != 1 {
			t.Fatalf("event %d did not clone Data: %#v", i, ev.Data)
		}
	}
}

func TestPreflightCancellationReportsZeroTurns(t *testing.T) {
	fake := provider.NewFake(provider.FakeStep{Text: []string{"unused"}})
	l, _ := New(Config{Provider: fake})
	ctx, cancel := context.WithCancel(testCtx())
	cancel()
	res, err := l.Run(ctx, []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndCancelled)
	if res.Turns != 0 || fake.Left() != 1 {
		t.Fatalf("preflight must use zero provider turns: result=%+v left=%d", res, fake.Left())
	}
}

type blockingGuard struct{ started chan struct{} }

func (g blockingGuard) Check(ctx context.Context, _ message.ToolCall) Decision {
	close(g.started)
	<-ctx.Done()
	return Decision{Action: GuardAllow}
}

func TestInterruptCancelsGuard(t *testing.T) {
	started := make(chan struct{})
	fake := provider.NewFake(provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "slow"}}})
	slow := tool.NewFake("slow", message.ToolResult{}, 0)
	l, _ := New(Config{Provider: fake, Tools: []tool.Tool{slow}, Guard: blockingGuard{started: started}})
	go func() {
		<-started
		l.Interrupt()
	}()
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("run")})
	assertEnd(t, res, err, EndInterrupted)
	if n, _, _ := slow.Calls(); n != 0 {
		t.Fatalf("tool executed after guard interruption: %d", n)
	}
}

type requestProbe struct {
	requests []provider.Request
	contexts []context.Context
	calls    int
}

func (p *requestProbe) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	p.contexts = append(p.contexts, ctx)
	p.requests = append(p.requests, req)
	ch := make(chan provider.StreamEvent, 1)
	if p.calls == 0 {
		ch <- provider.ErrorEvent{Err: errors.New("retry"), Retryable: true}
	} else {
		ch <- provider.MessageComplete{Message: message.NewAssistant("done")}
	}
	p.calls++
	close(ch)
	return ch, nil
}

func TestRetryContextsTerminateAndToolsKeepConfigOrder(t *testing.T) {
	probe := &requestProbe{}
	first := tool.NewFake("first", message.ToolResult{}, 0)
	second := tool.NewFake("second", message.ToolResult{}, 0)
	l, _ := New(Config{Provider: probe, Tools: []tool.Tool{first, second}})
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndDone)
	if len(probe.requests) != 2 || len(probe.requests[0].Tools) != 2 ||
		probe.requests[0].Tools[0].Name != "first" || probe.requests[0].Tools[1].Name != "second" {
		t.Fatalf("tool definition order changed: %+v", probe.requests)
	}
	for i, ctx := range probe.contexts {
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("attempt context %d remains live", i)
		}
	}
}

func TestRewriteAuditPreservesOriginalID(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "original", Name: "search"}}},
		provider.FakeStep{Text: []string{"done"}},
	)
	search := tool.NewFake("search", message.ToolResult{}, 0)
	l, _ := New(Config{Provider: fake, Tools: []tool.Tool{search}, Guard: rewriteGuard{}})
	ch, unsubscribe := l.Subscribe(32)
	_, err := l.Run(testCtx(), []message.Message{message.NewUser("search")})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range drain(ch, unsubscribe) {
		if d, ok := ev.Data.(ToolGuardDecisionData); ok {
			if d.Call.ID != "original" || d.Rewritten == nil || d.Rewritten.ID != "original" {
				t.Fatalf("rewrite audit IDs: %+v", d)
			}
			return
		}
	}
	t.Fatal("missing rewrite audit event")
}

// Interrupt 落在重试退避间隙：必须立刻抢占，不得让下一个 attempt
// 带着已置位的 interrupt 标志继续跑完整个流。
func TestInterruptDuringRetryBackoff(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Err: errors.New("503"), Retryable: true},
		provider.FakeStep{Text: []string{"很长的回答"}, ChunkDelay: 300 * time.Millisecond},
	)
	l, _ := New(Config{Provider: fake})
	go func() {
		time.Sleep(20 * time.Millisecond) // 落在 50ms 退避窗口内
		l.Interrupt()
	}()
	start := time.Now()
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndInterrupted)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("interrupt during backoff did not preempt: %v", elapsed)
	}
	if fake.Left() != 1 {
		t.Fatalf("next attempt must not consume a step after backoff interrupt: left=%d", fake.Left())
	}
}

// 重试 attempt 之间 Temperature 指针必须隔离：首个 attempt 收到的请求
// 被恶意修改后，重试请求不得看到变化。
func TestRetryRequestTemperatureIsolation(t *testing.T) {
	var first atomic.Bool
	var retryTemp atomic.Pointer[float64]
	probe := probeFunc(func(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
		ch := make(chan provider.StreamEvent, 1)
		if !first.CompareAndSwap(false, true) {
			v := *req.Options.Temperature
			retryTemp.Store(&v)
			ch <- provider.MessageComplete{Message: message.NewAssistant("ok")}
			close(ch)
			return ch, nil
		}
		*req.Options.Temperature = 99.0 // 恶意修改本 attempt 收到的请求
		ch <- provider.ErrorEvent{Err: errors.New("503"), Retryable: true}
		close(ch)
		return ch, nil
	})
	l, _ := New(Config{Provider: probe})
	ctx := ctxx.WithOptions(testCtx(), ctxx.Options{Temperature: ptr(0.7)})
	res, err := l.Run(ctx, []message.Message{message.NewUser("hi")})
	assertEnd(t, res, err, EndDone)
	if retryTemp.Load() == nil || *retryTemp.Load() != 0.7 {
		t.Fatalf("retry attempt saw mutated temperature: %v", retryTemp.Load())
	}
}

func probeFunc(fn func(context.Context, provider.Request) (<-chan provider.StreamEvent, error)) provider.Provider {
	return probeFuncT(fn)
}

type probeFuncT func(context.Context, provider.Request) (<-chan provider.StreamEvent, error)

func (f probeFuncT) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	return f(ctx, req)
}

func ptr(v float64) *float64 { return &v }

// GuardDeny 分支退出前必须清理 opCancel：不得残留指向已完成调用 ctx 的
// cancel（否则后续 Interrupt 会调用陈旧 cancel，掩盖真实操作靶点）。
func TestGuardDenyCleansOpCancel(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "rm_rf"}}},
		provider.FakeStep{Text: []string{"好吧。"}},
	)
	danger := tool.NewFake("rm_rf", message.ToolResult{}, 0)
	l, _ := New(Config{Provider: fake, Tools: []tool.Tool{danger}, Guard: denyGuard{}})
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("删库")})
	assertEnd(t, res, err, EndDone)
	if box := l.opCancel.Load(); box != nil && box.fn != nil {
		t.Fatal("opCancel retains stale cancel after GuardDeny branch")
	}
}

// AgentStart 的 turn 必须是 0：Loop 会被复用（一个 Session 一个 Loop），
// 不复位就会把上一轮结束时的计数值写进 durable 记录，同一 run 内 turn 自相矛盾。
func TestAgentStartTurnResetsBetweenRuns(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Text: []string{"一"}},
		provider.FakeStep{Text: []string{"二"}},
	)
	l, err := New(Config{Provider: fake})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	defer cancel()

	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("第一轮")}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(testCtx(), []message.Message{message.NewUser("第二轮")}); err != nil {
		t.Fatal(err)
	}

	var starts []int
	for _, ev := range drain(ch, cancel) {
		if ev.Kind == KindAgentStart {
			starts = append(starts, ev.Turn)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("AgentStart 事件数 = %d, want 2", len(starts))
	}
	for i, turn := range starts {
		if turn != 0 {
			t.Fatalf("第 %d 个 run 的 AgentStart turn = %d, want 0", i+1, turn)
		}
	}
}

// ---------- Stopper：模型显式收尾 ----------

// stopperTool 是 Stopper 的最小实现（模拟 speak 模式的 stop 工具）。
type stopperTool struct{ onExec func() }

func (stopperTool) Def() tool.Def { return tool.Def{Name: "stop"} }
func (s stopperTool) Exec(_ context.Context, call tool.Call) tool.Result {
	if s.onExec != nil {
		s.onExec()
	}
	return tool.Result{CallID: call.ID,
		Blocks: []message.Block{message.TextBlock{Text: "已收尾"}}}
}
func (stopperTool) StopsLoop() bool { return true }

// Stopper 成功执行 → 本轮工具段后收敛 EndDone，不再发起下一轮 provider
// 调用（第二段脚本不被消费），工具结果照常进历史（配对完整）。
func TestStopperEndsRunAfterToolBatch(t *testing.T) {
	call := message.ToolCall{ID: "c1", Name: "stop"}
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{call}},
		provider.FakeStep{Text: []string{"这段不该被生成"}},
	)
	l, err := New(Config{Provider: fake, Tools: []tool.Tool{stopperTool{}}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("说完了")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 1 {
		t.Fatalf("turns: want 1 got %d", res.Turns)
	}
	if left := fake.Left(); left != 1 {
		t.Fatalf("stop 后不应再调 provider：剩 %d 段脚本未消费（want 1）", left)
	}
	var toolResults int
	for _, ev := range drain(ch, cancel) {
		if ev.Kind == KindToolExecEnd {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("工具结果事件 = %d, want 1", toolResults)
	}
}

// steering 插话优先于收尾：stop 执行期间入队的用户消息让 run 续轮回答，
// 回答完自然收敛（stop 请求被插话复位，不拦后续）。
func TestSteeringSupersedesStopRequest(t *testing.T) {
	var l *Loop
	qstop := stopperTool{onExec: func() {
		if err := l.Queue(message.NewUser("等等，还有一件事")); err != nil {
			t.Errorf("Queue 注入失败: %v", err)
		}
	}}
	call := message.ToolCall{ID: "c1", Name: "stop"}
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{call}},
		provider.FakeStep{Text: []string{"好，你说。"}},
	)
	var err error
	l, err = New(Config{Provider: fake, Tools: []tool.Tool{qstop}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("说完了")})
	assertEnd(t, res, err, EndDone)
	if res.Turns != 2 {
		t.Fatalf("插话应续轮回答：turns = %d, want 2", res.Turns)
	}
	var injected int
	for _, ev := range drain(ch, cancel) {
		if ev.Kind == KindUserMessageInjected {
			injected++
		}
	}
	if injected != 1 {
		t.Fatalf("steering 注入事件 = %d, want 1", injected)
	}
}

// 回归（真机 2026-09-29：模型一批发出 [speak,speak,stop,speak,speak]）：
// stop 成功后同批次剩余调用不得执行——那是模型的犹豫改口，照单执行会把
// 矛盾内容全部放出去。剩余调用补 stopped 结果（配对完整，重放不炸）。
func TestStopperTruncatesRestOfBatch(t *testing.T) {
	var execs int
	counting := execCounting{name: "speak", n: &execs}
	calls := []message.ToolCall{
		{ID: "c1", Name: "speak"},
		{ID: "c2", Name: "stop"},
		{ID: "c3", Name: "speak"},
	}
	fake := provider.NewFake(
		provider.FakeStep{Calls: calls},
		provider.FakeStep{Text: []string{"不该到这"}},
	)
	l, err := New(Config{Provider: fake, Tools: []tool.Tool{counting, stopperTool{}}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("看得到吗")})
	assertEnd(t, res, err, EndDone)
	if execs != 1 {
		t.Fatalf("stop 前的 speak 应执行 1 次，实际 %d（stop 后的不得执行）", execs)
	}
	if left := fake.Left(); left != 1 {
		t.Fatalf("不应进入下一轮：剩 %d 段脚本（want 1）", left)
	}
	var stoppedResults, okResults int
	for _, ev := range drain(ch, cancel) {
		e, isEnd := ev.Data.(ToolExecEndData)
		if !isEnd {
			continue
		}
		switch e.Call.ID {
		case "c1":
			if !e.Result.IsError {
				okResults++
			}
		case "c3":
			if e.Result.IsError {
				stoppedResults++
			}
		}
	}
	if okResults != 1 || stoppedResults != 1 {
		t.Fatalf("c1 应成功、c3 应 stopped：ok=%d stopped=%d", okResults, stoppedResults)
	}
}

// execCounting 是记执行次数的哑工具。
type execCounting struct {
	name string
	n    *int
}

func (e execCounting) Def() tool.Def { return tool.Def{Name: e.name} }
func (e execCounting) Exec(_ context.Context, call tool.Call) tool.Result {
	*e.n++
	return tool.Result{CallID: call.ID, Blocks: []message.Block{message.TextBlock{Text: "done"}}}
}
