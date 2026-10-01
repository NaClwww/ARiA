package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/runtime/artifact"
	"aria/runtime/window"
)

// ---------- 测试替身 ----------

// recorderProvider 记录每次 Stream 收到的请求（深拷贝），用于断言进模型的上下文。
type recorderProvider struct {
	inner provider.Provider
	mu    sync.Mutex
	reqs  []provider.Request
}

func (r *recorderProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, cloneRequest(req))
	r.mu.Unlock()
	return r.inner.Stream(ctx, req)
}

func (r *recorderProvider) at(i int) provider.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.reqs) {
		return provider.Request{}
	}
	return r.reqs[i]
}

func (r *recorderProvider) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func cloneRequest(req provider.Request) provider.Request {
	msgs := make([]message.Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = m.Clone()
	}
	req.Messages = msgs
	return req
}

// memStore 记录落盘事件。
type memStore struct {
	mu  sync.Mutex
	got []loop.Kind
}

func (s *memStore) Append(_ context.Context, _ string, ev loop.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev.Kind)
	return nil
}

func (s *memStore) has(k loop.Kind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.got {
		if g == k {
			return true
		}
	}
	return false
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func scope() ctxx.Scope { return ctxx.Scope{UserID: "u1", SessionID: "s1"} }

func rendered(msgs []message.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role)+":"+m.Text())
	}
	return out
}

// ---------- 用 Test 建会话的辅助 ----------

func newTestSession(t *testing.T, cfg Config) (*Session, *recorderProvider) {
	t.Helper()
	rp := &recorderProvider{inner: cfg.Provider}
	cfg.Provider = rp
	if cfg.Logger == nil {
		cfg.Logger = quiet()
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.NewSession(scope())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, rp
}

// ---------- 主线：Input → 组装 → Run → 结算 ----------

// 第二轮能看到第一轮的问答：窗口把历史带进下一次 Run。
func TestSessionCarriesHistoryAcrossInputs(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"你好"}}, provider.FakeStep{Text: []string{"再见"}}),
		Compressor: window.KeepLast(100),
	})

	if _, err := s.Input(context.Background(), message.NewUser("第一句")); err != nil {
		t.Fatalf("input 1: %v", err)
	}
	if _, err := s.Input(context.Background(), message.NewUser("第二句")); err != nil {
		t.Fatalf("input 2: %v", err)
	}

	// 第二次请求：窗口组装的完整历史 = 第一句 + 回答 + 第二句
	got := rendered(rp.at(1).Messages)
	want := []string{"user:第一句", "assistant:你好", "user:第二句"}
	if len(got) != len(want) {
		t.Fatalf("history: want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history: want %v got %v", want, got)
		}
	}
}

// 工具往返进历史：同一 Run 的第二次请求能看到 tool 消息。
func TestSessionToolResultEntersHistory(t *testing.T) {
	echo := tool.NewFake("echo", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: `{"ok":true}`}},
	}, 0)
	s, rp := newTestSession(t, Config{
		Provider: provider.NewFake(
			provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "echo", Args: []byte(`{}`)}}},
			provider.FakeStep{Text: []string{"查到了"}},
			provider.FakeStep{Text: []string{"不客气"}},
		),
		Tools:      []tool.Tool{echo},
		Compressor: window.KeepLast(100),
	})

	res, err := s.Input(context.Background(), message.NewUser("查一下"))
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if res.Turns != 2 || res.EndReason != loop.EndDone {
		t.Fatalf("run: %+v", res)
	}

	got := rendered(rp.at(1).Messages)
	if len(got) != 3 || got[0] != "user:查一下" || got[2] != "tool:{\"ok\":true}" {
		t.Fatalf("second request messages: %v", got)
	}
	if n, _, _ := echo.Calls(); n != 1 {
		t.Fatalf("tool calls: %d", n)
	}

	// 结算后下一轮仍能看到完整的工具往返
	if _, err := s.Input(context.Background(), message.NewUser("谢谢")); err != nil {
		t.Fatal(err)
	}
	third := rendered(rp.at(2).Messages)
	if len(third) != 5 {
		t.Fatalf("history after tool round: %v", third)
	}
}

// durable 事件落盘；volatile 不落盘。
func TestSessionPersistsDurableEvents(t *testing.T) {
	st := &memStore{}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"嗨"}}),
		Store:      st,
		Compressor: window.KeepLast(100),
	})

	if _, err := s.Input(context.Background(), message.NewUser("你好")); err != nil {
		t.Fatal(err)
	}
	// 落盘是独立订阅者，可能略滞后于 Input 返回
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.has(loop.KindAgentEnd) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, k := range []loop.Kind{loop.KindAgentStart, loop.KindMessageEnd, loop.KindAgentEnd} {
		if !st.has(k) {
			t.Fatalf("durable %s not persisted (%v)", k, st.got)
		}
	}
	if st.has(loop.KindMessageUpdate) {
		t.Fatal("volatile event must not be persisted")
	}
}

