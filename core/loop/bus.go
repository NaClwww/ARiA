package loop

import (
	"sync"
	"sync/atomic"
	"time"
)

// 总线（docs/02 §3.2）：emit 在飞轮 goroutine 内 O(1) 非阻塞。
// 每订阅者一个有界出站 channel + 一个投递协程。
//   - volatile 满则丢弃（MessageEnd 永远带全文，丢增量无损）；
//   - durable 满则暂存 overflow，由投递协程背压推送；订阅者停滞超过
//     stallTimeout 判死断开——durable 的「不丢」以订阅者存活为界。
//
// 飞轮状态的零锁纪律不适用于本文件的同步原语——复杂度关死在 bus.go。
const (
	defaultBuffer = 256
	stallTimeout  = 5 * time.Second
	overflowCap   = 4096 // 防御上限：溢出即断开，不无界占内存
)

type sub struct {
	ch   chan Event
	kick chan struct{} // emit 溢出写入 overflow 后的唤醒信号
	quit chan struct{}
	dead atomic.Bool

	mu       sync.Mutex
	overflow []Event
}

type bus struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

func newBus() *bus { return &bus{subs: make(map[*sub]struct{})} }

// add 注册订阅者并启动投递协程。取消函数只解除订阅，不 close 出站
// channel（避免与 emit 竞态；dead 之后 emit 跳过该订阅者）。
func (b *bus) add(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = defaultBuffer
	}
	s := &sub{ch: make(chan Event, buf), kick: make(chan struct{}, 1), quit: make(chan struct{})}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go b.deliver(s)
	return s.ch, func() { b.disconnect(s) }
}

func (b *bus) emit(ev Event) {
	b.mu.Lock()
	subs := make([]*sub, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		if s.dead.Load() {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			if !ev.Kind.Durable() {
				continue // volatile：丢增量无损
			}
			s.mu.Lock()
			if len(s.overflow) < overflowCap {
				s.overflow = append(s.overflow, ev)
				s.mu.Unlock()
				select {
				case s.kick <- struct{}{}:
				default:
				}
			} else {
				s.mu.Unlock()
				b.disconnect(s)
			}
		}
	}
}

// deliver 先倾倒 overflow，再等下一次溢出信号。
func (b *bus) deliver(s *sub) {
	for {
		for {
			s.mu.Lock()
			if len(s.overflow) == 0 {
				s.mu.Unlock()
				break
			}
			ev := s.overflow[0]
			s.overflow = s.overflow[1:]
			s.mu.Unlock()
			if !b.push(s, ev) {
				return
			}
		}
		select {
		case <-s.kick:
		case <-s.quit:
			return
		}
	}
}

// push 带背压地投递一条：满则等待，停滞超阈值判死断开。
func (b *bus) push(s *sub, ev Event) bool {
	select {
	case s.ch <- ev:
		return true
	default:
	}
	timer := time.NewTimer(stallTimeout)
	defer timer.Stop()
	select {
	case s.ch <- ev:
		return true
	case <-s.quit:
		return false
	case <-timer.C:
		b.disconnect(s)
		return false
	}
}

func (b *bus) disconnect(s *sub) {
	if s.dead.CompareAndSwap(false, true) {
		close(s.quit)
	}
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}
