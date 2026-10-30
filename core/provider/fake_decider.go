package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"aria/pkg/message"
)

// FakeDecider 是脚本化 Decider（与 Fake 同纪律）：按调用顺序消费 DeciderStep，脚本用完后交给
// Fallback（WithFallback），都没有时返回 FatalError。答案经与真实适配器相同的 NormalizeAnswer
// 补全（Choice / Score / Confidence），测试与线上走同一条归一路径。无网络无磁盘，并发安全。
//
// 并发调用（注意力层 max_inflight > 1）时谁先取到哪一步脚本不确定；这类测试用 Fallback，
// 它只看 state 与问题，与调用顺序无关。
type FakeDecider struct {
	mu       sync.Mutex
	steps    []DeciderStep
	i        int
	n        int // 已调用次数，生成 ID
	caps     DeciderCaps
	fallback func(state json.RawMessage, q Question) (Answer, bool)
	calls    []DeciderCall
}

// DeciderStep 是一次调用的脚本。
type DeciderStep struct {
	// Answers 按问题名给原始答案：noul 填 P；choice / score 填 Probs（score 按档位名作键）。
	// 被问到却不在表里的问题、Kind 与问题不符或选项不在 criteria 里的答案 → Decision.Missing；
	// 表里有但没被问到的 → 忽略。
	Answers map[string]Answer

	// Err 非空时本次调用整体失败；Retryable 时包成 RetryableError。
	Err       error
	Retryable bool

	// Delay > 0 时先等待，期间 ctx 取消返回 ctx.Err()（验证 timeout_ms → 兜底路径）。
	Delay time.Duration
}

// DeciderCall 记录一次调用（测试断言微批：一车装了几个条目、问了哪些问题、带了哪些图）。
type DeciderCall struct {
	State     json.RawMessage // 调用时刻编码：之后改原对象不影响记录
	Questions []Question
	Images    []string // 图片引用串（排序）；不记图片数据
}

var _ Decider = (*FakeDecider)(nil)

func NewFakeDecider(steps ...DeciderStep) *FakeDecider {
	cloned := make([]DeciderStep, len(steps))
	for i, s := range steps {
		s.Answers = cloneAnswers(s.Answers)
		cloned[i] = s
	}
	return &FakeDecider{steps: cloned}
}

// WithCaps 设置 Caps 的返回值（所有模型相同），Decide 按它校验问题集；返回 f 本身便于链式构造。
func (f *FakeDecider) WithCaps(c DeciderCaps) *FakeDecider {
	f.mu.Lock()
	f.caps = c
	f.caps.Kinds = append([]QuestionKind(nil), c.Kinds...)
	f.mu.Unlock()
	return f
}

// WithFallback 设置脚本用完后的回答函数：按 state（JSON）与问题给原始答案，false = 漏答。
func (f *FakeDecider) WithFallback(fn func(state json.RawMessage, q Question) (Answer, bool)) *FakeDecider {
	f.mu.Lock()
	f.fallback = fn
	f.mu.Unlock()
	return f
}

// Caps 返回 WithCaps 设置的值；未设置时为零值（只按协议上限校验）。
func (f *FakeDecider) Caps(string) DeciderCaps {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.caps
	c.Kinds = append([]QuestionKind(nil), f.caps.Kinds...)
	return c
}

// Left 返回剩余脚本步数（测试断言用）。
func (f *FakeDecider) Left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps) - f.i
}

// Calls 返回已记录调用的副本。校验失败的调用不记录——真实实现不会发出这些请求。
func (f *FakeDecider) Calls() []DeciderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DeciderCall, len(f.calls))
	for i, c := range f.calls {
		out[i] = DeciderCall{State: append(json.RawMessage(nil), c.State...), Questions: cloneQuestions(c.Questions),
			Images: append([]string(nil), c.Images...)}
	}
	return out
}

func (f *FakeDecider) Decide(ctx context.Context, in DecideInput, qs []Question) (Decision, error) {
	raw, err := ValidateInput(in, qs, f.Caps(""))
	if err != nil {
		return Decision{}, err
	}
	images := make([]string, 0, len(in.Images))
	for ref := range in.Images {
		images = append(images, ref)
	}
	sort.Strings(images)

	f.mu.Lock()
	f.calls = append(f.calls, DeciderCall{State: raw, Questions: cloneQuestions(qs), Images: images})
	f.n++
	id := f.n
	var step *DeciderStep
	if f.i < len(f.steps) {
		s := f.steps[f.i]
		f.i++
		step = &s
	}
	fallback := f.fallback
	f.mu.Unlock()

	if step == nil && fallback == nil {
		return Decision{}, &FatalError{Msg: "fake decider: script exhausted"}
	}
	if step != nil && step.Delay > 0 {
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-time.After(step.Delay):
		}
	} else if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if step != nil && step.Err != nil {
		if step.Retryable {
			return Decision{}, RetryableError{Err: step.Err}
		}
		return Decision{}, step.Err
	}

	d := Decision{
		Answers: make(map[string]Answer, len(qs)),
		Model:   "fake",
		ID:      fmt.Sprintf("fake-%d", id),
		Usage:   message.Usage{In: int(math.Ceil(DefaultTokenRates.text(string(raw)))) + len(images)*DefaultTokenRates.Image},
	}
	for _, q := range qs {
		var a Answer
		var ok bool
		if step != nil {
			a, ok = step.Answers[q.Name]
		} else {
			a, ok = fallback(raw, q)
		}
		if ok {
			a, ok = NormalizeAnswer(q, a)
		}
		if !ok {
			d.Missing = append(d.Missing, q.Name)
			continue
		}
		d.Answers[q.Name] = a
	}
	return d, nil
}

func cloneAnswers(in map[string]Answer) map[string]Answer {
	if in == nil {
		return nil
	}
	out := make(map[string]Answer, len(in))
	for k, a := range in {
		if a.Probs != nil {
			ps := make(map[string]float64, len(a.Probs))
			for n, p := range a.Probs {
				ps[n] = p
			}
			a.Probs = ps
		}
		out[k] = a
	}
	return out
}

func cloneQuestions(in []Question) []Question {
	out := make([]Question, len(in))
	for i, q := range in {
		q.Criteria = append([]Criterion(nil), q.Criteria...)
		out[i] = q
	}
	return out
}
