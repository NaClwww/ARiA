// Package provider 定义 core 消费的 Provider 契约（docs/02 §5）。
// Provider 是哑管道；适配器在 plugins（04 §2），core 只见接口与 Fake。
package provider

import (
	"context"
	"errors"

	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

type Request struct {
	Messages []message.Message
	Tools    []tool.Def
	Options  ctxx.Options // 从 ctxx 读出后显式传递，provider 不再摸 ctx 值
}

// StreamEvent 由 loop 翻译成事件词汇（02 §3.1）。
type StreamEvent interface{ isStreamEvent() }

// PartDelta / ThoughtDelta 是 volatile 渲染增量；真值以 MessageComplete 为准。
type PartDelta struct{ Text string }
type ThoughtDelta struct{ Text string }

// MessageComplete 是一次流式调用的定稿载荷。
// Interrupted=true 表示 ctx 取消时的部分输出——这是 provider 的硬性义务（02 §5），
// 是 Interrupt 可续的前提。残缺的 tool calls 一律不进 Message（A3 默认）。
type MessageComplete struct {
	Message     message.Message
	Usage       message.Usage
	Interrupted bool
}

// ErrorEvent 分类的流中错误：Retryable 且未产出任何内容时可退避重试（02 §7）。
type ErrorEvent struct {
	Err       error
	Retryable bool
}

// RetryableError 标记可重试的错误（连接失败、408/429/5xx）。
// Stream 立即返回 error 时用它区分「值得重试」与「参数/鉴权错误」。
type RetryableError struct{ Err error }

func (e RetryableError) Error() string { return e.Err.Error() }
func (e RetryableError) Unwrap() error { return e.Err }

// IsRetryable 报告 err 是否值得退避重试。值与指针形态的 RetryableError
// 都识别（插件适配器可能返回 &RetryableError{}）。
func IsRetryable(err error) bool {
	var re RetryableError
	if errors.As(err, &re) {
		return true
	}
	var pe *RetryableError
	return errors.As(err, &pe)
}

func (PartDelta) isStreamEvent()       {}
func (ThoughtDelta) isStreamEvent()    {}
func (MessageComplete) isStreamEvent() {}
func (ErrorEvent) isStreamEvent()      {}

type Provider interface {
	// Stream 返回事件通道；ctx 取消时必须以 MessageComplete{Interrupted:true}
	// 收尾返回已收内容，不得只回 error。通道关闭前必须恰好一个 MessageComplete
	// 或至少一个 ErrorEvent。
	Stream(ctx context.Context, req Request) (<-chan StreamEvent, error)

	// Limits 报告 model 的上下文窗口与缺省输出上限；model 为空指适配器的默认模型。
	// 未知模型返回零值，调用方据此不按上下文用量触发压缩。
	Limits(model string) Limits

	// CountTokens 估算 msgs 作为 model 的输入时的 token 数（无 usage 时用于判定上下文用量）；
	// 返回负数表示不提供估算，调用方改用 EstimateTokens。
	CountTokens(model string, msgs []message.Message) int
}