// 并发 Input 被串行化（互斥 + 排队语义，03 §5），不出现 ErrAlreadyRunning。
func TestConcurrentInputsAreSerialized(t *testing.T) {
	steps := make([]provider.FakeStep, 3)
	for i := range steps {
		steps[i] = provider.FakeStep{Text: []string{"ok"}}
	}
	s, rp := newTestSession(t, Config{
		Provider:   provider.NewFake(steps...),
		Compressor: window.KeepLast(100),
	})

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Input(context.Background(), message.NewUser("并发"+string(rune('A'+i))))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	if rp.count() != 3 {
		t.Fatalf("provider calls: %d", rp.count())
	}
	// 最后一轮必须能看到此前的全部输入（三次都进了窗口）
	last := rendered(rp.at(2).Messages)
	var users int
	for _, m := range last {
		if len(m) >= 5 && m[:5] == "user:" {
			users++
		}
	}
	if users != 3 {
		t.Fatalf("serialized history lost inputs: %v", last)
	}
}

// 注入的 Scope 覆盖宿主 ctx（且缺 Scope 时不放行）。
func TestSessionInjectsScopeAndRejectsInvalid(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Compressor: window.KeepLast(100),
	})
	if _, err := s.Input(context.Background(), message.NewUser("无 scope 的 ctx")); err != nil {
		t.Fatalf("session must inject scope: %v", err)
	}
	if rp.count() != 1 {
		t.Fatalf("run did not happen: %d", rp.count())
	}

	a, _ := New(Config{Provider: provider.NewFake(), Logger: quiet()})
	if _, err := a.NewSession(ctxx.Scope{UserID: "u"}); !errors.Is(err, loop.ErrNoScope) {
		t.Fatalf("SessionID missing must be rejected, got %v", err)
	}
}

// 没有 Compressor 时用 KeepLast 兜底：窗口最终有界（压缩在间隙异步收敛）。
func TestDefaultCompressorKeepsWindowBounded(t *testing.T) {
	steps := make([]provider.FakeStep, window.DefaultKeepLast+5)
	for i := range steps {
		steps[i] = provider.FakeStep{Text: []string{"ok"}}
	}
	s, _ := newTestSession(t, Config{Provider: provider.NewFake(steps...)})

	for i := 0; i < len(steps); i++ {
		if _, err := s.Input(context.Background(), message.NewUser("msg")); err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	// 输入连续快过压缩时窗口会短暂超出上限（在途消息必须留在上下文里），
	// 压缩收敛后必须回到界内——这是「有界」的准确含义。
	s.WaitCompress()
	mem, recent := s.History()
	if n := len(mem) + len(recent); n > window.DefaultKeepLast {
		t.Fatalf("window unbounded after compression settled: %d messages", n)
	}
}

func TestNewRequiresProvider(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("nil Provider must be rejected")
	}
}

// ---------- 审查回归（2026-09-11，两份独立审查复现的两个 P0） ----------

// P0：宿主 ctx 取消后下一轮 Input 不得丢失上一轮（轮次错配的永久失忆）。
func TestCancelMidRunKeepsTurnInWindow(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider: provider.NewFake(
			provider.FakeStep{Text: []string{"A", "B", "C", "D", "E"}, ChunkDelay: 5 * time.Millisecond},
			provider.FakeStep{Text: []string{"答2"}},
		),
		Compressor: window.KeepLast(1000),
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(8 * time.Millisecond)
		cancel()
	}()
	// 宿主取消本轮：Run 以 cancelled 收场并返回错误，这是既有语义（02 §6）
	res1, err := s.Input(ctx, message.NewUser("第一句"))
	if res1.EndReason != loop.EndCancelled && err != nil {
		t.Fatalf("input 1: end=%s err=%v", res1.EndReason, err)
	}
	if _, err := s.Input(context.Background(), message.NewUser("第二句")); err != nil {
		t.Fatalf("input 2: %v", err)
	}
	t.Logf("run2 request = %v", rendered(rp.at(1).Messages))

	hist := rendered(rp.at(1).Messages)
	if len(hist) == 0 || hist[0] != "user:第一句" {
		t.Fatalf("previous turn lost after ctx cancel: %v", hist)
	}
	// 第一句之后必须是第一轮的产出（哪怕是被打断的半截），再才是第二句
	last := hist[len(hist)-1]
	if last != "user:第二句" {
		t.Fatalf("turn order broken: %v", hist)
	}
}

// P0：Close 必须唤醒在途 Input，且不因压缩/落盘而挂死。
func TestCloseUnblocksInFlightInput(t *testing.T) {
	g := &gateProvider{started: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestSession(t, Config{Provider: g, Compressor: window.KeepLast(100)})

	inputDone := make(chan error, 1)
	go func() {
		_, err := s.Input(context.Background(), message.NewUser("在途"))
		inputDone <- err
	}()
	<-g.started
	close(g.release)

	closeDone := make(chan struct{})
	go func() { _ = s.Close(); close(closeDone) }()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case <-inputDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Input never returned after Close")
	}
	// 关闭后新输入被拒
	if _, err := s.Input(context.Background(), message.NewUser("x")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("want ErrSessionClosed, got %v", err)
	}
}

