package ariahost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/runtime/agent"
)

// Sink 是引擎投递口（gowild.Input 的 InputSink 实现）：Inject 对应 Session.Queue
// （轮间注入，非阻塞），Start 对应 Session.Input（起一轮并阻塞到本轮结算）。
// 投递顺序由 gowild.Input 保证；说话人标签与当次模型参数在这里写入。
type Sink struct {
	Session *agent.Session
	Cfg     config.Config
}

// Inject 尝试轮间注入：无在途轮次或本轮已判定收敛时返回 (false, nil)。
func (s Sink) Inject(text, speaker string) (bool, error) {
	err := s.Session.Queue(message.NewUser(message.TagSpeaker(speaker, text)))
	switch {
	case err == nil:
		fmt.Fprintf(os.Stderr, "  · 轮间注入：%s\n", strings.TrimSpace(text))
		return true, nil
	case errors.Is(err, agent.ErrNoActiveRun):
		return false, nil
	default:
		return false, err
	}
}

// Start 以该输入起一轮，阻塞到本轮结算。Scope 只带 UserID（说话人），SessionID 由会话锚定。
func (s Sink) Start(text, speaker string) error {
	ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: speaker})
	ctx = ctxx.WithOptions(ctx, llmOptions(s.Cfg))
	_, err := s.Session.Input(ctx, message.NewUser(message.TagSpeaker(speaker, text)))
	return err
}

// llmOptions 把 [llm] 配置翻译成每请求 Options。temperature/max_tokens/
// reasoning_effort 无 viper 默认值，零值 = 不下发该字段（用 provider 的模型
// 默认），不显式发送 temperature:0（等同贪心解码）。
// Options 会被压缩摘要继承（model/temperature；max_tokens 已隔离）：
// reasoning_effort 设 none 时摘要也不思考，见 deepseek 适配器的等级解析。
func llmOptions(cfg config.Config) ctxx.Options {
	opts := ctxx.Options{
		Model:           cfg.Provider.Model,
		MaxTokens:       cfg.LLM.MaxTokens,
		ReasoningEffort: cfg.LLM.ReasoningEffort,
	}
	if cfg.LLM.Temperature > 0 {
		temp := cfg.LLM.Temperature
		opts.Temperature = &temp
	}
	return opts
}
