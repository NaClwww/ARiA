package provider

import (
	"math"
	"unicode"

	"aria/pkg/message"
)

// Limits 是模型的 token 限额，由 Provider.Limits 按模型报告。
type Limits struct {
	ContextWindow int // 上下文窗口：单次请求输入与输出的 token 总上限；0 = 未知
	MaxOutput     int // 请求未设置 max_tokens 时服务端采用的输出上限；0 = 未知
}

// NoLimits 可嵌入 Provider 实现：Limits 返回零值（窗口未知），CountTokens 不提供估算。
// 适用于测试替身与不关心上下文预算的实现。
type NoLimits struct{}

func (NoLimits) Limits(string) Limits                      { return Limits{} }
func (NoLimits) CountTokens(string, []message.Message) int { return -1 }

// TokenRates 是按字符数估算 token 的换算比例。
type TokenRates struct {
	CJK        float64 // 每个中日韩字符（汉字、假名、谚文）折合的 token 数
	Other      float64 // 每个其他非空白字符折合的 token 数
	Image      int     // 每个图片块的 token 数
	PerMessage int     // 每条消息的固定开销（角色与分隔标记）
}

// DefaultTokenRates 是 Provider 不提供估算时的通用比例。取值偏大：
// 高估只会使按用量触发的压缩提前发生，低估则可能使请求超出模型窗口。
var DefaultTokenRates = TokenRates{CJK: 1, Other: 0.34, Image: 1024, PerMessage: 4}

// Estimate 按换算比例估算 msgs 的 token 数。计入文本块、思考块、工具调用的
// 名称与参数、文件块的名称与 URI，以及图片块；音频块不计。
func (r TokenRates) Estimate(msgs []message.Message) int {
	total := 0.0
	for _, m := range msgs {
		total += float64(r.PerMessage)
		for _, b := range m.Blocks {
			switch v := b.(type) {
			case message.TextBlock:
				total += r.text(v.Text)
			case *message.TextBlock:
				if v != nil {
					total += r.text(v.Text)
				}
			case message.ThoughtBlock:
				total += r.text(v.Text)
			case *message.ThoughtBlock:
				if v != nil {
					total += r.text(v.Text)
				}
			case message.FileBlock:
				total += r.text(v.Name) + r.text(v.URI)
			case *message.FileBlock:
				if v != nil {
					total += r.text(v.Name) + r.text(v.URI)
				}
			case message.ImageBlock, *message.ImageBlock:
				total += float64(r.Image)
			}
		}
		for _, c := range m.ToolCalls {
			total += r.text(c.Name) + r.text(string(c.Args))
		}
	}
	return int(math.Ceil(total))
}

func (r TokenRates) text(s string) float64 {
	n := 0.0
	for _, c := range s {
		switch {
		case unicode.IsSpace(c):
		case unicode.In(c, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			n += r.CJK
		default:
			n += r.Other
		}
	}
	return n
}

// EstimateTokens 用 DefaultTokenRates 估算 msgs 的 token 数。
func EstimateTokens(msgs []message.Message) int { return DefaultTokenRates.Estimate(msgs) }

// CountTokens 估算 msgs 作为 model 的输入时的 token 数：优先采用 p 的估算，
// p 不提供（返回负数）时改用 EstimateTokens。
func CountTokens(p Provider, model string, msgs []message.Message) int {
	if p != nil {
		if n := p.CountTokens(model, msgs); n >= 0 {
			return n
		}
	}
	return EstimateTokens(msgs)
}