// Close 先把已产出的 durable 事件排空再收尾（末尾不丢）。
func TestCloseDrainsPersist(t *testing.T) {
	st := &slowStore{delay: 5 * time.Millisecond}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"一"}}, provider.FakeStep{Text: []string{"二"}}),
		Store:      st,
		Compressor: window.KeepLast(100),
	})
	for _, text := range []string{"a", "b"} {
		if _, err := s.Input(context.Background(), message.NewUser(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := st.count(loop.KindAgentEnd); n != 2 {
		t.Fatalf("drained agent_end: want 2 got %d (all=%v)", n, st.kinds())
	}
}

// 压缩继承本轮的 ctx 身份（Scope/Credentials/Trace 都要在）。
func TestCompressionCtxCarriesTurnIdentity(t *testing.T) {
	probe := &ctxProbeCompressor{}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Compressor: probe,
	})
	host, _ := ctxx.EnsureTrace(ctxx.WithCredentials(context.Background(), ctxx.Credentials{"openai": "sk-test"}))
	if _, err := s.Input(host, message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	s.WaitCompress()

	scope, cred, trace := probe.seen()
	if !scope {
		t.Error("compression ctx lost Scope")
	}
	if !cred {
		t.Error("compression ctx lost Credentials")
	}
	if !trace {
		t.Error("compression ctx lost Trace")
	}
}

// 单次 Input 的 UserID 覆盖会话 UserID（03 §6：Scope.UserID = 当次说话人），
// SessionID 始终由会话锚定。
func TestInputScopeUserIDOverridesButSessionIsAnchored(t *testing.T) {
	sp := &scopeProbeProvider{inner: provider.NewFake(provider.FakeStep{Text: []string{"ok"}})}
	s, _ := newTestSession(t, Config{Provider: sp, Compressor: window.KeepLast(100)})

	host := ctxx.WithScope(context.Background(),
		ctxx.Scope{UserID: "speaker-b", SessionID: "别人的会话"})
	if _, err := s.Input(host, message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	got := sp.lastScope()
	if got.UserID != "speaker-b" {
		t.Fatalf("UserID: want speaker-b got %q", got.UserID)
	}
	if got.SessionID != "s1" {
		t.Fatalf("SessionID must be anchored to session: got %q", got.SessionID)
	}
}

// 宿主 ctx 只带 UserID、不带 SessionID（aria-host Sink 与 aria-demo 的用法）：
// 说话人覆盖照常生效，SessionID 仍由会话锚定。
func TestInputScopeUserIDOverrideWithoutSessionID(t *testing.T) {
	sp := &scopeProbeProvider{inner: provider.NewFake(provider.FakeStep{Text: []string{"ok"}})}
	s, _ := newTestSession(t, Config{Provider: sp, Compressor: window.KeepLast(100)})

	host := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "speaker-c"})
	if _, err := s.Input(host, message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	got := sp.lastScope()
	if got.UserID != "speaker-c" || got.SessionID != "s1" {
		t.Fatalf("want UserID=speaker-c SessionID=s1, got %+v", got)
	}
}

// 落盘失败必须能被宿主看见（而不是静默吞掉），且失败消费者要摘除订阅。
func TestPersistFailureSurfacesViaErr(t *testing.T) {
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Store:      failingStore{},
		Compressor: window.KeepLast(100),
	})
	// 本轮本身是成功的：落盘失败不中断对话（错误经 Err() 暴露）
	if res, err := s.Input(context.Background(), message.NewUser("hi")); err != nil || res.EndReason != loop.EndDone {
		t.Fatalf("turn must still succeed: end=%s err=%v", res.EndReason, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Err() == nil {
		t.Fatal("persist failure must surface via Session.Err()")
	}
	// 会话已降级：后续输入被明确拒绝，而不是无声地失去结算
	if _, err := s.Input(context.Background(), message.NewUser("next")); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("want ErrStreamClosed on degraded session, got %v", err)
	}
	if err := s.Queue(message.NewUser("q")); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Queue on degraded session: %v", err)
	}
}

// 跨包：ProviderCompressor 接进 Session 后，摘要真的成为下一轮的记忆。
func TestProviderCompressorWiredIntoSession(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider: provider.NewFake(
			provider.FakeStep{Text: []string{"第一次回答"}},
			provider.FakeStep{Text: []string{"第二次回答"}},
		),
		Compressor: &window.ProviderCompressor{
			Provider: provider.NewFake(provider.FakeStep{Text: []string{"用户打了招呼"}}),
		},
	})

	if _, err := s.Input(context.Background(), message.NewUser("你好")); err != nil {
		t.Fatal(err)
	}
	s.WaitCompress()
	if _, err := s.Input(context.Background(), message.NewUser("再来")); err != nil {
		t.Fatal(err)
	}

	got := rendered(rp.at(1).Messages)
	if len(got) == 0 || !strings.HasPrefix(got[0], "user:以下是与用户既往对话的摘要") {
		t.Fatalf("summary not in next turn's context: %v", got)
	}
	if !strings.Contains(got[0], "<memory>") || !strings.Contains(got[0], "用户打了招呼") {
		t.Fatalf("summary must be tagged as memory: %v", got)
	}
}

