package provider

import (
	"context"
	"errors"
	"fmt"
	"math"

	"aria/pkg/message"
)

// Decider 是决策模型契约（docs/discussions/2026-10-29-attention-gate.md §4）：对一份 state 一次回答
// 一组类型化问题，返回概率而不是文本。与 Provider 并列但互不相干——不流式、无工具、无消息历史。
//
// 形状取自 Decisions / System One API（Jev 等约 30 个决策模型共用的契约）；以后的 llm 决策器
// （普通 LLM 回答同样的问题）输出同一个 Decision，所以策略层不知道背后是哪种模型。
//
// 规范层的两条纪律，所有实现（含 FakeDecider）都经 ValidateQuestions / NormalizeAnswer 走：
//   - Confidence 由规范层按 Probs 统一计算（公式见 confidence），不采用厂商返回值——
//     不同模型的阈值因此可比；
//   - score 的 Probs 按档位名作键（线上协议是下标字符串，由适配器换算），与 choice 一致。
type Decider interface {
	// Decide 一次回答 qs。state 必须可 JSON 编码；不可信内容只能放在 state 的字符串里，
	// 问题的 Instructions / Criteria 全部由调用方书写（§4.4）。
	// 单个问题漏答或答案不合法不算错误，记在 Decision.Missing；error 只表示整次调用失败
	// （网络与 5xx 等用 RetryableError 包裹；问题超出模型能力为 ErrUnsupportedQuestion）。
	Decide(ctx context.Context, state any, qs []Question) (Decision, error)

	// Caps 报告 model 的能力；model 为空指实现的默认模型。未知项为零值（= 不限 / 不知道）。
	Caps(model string) DeciderCaps
}

// QuestionKind 是问题类型。
type QuestionKind string

const (
	KindNoul   QuestionKind = "noul"   // 是否题：Answer.P
	KindChoice QuestionKind = "choice" // 选一个：Answer.Choice / Probs
	KindScore  QuestionKind = "score"  // 按档位打分：Answer.Score / Choice / Probs
)

// 协议本身的上限（System One API）；DeciderCaps 可以收得更紧。
const (
	MaxChoiceOptions = 255
	MaxScoreLevels   = 10
)

// Question 是一个类型化问题。Name 只是结果键，模型看不到——含义必须全部写在 Instructions 里
// （可用 `items[2].text` 这类写法引用 state 的字段）。
type Question struct {
	Name         string
	Kind         QuestionKind
	Instructions string
	// Criteria：choice 为选项（1~255）；score 为档位，从低到高（1~10，Probs 按档位名作键，
	// 所以名字必须互不相同）；noul 可选，给出时恰为 "true" / "false" 两项。
	Criteria []Criterion
}

// Criterion 是一个选项 / 档位：Name 是结果里的键，Description 是给模型看的说明（可空）。
type Criterion struct{ Name, Description string }

// Answer 是一个问题的规范化答案。
type Answer struct {
	Kind       QuestionKind
	P          float64            // noul：为真的概率
	Choice     string             // choice：概率最大的选项；score：概率最大的档位名
	Score      float64            // score：按概率加权的档位下标（0 ~ 档位数-1，可落在两档之间）
	Probs      map[string]float64 // choice 按选项名、score 按档位名；noul 为 nil；厂商未给分布时为 nil
	Confidence float64            // 分布的集中程度（不是正确率），0~1；见 confidence
}

// Decision 是一次 Decide 的结果。
type Decision struct {
	Answers map[string]Answer // 按问题名
	Missing []string          // 漏答或答案不合法的问题名（按提问顺序），调用方按兜底处理
	Model   string            // 实际服务的模型快照（如 typesafe/jev-1.14-20261014），trace 用
	ID      string            // 请求 ID，trace 用
	Usage   message.Usage
}

// DeciderCaps 是决策模型的能力（与 Provider.Limits 同纪律：问题集在装配与配置热改时即可校验）。
// 零值 = 未知 / 不限，只按协议上限校验。
type DeciderCaps struct {
	Kinds         []QuestionKind // 支持的问题类型；空 = 全部
	MaxOptions    int            // choice 选项上限；0 = 协议上限
	MaxLevels     int            // score 档位上限；0 = 协议上限
	ContextWindow int            // token；0 = 未知
}

