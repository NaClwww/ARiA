package gowild

import (
	"sync"
	"testing"
	"time"
)

// inputCapture 不接受注入，每次 Start 计数一次（Start 在投递 goroutine 中执行）。
type inputCapture struct {
	mu    sync.Mutex
	count int
}

func (*inputCapture) Inject(string, string) (bool, error) { return false, nil }
func (s *inputCapture) Start(string, string) error {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	return nil
}
func (s *inputCapture) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func TestInputAcceptsDuringLoopButNotPlayback(t *testing.T) {
	busy, playing := NewGate(), NewGate()
	sink := &inputCapture{}
	in, err := NewInput(InputConfig{Sink: sink, Gate: busy, PlaybackGate: playing}, nil)
	if err != nil {
		t.Fatal(err)
	}
	busy.Acquire()
	in.Deliver("thinking", "user")
	playing.Acquire()
	in.Deliver("echo", "user")
	playing.Release()
	in.Deliver("after playback, loop still active", "user")
	waitFor(t, "两条放行的输入起轮", func() bool { return sink.n() == 2 })
	if !busy.Active() {
		t.Fatal("busy gate released by input delivery")
	}
	busy.Release()
}

type drainingSink struct{ drain chan struct{} }

func (*drainingSink) begin() error       { return nil }
func (*drainingSink) write([]byte) error { return nil }
func (*drainingSink) end() error         { return nil }
func (*drainingSink) stop()              {}
func (s *drainingSink) waitDrain()       { <-s.drain }

func TestPlaybackGateExcludesPrefetchAndWaitsForDrain(t *testing.T) {
	g := NewGate()
	raw := &drainingSink{drain: make(chan struct{})}
	out := &playbackGateSink{audioSink: raw, gate: g}
	hold := newHoldSink()
	hold.setFactory(func() audioSink { return out })
	if err := hold.begin(); err != nil {
		t.Fatal(err)
	}
	if err := hold.write([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if g.Active() {
		t.Fatal("prefetch closed input")
	}
	hold.release()
	if !g.Active() {
		t.Fatal("playback did not close input")
	}
	out.end()
	done := make(chan struct{})
	go func() { out.waitDrain(); close(done) }()
	select {
	case <-done:
		t.Fatal("released before device drained")
	case <-time.After(20 * time.Millisecond):
	}
	if !g.Active() {
		t.Fatal("gate released at end instead of drain")
	}
	close(raw.drain)
	<-done
	if g.Active() {
		t.Fatal("gate stuck after drain")
	}
	out.stop()
	if err := out.write([]byte{1, 2}); err == nil || g.Active() {
		t.Fatal("stopped output reacquired gate")
	}
}
