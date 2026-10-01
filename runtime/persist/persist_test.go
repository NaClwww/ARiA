package persist

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aria/core/loop"
	"aria/pkg/ctxx"
)

type fakeStore struct {
	mu     sync.Mutex
	got    []loop.Kind
	failAt int // 第 n 次 Append 失败（1 起算）；0 = 不失败
	n      int
}

func (f *fakeStore) Append(_ context.Context, _ string, ev loop.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.failAt != 0 && f.n == f.failAt {
		return errors.New("store down")
	}
	f.got = append(f.got, ev.Kind)
	return nil
}

func (f *fakeStore) kinds() []loop.Kind {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loop.Kind(nil), f.got...)
}

func ev(k loop.Kind) loop.Event {
	return loop.Event{Kind: k, RunID: "r1", At: time.Now()}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// durable 按序落盘，volatile 一律跳过。
func TestRecorderKeepsDurableOrderAndSkipsVolatile(t *testing.T) {
	st := &fakeStore{}
	r, err := New(st, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan loop.Event, 8)
	ch <- ev(loop.KindAgentStart)
	ch <- ev(loop.KindMessageUpdate) // volatile：丢弃
	ch <- ev(loop.KindMessageEnd)
	ch <- ev(loop.KindProgress) // volatile：丢弃
	ch <- ev(loop.KindAgentEnd)
	close(ch)

	if err := r.Consume(context.Background(), "s1", ch); err != nil {
		t.Fatalf("consume: %v", err)
	}
	want := []loop.Kind{loop.KindAgentStart, loop.KindMessageEnd, loop.KindAgentEnd}
	got := st.kinds()
	if len(got) != len(want) {
		t.Fatalf("kinds: want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: want %v got %v", want, got)
		}
	}
}

// 写失败必须冒泡（durable 承诺破掉不能静默）。
func TestRecorderPropagatesStoreError(t *testing.T) {
	st := &fakeStore{failAt: 2}
	r, _ := New(st, quietLogger())
	ch := make(chan loop.Event, 4)
	ch <- ev(loop.KindAgentStart)
	ch <- ev(loop.KindMessageEnd)
	ch <- ev(loop.KindAgentEnd)
	close(ch)

	err := r.Consume(context.Background(), "s1", ch)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if got := st.kinds(); len(got) != 1 {
		t.Fatalf("must stop at first failure, got %v", got)
	}
}

// ctx 取消终止消费，剩余事件不写。
func TestRecorderStopsOnContextCancel(t *testing.T) {
	st := &fakeStore{}
	r, _ := New(st, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan loop.Event) // 无缓冲：没有新事件时停在 select
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := r.Consume(ctx, "s1", ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRecorderRequiresStore(t *testing.T) {
	if _, err := New(nil, quietLogger()); err == nil {
		t.Fatal("nil Store must be rejected")
	}
}

// sidStore 按「会话标识:事件类型」记录写入的事件。
type sidStore struct {
	mu  sync.Mutex
	got []string
}

func (s *sidStore) Append(_ context.Context, sessionID string, ev loop.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sessionID+":"+string(ev.Kind))
	return nil
}

// 事件记在所属轮次 AgentStart 的 Scope.SessionID 下；首个 AgentStart 之前使用 Consume 的 sessionID。
func TestRecorderFollowsAgentStartSession(t *testing.T) {
	st := &sidStore{}
	r, _ := New(st, quietLogger())
	start := func(sid string) loop.Event {
		return loop.Event{Kind: loop.KindAgentStart, Data: loop.AgentStartData{Scope: ctxx.Scope{SessionID: sid}}}
	}
	ch := make(chan loop.Event, 8)
	ch <- ev(loop.KindMessageEnd)
	ch <- start("s1")
	ch <- ev(loop.KindMessageEnd)
	ch <- ev(loop.KindAgentEnd)
	ch <- start("s2")
	ch <- ev(loop.KindAgentEnd)
	close(ch)
	if err := r.Consume(context.Background(), "s0", ch); err != nil {
		t.Fatal(err)
	}
	want := []string{"s0:message_end", "s1:agent_start", "s1:message_end", "s1:agent_end", "s2:agent_start", "s2:agent_end"}
	if len(st.got) != len(want) {
		t.Fatalf("want %v got %v", want, st.got)
	}
	for i := range want {
		if st.got[i] != want[i] {
			t.Fatalf("want %v got %v", want, st.got)
		}
	}
}
