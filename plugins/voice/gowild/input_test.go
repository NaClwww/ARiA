package gowild

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// orderSink 记录 Inject/Start 的调用顺序；Start 阻塞到测试放行，模拟一轮生成与播放。
type orderSink struct {
	mu      sync.Mutex
	inject  bool // Inject 的返回值：true 模拟在途轮次可注入
	log     []string
	started chan string
	release chan struct{}
}

func newOrderSink() *orderSink {
	return &orderSink{started: make(chan string, 8), release: make(chan struct{})}
}

func (s *orderSink) Inject(text, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inject {
		return false, nil
	}
	s.log = append(s.log, "inject:"+text)
	return true, nil
}

func (s *orderSink) Start(text, _ string) error {
	s.mu.Lock()
	s.log = append(s.log, "start:"+text)
	s.mu.Unlock()
	s.started <- text
	<-s.release
	s.mu.Lock()
	s.log = append(s.log, "end:"+text)
	s.mu.Unlock()
	return nil
}

func (s *orderSink) setInject(v bool) {
	s.mu.Lock()
	s.inject = v
	s.mu.Unlock()
}

func (s *orderSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// deliverReturns 校验 Deliver 在 1 s 内返回（不随 Start 阻塞）。
func deliverReturns(t *testing.T, in *Input, text string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		in.Deliver(text, "user")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Deliver(%q) 阻塞超过 1 s", text)
	}
}

// 起轮期间到达的输入经 Inject 注入在途轮次，Deliver 不阻塞；本轮结算后轮次份额归还。
func TestInputInjectsWhileTurnRunning(t *testing.T) {
	sink := newOrderSink()
	gate := NewGate()
	in, err := NewInput(InputConfig{Sink: sink, Gate: gate, Bypass: true}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	deliverReturns(t, in, "一")
	if got := <-sink.started; got != "一" {
		t.Fatalf("起轮输入：%q", got)
	}
	sink.setInject(true)
	deliverReturns(t, in, "二")
	if !gate.Active() {
		t.Fatal("本轮未结算时轮次份额应仍持有")
	}
	sink.release <- struct{}{}
	waitFor(t, "本轮结算后轮次份额归还", func() bool { return !gate.Active() })
	want := []string{"start:一", "inject:二", "end:一"}
	if got := sink.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("顺序：want %v got %v", want, got)
	}
}

// 无法注入的输入进入待发队列；队列非空时后到的输入即使可注入也排队，按到达顺序依次起轮。
func TestInputPendingKeepsArrivalOrder(t *testing.T) {
	sink := newOrderSink()
	gate := NewGate()
	in, err := NewInput(InputConfig{Sink: sink, Gate: gate, Bypass: true}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	deliverReturns(t, in, "一")
	<-sink.started
	deliverReturns(t, in, "二") // Inject 返回 false：进入待发队列
	sink.setInject(true)
	deliverReturns(t, in, "三") // 待发队列非空：排队，不注入

	sink.release <- struct{}{}
	if got := <-sink.started; got != "二" {
		t.Fatalf("第二轮应以「二」起轮，得到 %q", got)
	}
	sink.release <- struct{}{}
	if got := <-sink.started; got != "三" {
		t.Fatalf("第三轮应以「三」起轮，得到 %q", got)
	}
	sink.release <- struct{}{}
	waitFor(t, "全部结算后轮次份额归还", func() bool { return !gate.Active() })
	want := []string{"start:一", "end:一", "start:二", "end:二", "start:三", "end:三"}
	if got := sink.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("顺序：want %v got %v", want, got)
	}
}
