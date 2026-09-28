package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aria/core/loop"
)

// fakeSrc 模拟事件流源：Subscribe 发缓冲通道，退订关通道（与 core bus 的
// 「排空后关闭」对消费者等价——range 都能干净退出）。
type fakeSrc struct {
	mu      sync.Mutex
	subs    int
	unsubed int
	chans   []chan loop.Event
}

func (f *fakeSrc) Subscribe(int) (<-chan loop.Event, func()) {
	ch := make(chan loop.Event, 8)
	f.mu.Lock()
	f.subs++
	f.chans = append(f.chans, ch)
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		f.unsubed++
		f.mu.Unlock()
		close(ch)
	}
}

func (f *fakeSrc) snapshot() (subs, unsubed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs, f.unsubed
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) rec(s string) {
	r.mu.Lock()
	r.seq = append(r.seq, s)
	r.mu.Unlock()
}

func (r *recorder) has(s string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, x := range r.seq {
			if x == s {
				r.mu.Unlock()
				return true
			}
		}
		r.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seq...)
}

// 启动序 = 挂载序（挂载即起跑，不等 Run）；收尾严格逆序 LIFO。
func TestOrderStartAndTeardown(t *testing.T) {
	rec := &recorder{}
	src := &fakeSrc{}
	a := New(src, quietLog())

	a.OnShutdown("bottom-a", func() { rec.rec("shutdown-a") }) // 链底：最先注册，最后执行
	a.Mount("m1", 0, func(ch <-chan loop.Event) {
		rec.rec("m1-start")
		for range ch {
		}
		rec.rec("m1-done")
	})
	a.Service("s1", func(ctx context.Context) {
		rec.rec("s1-start")
		<-ctx.Done()
		rec.rec("s1-done")
	})
	a.Mount("m2", 0, func(ch <-chan loop.Event) {
		rec.rec("m2-start")
		for range ch {
		}
		rec.rec("m2-done")
	})
	a.OnShutdown("bottom-b", func() { rec.rec("shutdown-b") }) // 最晚注册，最先执行

	// 挂载即生效：不等 Run，三个都已起跑（goroutine 首跑顺序无保证，集合同）
	for _, s := range []string{"m1-start", "s1-start", "m2-start"} {
		if !rec.has(s) {
			t.Fatalf("挂载后未见 %s，序列 %v", s, rec.snapshot())
		}
	}

	quit := make(chan struct{})
	close(quit)
	a.Run(quit)

	// 收尾段顺序是契约：严格逆序 LIFO
	got := rec.snapshot()
	wantTail := []string{"shutdown-b", "m2-done", "s1-done", "m1-done", "shutdown-a"}
	if len(got) != 3+len(wantTail) || !equal(got[3:], wantTail) {
		t.Fatalf("序列不符\n got %v\nwant [三个启动(任意序) %v]", got, wantTail)
	}
	if subs, unsubed := src.snapshot(); subs != 2 || unsubed != 2 {
		t.Fatalf("订阅/退订 %d/%d，应为 2/2", subs, unsubed)
	}
}

// Mount 收尾 = 退订 + 等排干：Run 返回时消费者已把已有事件全处理完。
func TestMountDrainsBeforeRunReturns(t *testing.T) {
	rec := &recorder{}
	src := &fakeSrc{}
	a := New(src, quietLog())
	a.Mount("m", 0, func(ch <-chan loop.Event) {
		n := 0
		for range ch {
			n++
		}
		rec.rec("drained")
		_ = n
	})
	src.mu.Lock()
	src.chans[0] <- loop.Event{}
	src.chans[0] <- loop.Event{}
	src.mu.Unlock()

	quit := make(chan struct{})
	close(quit)
	a.Run(quit)
	if !rec.has("drained") {
		t.Fatal("Run 返回前消费者未排干")
	}
}

// 收尾钩子 panic 不拖垮链：后续钩子照跑。
func TestTeardownPanicIsolated(t *testing.T) {
	rec := &recorder{}
	a := New(&fakeSrc{}, quietLog())
	a.OnShutdown("bottom", func() { rec.rec("bottom") }) // 最先注册 → 最后执行
	a.OnShutdown("boom", func() { panic("boom") })
	quit := make(chan struct{})
	close(quit)
	a.Run(quit)
	if !rec.has("bottom") {
		t.Fatalf("panic 钩子拖垮了后续收尾，序列 %v", rec.snapshot())
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