var (
	// ErrInvalidQuestion：问题集本身不合协议（名字重复、缺选项……），是调用方的 bug。
	ErrInvalidQuestion = errors.New("decider: invalid question")
	// ErrUnsupportedQuestion：问题合协议，但超出该模型的能力（DeciderCaps）。
	ErrUnsupportedQuestion = errors.New("decider: unsupported question")
)

// ValidateQuestions 按协议与 caps 校验问题集；错误包裹 ErrInvalidQuestion 或 ErrUnsupportedQuestion。
func ValidateQuestions(qs []Question, caps DeciderCaps) error {
	if len(qs) == 0 {
		return fmt.Errorf("%w: 问题集为空", ErrInvalidQuestion)
	}
	seen := make(map[string]bool, len(qs))
	for _, q := range qs {
		if q.Name == "" {
			return fmt.Errorf("%w: 问题名为空", ErrInvalidQuestion)
		}
		if seen[q.Name] {
			return fmt.Errorf("%w: 问题名 %q 重复", ErrInvalidQuestion, q.Name)
		}
		seen[q.Name] = true
		if err := validateQuestion(q, caps); err != nil {
			return err
		}
	}
	return nil
}

func validateQuestion(q Question, caps DeciderCaps) error {
	switch q.Kind {
	case KindNoul, KindChoice, KindScore:
	default:
		return fmt.Errorf("%w: %s 的类型 %q 未知", ErrInvalidQuestion, q.Name, q.Kind)
	}
	if len(caps.Kinds) > 0 && !containsKind(caps.Kinds, q.Kind) {
		return fmt.Errorf("%w: 模型不支持 %s 类问题（%s）", ErrUnsupportedQuestion, q.Kind, q.Name)
	}
	names := make(map[string]bool, len(q.Criteria))
	for _, c := range q.Criteria {
		if c.Name == "" {
			return fmt.Errorf("%w: %s 有空的选项名", ErrInvalidQuestion, q.Name)
		}
		if names[c.Name] {
			return fmt.Errorf("%w: %s 的选项 %q 重复", ErrInvalidQuestion, q.Name, c.Name)
		}
		names[c.Name] = true
	}
	n := len(q.Criteria)
	switch q.Kind {
	case KindNoul:
		if n != 0 && (n != 2 || !names["true"] || !names["false"]) {
			return fmt.Errorf("%w: noul 问题 %s 的 criteria 须恰为 true / false", ErrInvalidQuestion, q.Name)
		}
	case KindChoice:
		if n < 1 || n > MaxChoiceOptions {
			return fmt.Errorf("%w: choice 问题 %s 有 %d 个选项（须 1~%d）", ErrInvalidQuestion, q.Name, n, MaxChoiceOptions)
		}
		if caps.MaxOptions > 0 && n > caps.MaxOptions {
			return fmt.Errorf("%w: choice 问题 %s 有 %d 个选项，模型上限 %d", ErrUnsupportedQuestion, q.Name, n, caps.MaxOptions)
		}
	case KindScore:
		if n < 1 || n > MaxScoreLevels {
			return fmt.Errorf("%w: score 问题 %s 有 %d 档（须 1~%d）", ErrInvalidQuestion, q.Name, n, MaxScoreLevels)
		}
		if caps.MaxLevels > 0 && n > caps.MaxLevels {
			return fmt.Errorf("%w: score 问题 %s 有 %d 档，模型上限 %d", ErrUnsupportedQuestion, q.Name, n, caps.MaxLevels)
		}
	}
	return nil
}

