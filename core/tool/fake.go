package tool

import (
	"context"
	"sync"
	"time"

	"aria/pkg/message"
)

// Fake 是脚本化工具（docs/02 §8）：注错、慢执行、记录调用。无网络无磁盘。
type Fake struct {
	name  string
	res   message.ToolResult
	delay time.Duration

	mu       sync.Mutex
	execN    int
	lastArgs []byte
	lastID   string
}

func NewFake(name string, res message.ToolResult, delay time.Duration) *Fake {
	return &Fake{name: name, res: res, delay: delay}
}

func (f *Fake) Def() Def { return Def{Name: f.name, Description: "fake tool"} }

func (f *Fake) Exec(ctx context.Context, call Call) Result {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return Result{CallID: call.ID, IsError: true,
				Blocks: []message.Block{message.TextBlock{Text: "[tool ctx canceled]"}}}
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	f.execN++
	f.lastArgs = append([]byte(nil), call.Args...)
	f.lastID = call.ID
	f.mu.Unlock()

	res := f.res
	if res.CallID == "" {
		res.CallID = call.ID
	}
	return res
}

// Calls 返回执行次数与最近一次调用的参数（测试断言用）。
func (f *Fake) Calls() (n int, args []byte, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execN, f.lastArgs, f.lastID
}
