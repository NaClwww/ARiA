package loop

import (
	"sync"
	"time"
)

const (
	defaultBuffer       = 256
	defaultStallTimeout = 5 * time.Second
	overflowCap         = 4096
)

// A subscriber has one FIFO and one goroutine that alone writes and closes ch.
type sub struct {
	ch   chan Event
	kick chan struct{}
	quit chan struct{}
	once sync.Once

	mu      sync.Mutex
	queue   []Event
	closing bool
}

type bus struct {
	mu           sync.Mutex
	subs         map[*sub]struct{}
	stallTimeout time.Duration
}

func newBus() *bus { return newBusWithTimeout(defaultStallTimeout) }

func newBusWithTimeout(timeout time.Duration) *bus {
	return &bus{subs: make(map[*sub]struct{}), stallTimeout: timeout}
}

func (b *bus) add(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = defaultBuffer
	}
	s := &sub{ch: make(chan Event, buf), kick: make(chan struct{}, 1), quit: make(chan struct{})}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go b.deliver(s)
	return s.ch, func() { b.close(s) }
}

func (b *bus) emit(ev Event) {
	b.mu.Lock()
	for s := range b.subs {
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			continue
		}
		if !ev.Kind.Durable() && len(s.queue) >= cap(s.ch) {
			s.mu.Unlock()
			continue
		}
		if len(s.queue) >= overflowCap {
			s.mu.Unlock()
			go b.disconnect(s)
			continue
		}
		s.queue = append(s.queue, cloneEvent(ev))
		s.mu.Unlock()
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *bus) deliver(s *sub) {
	defer close(s.ch)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return
			}
			select {
			case <-s.kick:
				continue
			case <-s.quit:
				return
			}
		}
		ev := s.queue[0]
		s.queue[0] = Event{}
		s.queue = s.queue[1:]
		s.mu.Unlock()

		timer := time.NewTimer(b.stallTimeout)
		select {
		case s.ch <- ev:
			if !timer.Stop() {
				<-timer.C
			}
		case <-s.quit:
			timer.Stop()
			return
		case <-timer.C:
			b.disconnect(s)
			return
		}
	}
}

func (b *bus) close(s *sub) {
	b.mu.Lock()
	delete(b.subs, s)
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	b.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (b *bus) disconnect(s *sub) {
	s.once.Do(func() { close(s.quit) })
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}
