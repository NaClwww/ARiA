// Package app 是宿主外设的挂载与生命周期容器（docs/06 §5）。装配纪律
// 不变——依赖仍是类型化构造注入，本容器只收编「订阅/goroutine/退订/等待/
// 退出钩子」这套每个外设都要抄一遍的样板，main 退化成一份挂载清单。
//
// 顺序纪律（执行顺序是本容器存在的理由，别破坏）：
//
//   - 挂载即生效：Mount 的订阅在调用返回前已注册进总线（Service 的
//     goroutine 也已起跑）——下一行挂载开始时，上一个消费者已在位。
//     goroutine 首跑顺序无保证也不需要：事件在通道里缓冲；
//   - 事件消费者先挂、输入插头最后挂——事件不重放，晚订阅者错过即错过，
//     输入开始流动前所有消费者必须已订阅；
//   - 收尾严格逆序（单链 LIFO，三类挂载 interleaved 按注册序）：后挂的
//     先收。于是输入插头先停（不再进新输入），消费者后排干（已有的事件
//     处理完），最先注册的 OnShutdown 最后跑——会话/引擎关闭挂链底。
//
// 本包只依赖 core/loop 与标准库；不认识任何具体插件。
package app

import (
	"context"
	"log/slog"
	"sync"

	"aria/core/loop"
)

// EventSource 是事件流源（agent.Session 结构化满足）；Mount 用它订阅。
type EventSource interface {
	Subscribe(buf int) (<-chan loop.Event, func())
}

type App struct {
	src EventSource
	log *slog.Logger

	mu   sync.Mutex
	down []hook // 收尾链：注册序存储，Run 时逆序执行
}

type hook struct {
	name string
	fn   func()
}

func New(src EventSource, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{src: src, log: log}
}

// Mount 挂事件流消费者：立即订阅（buf<=0 → 引擎默认 256）并起 goroutine
// 跑 fn。收尾 = 退订 + 等 fn 返回（退订后通道排空关闭，fn 应随 range
// 退出——把「通道关闭即退出」写进消费者契约）。
func (a *App) Mount(name string, buf int, fn func(<-chan loop.Event)) {
	ch, unsub := a.src.Subscribe(buf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ch)
	}()
	a.push(name, func() {
		unsub()
		<-done
	})
}

// Service 挂后台服务（不读事件流的插头/循环：ASR 重连、stdin……）：
// goroutine 跑 fn(独立 ctx)。收尾 = 取消 ctx + 等 fn 返回——fn 必须尊重
// ctx 取消。
func (a *App) Service(name string, fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	a.push(name, func() {
		cancel()
		<-done
	})
}

// OnShutdown 注册纯收尾钩子（无启动动作）。注册得越早、收尾跑得越晚——
// 会话/引擎这类「链底」关闭要在任何 Mount/Service 之前注册。name 只进
// 收尾日志，让逆序链看得见是谁。
func (a *App) OnShutdown(name string, fn func()) {
	a.push(name, fn)
}

func (a *App) push(name string, fn func()) {
	a.mu.Lock()
	a.down = append(a.down, hook{name: name, fn: fn})
	a.mu.Unlock()
}

// Run 阻塞到 quit 关闭，然后逆序执行收尾链（每个钩子 panic 不拖垮后续）。
// 只该调用一次；quit 已关时立即进入收尾。
func (a *App) Run(quit <-chan struct{}) {
	<-quit
	a.mu.Lock()
	chain := a.down
	a.down = nil
	a.mu.Unlock()
	for i := len(chain) - 1; i >= 0; i-- {
		a.safe(chain[i])
	}
}

func (a *App) safe(h hook) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("app: 收尾钩子 panic（继续后续收尾）", "part", h.name, "panic", r)
		}
	}()
	h.fn()
	a.log.Info("app: 已收尾", "part", h.name)
}
