package window

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// DefaultSummaryInstruction 是间隙压缩的默认提示词。
const DefaultSummaryInstruction = "把下面的对话压缩成简洁的摘要，保留：谁说了什么关键信息、" +
	"已达成的结论、未完成的事项、与用户相关的偏好。不要添加原文没有的内容。"

// ProviderCompressor 用一次 Provider 调用把「记忆 + 本轮」压成一段摘要（03 §5）。
// 它是 Compressor 的默认 LLM 实现；换压缩策略只需换 Compressor，窗口不动。
//
// ctx 若带 ctxx.Options（model、temperature…）会作为兜底；本结构体字段优先。
// MaxTokens 例外——不继承 ctx（见 Compress），仅本字段显式设置才生效。
type ProviderCompressor struct {
	Provider provider.Provider

	Model       string // 空 = 用 ctx 里 ctxx.Options 的模型
	MaxTokens   int    // 摘要输出上限；0 = 不限，也不继承主对话的配置
	Instruction string // 空 = DefaultSummaryInstruction
}

func (p *ProviderCompressor) Compress(ctx context.Context, memory, turn []message.Message) ([]message.Message, error) {
	if p.Provider == nil {
		return nil, errors.New("window: ProviderCompressor requires Provider")
	}
	msgs := append(cloneAll(memory), cloneAll(turn)...)
	if len(msgs) == 0 {
		return nil, nil
	}

	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text())
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, text)
	}
	if b.Len() == 0 {
		return nil, nil
	}

	instruction := p.Instruction
	if instruction == "" {
		instruction = DefaultSummaryInstruction
	}
	// 本结构体字段优先，其次是调用方 ctx 里的 Options（模型覆盖等）。
	// MaxTokens 不继承：主对话的输出上限套在摘要上，思考型模型会把
	// 限额全烧在推理里、正文一字未出即被截断——落进窗口的就是
	// 「空摘要」失败（2026-09-28 真机实测）。上限只有显式给才生效。
	opts, _ := ctxx.OptionsFrom(ctx)
	if p.Model != "" {
		opts.Model = p.Model
	}
	opts.MaxTokens = p.MaxTokens
	req := provider.Request{
		Messages: []message.Message{
			message.NewSystem(instruction),
			message.NewUser(b.String()),
		},
		Options: opts,
	}

	ch, err := p.Provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var summary string
	for ev := range ch {
		switch e := ev.(type) {
		case provider.MessageComplete:
			if e.Interrupted {
				return nil, errors.New("window: compression interrupted")
			}
			summary = strings.TrimSpace(e.Message.Text())
		case provider.ErrorEvent:
			return nil, e.Err
		}
	}
	if summary == "" {
		return nil, errors.New("window: compressor produced empty summary")
	}
	// 记忆走非 system 的 <memory> 块：内容来自对话，属不可信数据
	// （03 §5 上下文分层——不可信内容永不进 system role）。
	return []message.Message{MemoryMessage(summary)}, nil
}
