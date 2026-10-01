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

// Sink 是引擎投递口（gowild.Input 的 InputSink 实现）：多插头纪律
// （06 §2）归宿主——在途轮次先试 Queue（轮间注入，非阻塞）；空闲
// （ErrNoActiveRun）则用 Input 起一轮、阻塞到本轮结算。先后由会话锁与
// 到达顺序决定，与「谁先来谁先进」一致。说话人标签与当次模型参数都在
// 这里落。
type Sink struct {
	Session *agent.Session
	Cfg     config.Config
}

func (s Sink) Deliver(text, speaker string) error {
	sess, cfg := s.Session, s.Cfg
	msg := message.NewUser(message.TagSpeaker(speaker, text))
	if err := sess.Queue(msg); errors.Is(err, agent.ErrNoActiveRun) {
		ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: speaker})
		ctx = ctxx.WithOptions(ctx, llmOptions(cfg))
		_, err := sess.Input(ctx, msg)
		return err
	} else if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  · 轮间注入：%s\n", strings.TrimSpace(text))
	return nil
}

// llmOptions 把 [llm] 配置翻译成每请求 Options。temperature/max_tokens/
// reasoning_effort 无 viper 兜底，零值 = 不下发该字段（用 provider 的模型
// 默认）——绝不显式发 temperature:0，那等于把人设锁进贪心解码。
// 注意 Options 会被压缩摘要继承（model/temperature；max_tokens 已隔离）：
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