// ---------- 回归用测试替身 ----------

// gateProvider 在 Stream 里等 release；ctx 取消时按 provider 义务收尾部分输出。
type gateProvider struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (g *gateProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 1)
	g.once.Do(func() { close(g.started) })
	go func() {
		defer close(ch)
		select {
		case <-g.release:
			ch <- provider.MessageComplete{Message: message.NewAssistant("done")}
		case <-ctx.Done():
			ch <- provider.MessageComplete{
				Message:     message.Message{Role: message.RoleAssistant, Interrupted: true},
				Interrupted: true,
			}
		}
	}()
	return ch, nil
}

// slowStore 每次 Append 有延迟，用来验证 Close 的排空。
type slowStore struct {
	delay time.Duration
	mu    sync.Mutex
	got   []loop.Kind
}

func (s *slowStore) Append(ctx context.Context, _ string, ev loop.Event) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.got = append(s.got, ev.Kind)
	s.mu.Unlock()
	return nil
}

func (s *slowStore) kinds() []loop.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]loop.Kind(nil), s.got...)
}

func (s *slowStore) count(k loop.Kind) int {
	n := 0
	for _, g := range s.kinds() {
		if g == k {
			n++
		}
	}
	return n
}

type failingStore struct{}

func (failingStore) Append(context.Context, string, loop.Event) error {
	return errors.New("store down")
}

// ctxProbeCompressor 记录压缩 ctx 里有哪些 ctxx 值。
type ctxProbeCompressor struct {
	mu                 sync.Mutex
	scope, cred, trace bool
}

func (p *ctxProbeCompressor) Compress(ctx context.Context, memory, turn []message.Message) ([]message.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, p.scope = ctxx.ScopeFrom(ctx)
	_, p.cred = ctxx.CredentialFrom(ctx, "openai")
	_, p.trace = ctxx.TraceFrom(ctx)
	out := make([]message.Message, 0, len(memory)+len(turn))
	for _, m := range append(append([]message.Message{}, memory...), turn...) {
		out = append(out, m.Clone())
	}
	return out, nil
}

func (p *ctxProbeCompressor) seen() (scope, cred, trace bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scope, p.cred, p.trace
}

// scopeProbeProvider 记录 Run 内看到的 Scope。
type scopeProbeProvider struct {
	inner provider.Provider
	mu    sync.Mutex
	scope ctxx.Scope
}

func (p *scopeProbeProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	if sc, ok := ctxx.ScopeFrom(ctx); ok {
		p.mu.Lock()
		p.scope = sc
		p.mu.Unlock()
	}
	return p.inner.Stream(ctx, req)
}

func (p *scopeProbeProvider) lastScope() ctxx.Scope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scope
}

// 高强度交错回归：反复「流中途取消 → 立刻续两轮」，窗口永不丢输入、
// 不重复、轮次不错配（审查者原始复现的强化版）。
func TestCancelInterleaveNeverLosesHistory(t *testing.T) {
	const iters = 60
	for iter := 0; iter < iters; iter++ {
		steps := make([]provider.FakeStep, 4)
		for i := range steps {
			steps[i] = provider.FakeStep{Text: []string{"答"}, ChunkDelay: 2 * time.Millisecond}
		}
		s, rp := newTestSession(t, Config{
			Provider:   provider.NewFake(steps...),
			Compressor: window.KeepLast(1000),
		})

		ctx, cancel := context.WithCancel(context.Background())
		in1 := make(chan struct{})
		go func() {
			defer close(in1)
			_, _ = s.Input(ctx, message.NewUser("第一句"))
		}()
		for rp.count() == 0 { // 等第一次 Stream 真的开始
			time.Sleep(200 * time.Microsecond)
		}
		cancel()
		<-in1

		if _, err := s.Input(context.Background(), message.NewUser("第二句")); err != nil {
			t.Fatalf("iter %d: input2: %v", iter, err)
		}
		s.WaitCompress()

		all := func() []string {
			mem, recent := s.History()
			return rendered(append(mem, recent...))
		}()
		if countText(all, "第一句") != 1 {
			t.Fatalf("iter %d: 第一句 must appear exactly once, window=%v", iter, all)
		}
		if countText(all, "第二句") != 1 {
			t.Fatalf("iter %d: 第二句 must appear exactly once, window=%v", iter, all)
		}
		if _, err := s.Input(context.Background(), message.NewUser("第三句")); err != nil {
			t.Fatalf("iter %d: input3: %v", iter, err)
		}
		third := rendered(rp.at(rp.count() - 1).Messages)
		if countText(third, "第二句") != 1 || countText(third, "第一句") != 1 {
			t.Fatalf("iter %d: history broken at run3: %v", iter, third)
		}
		_ = s.Close()
	}
}

