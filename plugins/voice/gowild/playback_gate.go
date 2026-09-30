package gowild

import "sync"

// playbackGateSink wraps the real output, inside any FIFO prefetch buffer.
// Holding starts before the first PCM write and ends after drain or stop.
// Synthesis, empty responses and queued audio do not close the input gate.
type playbackGateSink struct {
	audioSink
	gate    Gate
	mu      sync.Mutex
	held    bool
	stopped bool
}

func (s *playbackGateSink) write(p []byte) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errHoldStopped
	}
	if len(p) > 0 && !s.held {
		s.held = true
		s.gate.Acquire()
	}
	s.mu.Unlock()
	return s.audioSink.write(p)
}

func (s *playbackGateSink) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.held {
		s.held = false
		s.gate.Release()
	}
}

func (s *playbackGateSink) stop() {
	// Prevent a concurrent write from acquiring a new share after stop.
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.audioSink.stop()
	s.release()
}

func (s *playbackGateSink) waitDrain() {
	s.audioSink.waitDrain()
	s.release()
}
