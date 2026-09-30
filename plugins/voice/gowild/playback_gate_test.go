package gowild

import (
	"testing"
	"time"
)

type inputCapture struct{ count int }

func (s *inputCapture) Deliver(string, string) error { s.count++; return nil }

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
	if sink.count != 2 || !busy.Active() {
		t.Fatalf("count=%d busy=%v", sink.count, busy.Active())
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
