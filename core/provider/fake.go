package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"aria/pkg/message"
)

// Fake 是脚本化 Provider（02 §8）：给定应答序列确定性行为，
// 支持模拟流式分片、取消部分输出、可重试错误。无网络无磁盘。
type Fake struct {
	mu    sync.Mutex
	steps []FakeStep
	i     int
}

type FakeStep struct {
	Text    []string // text 增量序列
	Thought []string // thought 增量序列
	Calls   []message.ToolCall
	Usage   message.Usage

	// Err 非空时本步以 ErrorEvent 收场；Retryable 标记可重试。
	Err       error
	Retryable bool

	// ChunkDelay > 0 时每个增量间休眠，ctx 取消即产出部分输出
	// （验证 provider 部分结果义务与 Interrupt 路径）。
	ChunkDelay time.Duration
}

func NewFake(steps ...FakeStep) *Fake {
	return &Fake{steps: steps}
}

// Left 返回剩余脚本步数（测试断言用）。
func (f *Fake) Left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps) - f.i
}

func (f *Fake) Stream(ctx context.Context, _ Request) (<-chan StreamEvent, error) {
	f.mu.Lock()
	if f.i >= len(f.steps) {
		f.mu.Unlock()
		return nil, &FatalError{Msg: "fake: script exhausted"}
	}
	s := f.steps[f.i]
	f.i++
	f.mu.Unlock()

	ch := make(chan StreamEvent, 16)
	go func() {
		defer close(ch)
		if s.Err != nil {
			ch <- ErrorEvent{Err: s.Err, Retryable: s.Retryable}
			return
		}
		var text strings.Builder
		partial := func() {
			var blocks []message.Block
			if text.Len() > 0 {
				blocks = append(blocks, message.TextBlock{Text: text.String()})
			}
			ch <- MessageComplete{
				Message:     message.Message{Role: message.RoleAssistant, Blocks: blocks},
				Interrupted: true,
			}
		}
		for _, c := range s.Thought {
			if !f.wait(ctx, s.ChunkDelay, partial) {
				return
			}
			ch <- ThoughtDelta{Text: c}
		}
		for _, c := range s.Text {
			if !f.wait(ctx, s.ChunkDelay, partial) {
				return
			}
			text.WriteString(c)
			ch <- PartDelta{Text: c}
		}
		msg := message.Message{Role: message.RoleAssistant}
		if text.Len() > 0 {
			msg.Blocks = append(msg.Blocks, message.TextBlock{Text: text.String()})
		}
		msg.ToolCalls = s.Calls // 只在流完整走完才挂上（残缺即丢弃，A3 默认）
		ch <- MessageComplete{Message: msg, Usage: s.Usage}
	}()
	return ch, nil
}

// wait 休眠 d 并尊重取消；返回 false 表示已取消且 partial 已发出。
func (f *Fake) wait(ctx context.Context, d time.Duration, partial func()) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			partial()
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		partial()
		return false
	case <-time.After(d):
		return true
	}
}

// FatalError 是不可重试的 provider 故障（02 §7 错误三分法的 Fatal 一侧）。
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }
