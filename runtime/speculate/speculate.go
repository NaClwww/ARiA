// Package speculate 实现预测输入的提前生成（runtime 层，docs/03）。
//
// 预测不经过 Loop：预测轮没有工具、没有 Guard、没有 durable 事件，
// Loop 的价值都不在场，因此这里直接调用 Provider。猜测错误时取消重来，
// 没有任何需要回滚的状态；猜测正确时 Confirm 返回预生成答案，
// 由宿主/runtime 把 [.., 确认输入, 预生成答案] 作为下一次 Run 的
// input 传入，即完成「复用已生成的 tokens」。
//
// 同时只允许一个活动预测；ID+Revision 拒绝迟到的控制命令。
package speculate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

type Input struct {
	ID       string
	Revision uint64
	Message  message.Message
}

// Predicted 是一次确认命中的预生成结果。
type Predicted struct {
	Input message.Message // 确认的用户输入（已定型 role/ID）
	Msg   message.Message // 预生成的 assistant 回答（无工具调用时才有意义）
	Usage message.Usage
}

// DeltaFn 可选：实时接收预测增量（渲染/TTS 提前量用）。
type DeltaFn func(text, thought string)

type Predictor struct {
	prov provider.Provider

	mu     sync.Mutex
	active *run
}

type run struct {
	input  Input
	cancel context.CancelFunc
	done   chan struct{}

	// 以下仅预测 goroutine 写、Confirm 前读。
	result *Predicted
}

func New(prov provider.Provider) (*Predictor, error) {
	if prov == nil {
		return nil, errors.New("speculate: provider is required")
	}
	return &Predictor{prov: prov}, nil
}

// Speculate 用当前 transcript 快照 base + 猜测输入启动一次预测流。
// 若已有活动预测，旧预测被取消后由新预测取代（等同于 Replace 语义）。
func (p *Predictor) Speculate(parent context.Context, base []message.Message, in Input, onDelta DeltaFn) error {
	if in.ID == "" || in.Revision == 0 {
		return errors.New("speculate: ID and revision are required")
	}
	if _, ok := ctxx.ScopeFrom(parent); !ok {
		return errors.New("speculate: scope missing in ctx (fail-closed)")
	}

	p.mu.Lock()
	if old := p.active; old != nil {
		old.cancel()
		<-old.done // 预测无副作用，等它退出只是为了不与新旧流交错
	}
	ctx, cancel := context.WithCancel(parent)
	r := &run{input: in, cancel: cancel, done: make(chan struct{})}
	p.active = r
	p.mu.Unlock()

	go p.stream(ctx, cloneBase(base), r, onDelta)
	return nil
}

// Confirm 返回命中的预生成结果并清空预测槽；ID/revision 不匹配返回 false。
func (p *Predictor) Confirm(id string, rev uint64) (Predicted, bool) {
	p.mu.Lock()
	r := p.active
	p.mu.Unlock()
	if r == nil || r.input.ID != id || r.input.Revision != rev {
		return Predicted{}, false
	}
	<-r.done // 预测已提交或被取消
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != r { // 等待期间被 Replace/新 Speculate 顶掉
		return Predicted{}, false
	}
	p.active = nil
	if r.result == nil {
		return Predicted{}, false
	}
	return *r.result, true
}

// Cancel 取消预测；已完成的预测被取消同样丢弃（未确认就不是事实）。
func (p *Predictor) Cancel(id string, rev uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.active
	if r == nil || r.input.ID != id || r.input.Revision != rev {
		return false
	}
	r.cancel()
	return true
}

func (p *Predictor) stream(ctx context.Context, base []message.Message, r *run, onDelta DeltaFn) {
	defer func() {
		r.cancel()
		close(r.done)
	}()

	input := r.input.Message.Clone()
	if input.ID == "" {
		input.ID = newID()
	}
	if input.Role == "" {
		input.Role = message.RoleUser
	}
	opts, _ := ctxx.OptionsFrom(ctx)
	req := provider.Request{
		Messages: append(base, input),
		Options:  opts, // Tools 留空：预测轮不允许工具调用
	}

	evCh, err := p.prov.Stream(ctx, req)
	if err != nil {
		return // 预测失败 = 丢弃，无副作用
	}
	for ev := range evCh {
		switch e := ev.(type) {
		case provider.PartDelta:
			if onDelta != nil {
				onDelta(e.Text, "")
			}
		case provider.ThoughtDelta:
			if onDelta != nil {
				onDelta("", e.Text)
			}
		case provider.MessageComplete:
			if e.Interrupted || ctx.Err() != nil {
				return
			}
			m := e.Message.Clone()
			if len(m.ToolCalls) > 0 {
				m.ToolCalls = nil // 预测轮不执行工具；带调用的答案不可复用
			}
			r.result = &Predicted{Input: input, Msg: m, Usage: e.Usage}
			return
		case provider.ErrorEvent:
			return
		}
	}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("speculate: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func cloneBase(base []message.Message) []message.Message {
	out := make([]message.Message, len(base))
	for i := range base {
		out[i] = base[i].Clone()
	}
	return out
}