func countText(msgs []string, want string) int {
	n := 0
	for _, m := range msgs {
		if m == "user:"+want || m == want {
			n++
		}
	}
	return n
}

// ---------- 第二轮审查（D1–D5）回归 ----------

// D1：Close 的 Interrupt 可能被 core.Run 入口的 interrupt.Store(false) 抹掉，
// 此时在途轮的终止必须由「会话 ctx 取消」保证（不能靠 Interrupt）。
// 确定性构造：持 s.mu 让 Input 停在 closed 检查之后、Run 之前，
// 让 Close 的 Interrupt 先发出，再由 Run 入口抹掉。
func TestCloseTerminatesRunEvenIfInterruptWiped(t *testing.T) {
	p := &cancelOnlyProvider{entered: make(chan struct{})}
	s, _ := newTestSession(t, Config{Provider: p, Compressor: window.KeepLast(100)})

	s.mu.Lock() // 卡住 beginTurn
	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = s.Input(context.Background(), message.NewUser("x"))
	}()
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() { _ = s.Close(); close(closeDone) }()
	time.Sleep(50 * time.Millisecond)
	s.mu.Unlock() // Input 继续 → Run 入口抹掉 Interrupt → 只有会话 ctx 取消能救

	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 死锁：Interrupt 被 Run 入口抹掉且无 ctx 取消兜底")
	}
	select {
	case <-inDone:
	case <-time.After(2 * time.Second):
		t.Fatal("在途 Input 未退出")
	}
}

// D2：Settle 之后立刻 Close —— Close 必须能取消这次压缩（不挂死）。
// 刻意在单核下高频采样，命中「cancel 尚未注册」的窗口。
func TestCloseCancelsFreshCompression(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	for i := 0; i < 30; i++ {
		c := &ctxBlockingCompressorAgent{entered: make(chan struct{})}
		s, _ := newTestSession(t, Config{
			Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
			Compressor: c,
		})
		if _, err := s.Input(context.Background(), message.NewUser("x")); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = s.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: Close 未被在途压缩唤醒", i)
		}
	}
}

// D3：Store.Append 阻塞（但尊重 ctx）时，Close 必须在 CloseGrace 内有界返回。
func TestCloseBoundedWhenStoreBlocks(t *testing.T) {
	st := &ctxBlockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Store:      st,
		Compressor: window.KeepLast(100),
		CloseGrace: 200 * time.Millisecond,
	})
	if _, err := s.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	<-st.entered

	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(st.release)
		t.Fatal("Close 被阻塞的 Store 拖死（应受 CloseGrace 约束）")
	}
	close(st.release)
}

// D3 补充：完全无视 ctx 的 Store 也不能把 Close 拖死（放弃等待，记 warn）。
func TestCloseDoesNotHangOnCtxIgnoringStore(t *testing.T) {
	st := &ctxIgnoringStore{entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Store:      st,
		Compressor: window.KeepLast(100),
		CloseGrace: 150 * time.Millisecond,
	})
	if _, err := s.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	<-st.entered

	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(st.release)
		t.Fatal("Close 被无视 ctx 的 Store 拖死")
	}
	close(st.release)
}

// D4：落盘订阅被总线断开（channel 关闭但会话未关闭）必须被看见：Err() 上报
// 且等待结算的路径不再永久阻塞（dead 通道）。
func TestUnexpectedStreamCloseSurfaces(t *testing.T) {
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Compressor: window.KeepLast(100),
	})
	if _, err := s.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	// 模拟总线把窗口订阅者断开：关掉它的事件流（测试内直接触发 fail 路径）。
	s.fail(ErrStreamClosed)

	select {
	case <-s.dead:
	case <-time.After(time.Second):
		t.Fatal("dead 通道未关闭：在途 Input 会永久等待")
	}
	if !errors.Is(s.Err(), ErrStreamClosed) {
		t.Fatalf("Err: %v", s.Err())
	}
}

// D5：会话关闭后 Queue 返回错误（不再静默丢弃）。
func TestQueueAfterCloseReturnsError(t *testing.T) {
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
		Compressor: window.KeepLast(100),
	})
	if _, err := s.Input(context.Background(), message.NewUser("a")); err != nil {
		t.Fatal(err)
	}
	// 空闲（本轮已结算）：Queue 被拒——空闲入队会被下一次 Run 排在它的输入之后，
	// 造成「后说的在前」（2026-09-14 审查）。
	if err := s.Queue(message.NewUser("b")); !errors.Is(err, ErrNoActiveRun) {
		t.Fatalf("idle Queue want ErrNoActiveRun, got %v", err)
	}
	_ = s.Close()
	if err := s.Queue(message.NewUser("c")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("want ErrSessionClosed, got %v", err)
	}
}

// 运行中 Queue 可用（steering 语义）：注入的消息排在当前轮输入之后，
// 顺序与到达顺序一致。
func TestQueueInjectsDuringRun(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	prov := &gatedProvider{gate: gate, entered: entered, reply: "第一轮答"}
	s, rp := newTestSession(t, Config{
		Provider:   prov,
		Compressor: window.KeepLast(100),
	})
	_ = rp

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Input(context.Background(), message.NewUser("先输入的X"))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("首轮未开始")
	}

	if err := s.Queue(message.NewUser("后注入的Y")); err != nil {
		t.Fatalf("运行中 Queue 应被接受：%v", err)
	}
	close(gate)
	<-done
	s.WaitCompress()

	// 窗口历史里 X 在 Y 之前。
	mem, recent := s.History()
	all := append(append([]message.Message{}, mem...), recent...)
	var order []string
	for _, m := range all {
		if t := strings.TrimSpace(m.Text()); t != "" {
			order = append(order, t)
		}
	}
	joined := strings.Join(order, "|")
	if xi, yi := strings.Index(joined, "先输入的X"), strings.Index(joined, "后注入的Y"); xi < 0 || yi < 0 || xi > yi {
		t.Fatalf("顺序错误：%v", order)
	}
}

// 回归（2026-09-28）：模型循环结束、等待历史结算的窗口里（Input 仍持会话锁），
// Queue 必须拒绝——旧实现以 runMu.TryLock 推断「在途」，此窗口内误收，消息
// 滞留到下一次 Input 且排到其输入之后（时序颠倒随记忆长留）。
func TestQueueRejectedDuringSettlementWait(t *testing.T) {
	prov := provider.NewFake(provider.FakeStep{
		Text: []string{"a", "b", "c", "d", "e"}, ChunkDelay: 25 * time.Millisecond,
	})
	s, _ := newTestSession(t, Config{Provider: prov, Compressor: window.KeepLast(100)})
	ch, unsub := s.Subscribe(64)
	defer unsub()

	done := make(chan error, 1)
	go func() {
		_, err := s.Input(context.Background(), message.NewUser("hi"))
		done <- err
	}()

	// 等 beginTurn 完成（runEnd 已置位）再锁 s.mu：consumeWindow 处理 durable
	// 事件（appendTurn/settle）都要拿 s.mu，锁住它即卡住结算链，Input 停在
	// 「等本轮进窗口」段。趁流还在输出时拿锁，不会与 beginTurn 死锁。
	for i := 0; ; i++ {
		s.mu.Lock()
		if s.runEnd != nil {
			break
		}
		s.mu.Unlock()
		if i > 100000 {
			t.Fatal("轮次未开始")
		}
		runtime.Gosched()
	}

	sawEnd := make(chan struct{})
	go func() {
		for ev := range ch {
			if _, ok := ev.Data.(loop.AgentEndData); ok {
				close(sawEnd)
				return
			}
		}
	}()
	select {
	case <-sawEnd: // 收敛已判定（入队口已封），Run 即将/已经返回
	case <-time.After(3 * time.Second):
		s.mu.Unlock()
		t.Fatal("未见 AgentEnd")
	}

	if err := s.Queue(message.NewUser("迟到的")); !errors.Is(err, ErrNoActiveRun) {
		s.mu.Unlock()
		t.Fatalf("结算等待期 Queue 应拒绝，got %v", err)
	}
	s.mu.Unlock() // 放行结算链

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Input 未随结算放行返回")
	}
}

// gatedProvider 首次调用阻塞在 gate 上（模拟慢速模型），便于在运行中注入。
type gatedProvider struct {
	gate    chan struct{}
	entered chan struct{}
	reply   string
	once    sync.Once
}

func (g *gatedProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.StreamEvent, error) {
	g.once.Do(func() { g.entered <- struct{}{} })
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		select {
		case <-g.gate:
		case <-ctx.Done():
			ch <- provider.MessageComplete{Message: message.Message{Role: message.RoleAssistant}, Interrupted: true}
			return
		}
		ch <- provider.PartDelta{Text: g.reply}
		ch <- provider.MessageComplete{Message: message.Message{
			Role:   message.RoleAssistant,
			Blocks: []message.Block{message.TextBlock{Text: g.reply}},
		}}
	}()
	return ch, nil
}

// ---------- 第二轮回归用测试替身 ----------

// cancelOnlyProvider 只在 ctx 取消时收尾（契约合规）。
type cancelOnlyProvider struct {
	once    sync.Once
	entered chan struct{}
}

func (p *cancelOnlyProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.StreamEvent, error) {
	p.once.Do(func() { close(p.entered) })
	ch := make(chan provider.StreamEvent, 1)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- provider.MessageComplete{
			Message:     message.Message{Role: message.RoleAssistant, Interrupted: true},
			Interrupted: true,
		}
	}()
	return ch, nil
}