func containsKind(ks []QuestionKind, k QuestionKind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// NormalizeAnswer 把实现拿到的原始答案补全为规范答案；ok=false 表示答案不合法（调用方记进 Missing）。
//
// raw 只需填原始输入：noul 填 P；choice / score 填 Probs（score 按档位名作键）。厂商未给分布时
// （Probs 为空）退而用 raw.Choice / raw.Score，此时 Probs 为 nil、Confidence 取 raw.Confidence。
// 拒收：Kind 与问题不符、概率越界或非有限数、Probs 里有 criteria 之外的键、分布全为 0。
// 分布缺的选项补 0，和不为 1 时按比例归一。
func NormalizeAnswer(q Question, raw Answer) (Answer, bool) {
	if raw.Kind != "" && raw.Kind != q.Kind {
		return Answer{}, false
	}
	switch q.Kind {
	case KindNoul:
		if !isProb(raw.P) {
			return Answer{}, false
		}
		return Answer{Kind: KindNoul, P: raw.P, Confidence: math.Abs(2*raw.P - 1)}, true
	case KindChoice, KindScore:
		if len(raw.Probs) == 0 {
			return normalizeWithoutProbs(q, raw)
		}
		ps, ok := orderedProbs(q.Criteria, raw.Probs)
		if !ok {
			return Answer{}, false
		}
		m := argmax(ps)
		a := Answer{
			Kind:       q.Kind,
			Choice:     q.Criteria[m].Name,
			Probs:      make(map[string]float64, len(ps)),
			Confidence: confidence(q.Kind, ps),
		}
		for i, c := range q.Criteria {
			a.Probs[c.Name] = ps[i]
			if q.Kind == KindScore {
				a.Score += float64(i) * ps[i]
			}
		}
		return a, true
	}
	return Answer{}, false
}

// normalizeWithoutProbs 处理厂商省略了分布（协议里 probabilities 是可选字段）的答案。
func normalizeWithoutProbs(q Question, raw Answer) (Answer, bool) {
	conf := raw.Confidence
	if !isProb(conf) {
		conf = 0
	}
	switch q.Kind {
	case KindChoice:
		for _, c := range q.Criteria {
			if c.Name == raw.Choice {
				return Answer{Kind: KindChoice, Choice: raw.Choice, Confidence: conf}, true
			}
		}
	case KindScore:
		top := float64(len(q.Criteria) - 1)
		if math.IsNaN(raw.Score) || raw.Score < 0 || raw.Score > top {
			return Answer{}, false
		}
		return Answer{Kind: KindScore, Score: raw.Score, Choice: q.Criteria[int(math.Round(raw.Score))].Name, Confidence: conf}, true
	}
	return Answer{}, false
}

// orderedProbs 按 criteria 顺序取出分布：缺的补 0，未知键拒收，和不为 1 时归一。
func orderedProbs(cs []Criterion, probs map[string]float64) ([]float64, bool) {
	known := 0
	ps := make([]float64, len(cs))
	sum := 0.0
	for i, c := range cs {
		p, ok := probs[c.Name]
		if !ok {
			continue
		}
		if !isProb(p) {
			return nil, false
		}
		known++
		ps[i] = p
		sum += p
	}
	if known != len(probs) || sum <= 0 {
		return nil, false
	}
	for i := range ps {
		ps[i] /= sum
	}
	return ps, true
}

func isProb(p float64) bool { return p >= 0 && p <= 1 } // NaN 两个比较都为假

// argmax 返回最大概率的下标；并列取靠前的（criteria 顺序）。
func argmax(ps []float64) int {
	m := 0
	for i, p := range ps {
		if p > ps[m] {
			m = i
		}
	}
	return m
}

// confidence 按 TypeSafe 公开的定义（docs.typesafe.ai/confidence）由分布计算，ps 按 criteria 顺序且和为 1：
//   - choice：(pmax − 1/n) / (1 − 1/n)，即最大概率比均匀分布高出多少；
//   - score：max(0, 1 − Σ pᵢ·|i − m| / MAD_unif)，m 为最可能的档位，MAD_unif = (1/n)·Σ |i − (n−1)/2|，
//     远离峰值的概率比相邻档位扣得多；
//   - noul（在 NormalizeAnswer 里）：|2p − 1|，即两个选项的 choice 公式。
//
// 只有一个选项 / 档位时为 1。
func confidence(kind QuestionKind, ps []float64) float64 {
	n := float64(len(ps))
	if len(ps) <= 1 {
		return 1
	}
	m := argmax(ps)
	if kind == KindChoice {
		return clamp01((ps[m] - 1/n) / (1 - 1/n))
	}
	spread, mad := 0.0, 0.0
	for i, p := range ps {
		spread += p * math.Abs(float64(i-m))
		mad += math.Abs(float64(i) - (n-1)/2)
	}
	mad /= n
	return clamp01(1 - spread/mad)
}

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }
