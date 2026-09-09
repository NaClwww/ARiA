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

// IsRetryable 报告 err 是否值得退避重试。
func IsRetryable(err error) bool {
	var re RetryableError
	return errors.As(err, &re)
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
}