// ctxBlockingStore 尊重 ctx：Append 阻塞到 ctx 取消或放行。
type ctxBlockingStore struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *ctxBlockingStore) Append(ctx context.Context, _ string, _ loop.Event) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

// ctxIgnoringStore 无视 ctx：永远阻塞到放行。
type ctxIgnoringStore struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *ctxIgnoringStore) Append(context.Context, string, loop.Event) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

// ctxBlockingCompressorAgent 阻塞到 ctx 取消（尊重 ctx），用于验证 Close 能取消压缩。
type ctxBlockingCompressorAgent struct {
	once    sync.Once
	entered chan struct{}
}

func (c *ctxBlockingCompressorAgent) Compress(ctx context.Context, _, _ []message.Message) ([]message.Message, error) {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// ---------- 工具结果截断 + artifact 读回（03 §1 artifact+ref） ----------

func refFromHint(t *testing.T, hint string) string {
	t.Helper()
	const marker = `ref="`
	i := strings.Index(hint, marker)
	if i < 0 {
		t.Fatalf("no ref in hint: %q", hint)
	}
	rest := hint[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated ref: %q", hint)
	}
	return rest[:j]
}

func toolMessageText(msgs []message.Message) string {
	for _, m := range msgs {
		if m.Role == message.RoleTool {
			return m.Text()
		}
	}
	return ""
}

// 超长工具结果只把「提示 + 预览」喂回模型，全文可经 artifact.open 读回。
func TestLongToolResultTruncatedAndReadable(t *testing.T) {
	const total = 9000
	full := strings.Repeat("0123456789", total/10) // 9000 字符
	big := tool.NewFake("big", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: full}},
	}, 0)

	rp := &recorderProvider{inner: provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "big", Args: []byte(`{}`)}}},
		provider.FakeStep{Text: []string{"收到"}},
	)}
	a, err := New(Config{
		Provider:        rp,
		Tools:           []tool.Tool{big},
		ToolResultLimit: 4000,
		Compressor:      window.KeepLast(1000),
		Logger:          quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.NewSession(scope())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Input(context.Background(), message.NewUser("看看")); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 启用截断时读取工具必须已注册给模型
	var sawOpen bool
	for _, d := range rp.at(0).Tools {
		if d.Name == artifact.OpenToolName {
			sawOpen = true
		}
	}
	if !sawOpen {
		t.Fatal("artifact.open must be registered when truncation is enabled")
	}

	// 第二轮请求里的 tool 消息必须是截断形态
	second := rp.at(1).Messages
	toolText := toolMessageText(second)
	if toolText == "" {
		t.Fatalf("tool message missing: %v", rendered(second))
	}
	if n := len([]rune(toolText)); n > 3000 {
		t.Fatalf("tool result not truncated: %d runes", n)
	}
	if !strings.Contains(toolText, "输出过长已截断") || !strings.Contains(toolText, artifact.OpenToolName) {
		t.Fatalf("truncation notice missing: %q", toolText)
	}
	ref := refFromHint(t, toolText)

	// 第二个会话（同一 Agent → 同一 artifact 存储）按提示读回后续内容
	s2, err := a.NewSession(scope())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	reader := artifact.OpenTool(a.Artifacts())
	got := reader.Exec(context.Background(), tool.Call{
		ID:   "c9",
		Name: artifact.OpenToolName,
		Args: []byte(`{"ref":"` + ref + `","offset":4000,"limit":2000}`),
	})
	if got.IsError {
		t.Fatalf("artifact.open failed: %+v", got.Blocks)
	}
	body, _ := got.Blocks[0].(message.TextBlock)
	if !strings.Contains(body.Text, full[4000:4200]) {
		t.Fatalf("wrong slice: %q", body.Text[:120])
	}
}

// 短结果不受影响；未超限时 artifact 存储保持为空。
func TestShortToolResultUntouched(t *testing.T) {
	echo := tool.NewFake("echo", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: "短"}},
	}, 0)
	rp := &recorderProvider{inner: provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "echo", Args: []byte(`{}`)}}},
		provider.FakeStep{Text: []string{"好"}},
	)}
	a, err := New(Config{Provider: rp, Tools: []tool.Tool{echo}, Compressor: window.KeepLast(10), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Artifacts().(*artifact.Memory).Len() != 0 {
		t.Fatal("store must start empty")
	}
	s, _ := a.NewSession(scope())
	defer func() { _ = s.Close() }()
	if _, err := s.Input(context.Background(), message.NewUser("x")); err != nil {
		t.Fatal(err)
	}
	if got := toolMessageText(rp.at(1).Messages); got != "短" {
		t.Fatalf("short result must pass through: %q", got)
	}
	if a.Artifacts().(*artifact.Memory).Len() != 0 {
		t.Fatal("short result must not be stored")
	}
}

// 关闭截断（负数）时不注册 artifact.open，也不包装工具。
func TestTruncationDisabled(t *testing.T) {
	big := tool.NewFake("big", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: strings.Repeat("x", 9000)}},
	}, 0)
	rp := &recorderProvider{inner: provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "big", Args: []byte(`{}`)}}},
		provider.FakeStep{Text: []string{"好"}},
	)}
	a, err := New(Config{
		Provider: rp, Tools: []tool.Tool{big},
		ToolResultLimit: -1, Compressor: window.KeepLast(10), Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rp.at(0).Tools {
		if d.Name == artifact.OpenToolName {
			t.Fatal("artifact.open must not be registered when truncation is disabled")
		}
	}
	s, _ := a.NewSession(scope())
	defer func() { _ = s.Close() }()
	if _, err := s.Input(context.Background(), message.NewUser("x")); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(toolMessageText(rp.at(1).Messages))); n != 9000 {
		t.Fatalf("truncation disabled: want 9000 runes got %d", n)
	}
}

