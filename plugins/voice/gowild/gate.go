package gowild

import "sync"

// GateState 是闸门的完整能力：份额（Acquire/Release）+ 状态（Active）+
// 迁移订阅（Observe）。TTS 只消费份额（Gate）；宿主的输入纪律与 Light
// 消费状态与迁移。
type GateState interface {
	Gate
	Active() bool
	// Observe 订阅迁移：计数每次跨越 0↔1 时回调一次，回调在锁外同步调用，只作唤醒信号；
	// 并发的 Acquire/Release 之间回调顺序与计数变化顺序可能不同，观察者以 Active() 读取
	// 当前状态。返回退订函数。
	Observe(fn func()) (cancel func())
}

// NewGate 建引用计数闸门——半双工策略的锚点：轮次份额（输入被接收→
// 结算）、生成份额（AgentStart→AgentEnd）、播放份额（TTS 请求发出→设备
// 放完）同池计数，全释放才放行输入。顺带挡自回声：播放期间 mic 收到的
// 「设备自己的声音」不再成轮。
//
// 状态灯的 thinking 态直接订阅迁移——闸门即「她正忙着说」这条总线的
// 事件源，宿主不再各自轮询。
type gate struct {
	mu   sync.Mutex
	n    int
	obs  map[int]func()
	next int
}

func NewGate() GateState {
	return &gate{obs: make(map[int]func())}
}

func (g *gate) Acquire() {
	g.mu.Lock()
	g.n++
	crossed := g.n == 1
	g.mu.Unlock()
	if crossed {
		g.notify()
	}
}

func (g *gate) Release() {
	g.mu.Lock()
	if g.n == 0 { // 无对应 Acquire：计数不变，也不发出迁移通知
		g.mu.Unlock()
		return
	}
	g.n--
	idle := g.n == 0
	g.mu.Unlock()
	if idle {
		g.notify()
	}
}

func (g *gate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n > 0
}

func (g *gate) Observe(fn func()) (cancel func()) {
	g.mu.Lock()
	id := g.next
	g.next++
	g.obs[id] = fn
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		delete(g.obs, id)
		g.mu.Unlock()
	}
}

// notify 在锁外快照回调后逐个调用：回调里再碰闸门（如 Active）不会死锁。
func (g *gate) notify() {
	g.mu.Lock()
	fns := make([]func(), 0, len(g.obs))
	for _, fn := range g.obs {
		fns = append(fns, fn)
	}
	g.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