// countingCompressor 记录被调次数（热切换测试用）。
type countingCompressor struct {
	mu sync.Mutex
	n  int
}

func (c *countingCompressor) Compress(_ context.Context, memory, turn []message.Message) ([]message.Message, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	out := make([]message.Message, 0, len(memory)+len(turn))
	for _, m := range append(append([]message.Message{}, memory...), turn...) {
		out = append(out, m.Clone())
	}
	return out, nil
}

func (c *countingCompressor) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// 会话级热切换压缩策略：SetCompressor 之后下一轮的间隙压缩用新实现。
func TestSessionSetCompressorHotSwap(t *testing.T) {
	first := &countingCompressor{}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"第一轮"}}, provider.FakeStep{Text: []string{"第二轮"}}),
		Compressor: first,
	})
	if _, err := s.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	s.WaitCompress()
	if first.calls() == 0 {
		t.Fatal("首次压缩没走旧实现")
	}

	second := &countingCompressor{}
	s.SetCompressor(second)
	if _, err := s.Input(context.Background(), message.NewUser("再来")); err != nil {
		t.Fatal(err)
	}
	s.WaitCompress()

	if second.calls() == 0 {
		t.Fatalf("热切换后新实现未被使用（旧 %d 次 / 新 %d 次）", first.calls(), second.calls())
	}
}

// SetCompressor 与在途轮并发（热切换不会打乱会话）。
func TestSetCompressorWhileTurnInFlight(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	s, _ := newTestSession(t, Config{
		Provider:   &gatedProvider{gate: gate, entered: entered, reply: "答"},
		Compressor: window.KeepLast(100),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Input(context.Background(), message.NewUser("hi"))
	}()
	<-entered
	s.SetCompressor(window.KeepLast(5))
	close(gate)
	<-done
	s.WaitCompress()
}

// ---------- Stopper × 工具装饰器 ----------

// agentStopTool 是收尾工具（core/tool.Stopper）。
type agentStopTool struct{}

func (agentStopTool) Def() tool.Def { return tool.Def{Name: "stop"} }
func (agentStopTool) Exec(_ context.Context, call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID,
		Blocks: []message.Block{message.TextBlock{Text: "本轮已收尾"}}}
}
func (agentStopTool) StopsLoop() bool { return true }

// 回归（真机 2026-09-29 18:48：stop 执行成功、run 却续轮又说了两段）：
// buildTools 默认给所有工具套 toolkit.Truncate 装饰器，loop 若对包装器
// 直接断言 Stopper 就会漏判——收尾能力必须沿 Unwrap 链透出。
func TestStopperSurvivesToolWrapping(t *testing.T) {
	fake := provider.NewFake(
		provider.FakeStep{Calls: []message.ToolCall{{ID: "c1", Name: "stop", Args: []byte(`{}`)}}},
		provider.FakeStep{Text: []string{"不该有这段"}},
	)
	s, _ := newTestSession(t, Config{
		Provider:   fake,
		Tools:      []tool.Tool{agentStopTool{}},
		Compressor: window.KeepLast(100),
	})
	res, err := s.Input(context.Background(), message.NewUser("说完了"))
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if res.EndReason != loop.EndDone || res.Turns != 1 {
		t.Fatalf("stop 应在工具轮后收敛为 EndDone（turns=1）：end=%s turns=%d", res.EndReason, res.Turns)
	}
	if left := fake.Left(); left != 1 {
		t.Fatalf("stop 后不应再调 provider：剩 %d 段脚本（want 1）", left)
	}
}

// 间隙压缩产生 window_compressed durable 事件（异步，轮询等它落盘）。
func TestWindowCompressedEventPersisted(t *testing.T) {
	st := &memStore{}
	s, _ := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"好"}}),
		Store:      st,
		Compressor: window.KeepLast(10),
	})
	if _, err := s.Input(context.Background(), message.NewUser("你好")); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.hasKind(loop.KindWindowCompressed) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("window_compressed 事件未落盘")
}

func (s *memStore) hasKind(k loop.Kind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.got {
		if got == k {
			return true
		}
	}
	return false
}
