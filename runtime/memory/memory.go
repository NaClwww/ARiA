// Package memory 是记忆服务在 runtime 侧的接口与调用策略（docs/memory/options.md「记忆服务接口」）：
// Service 由记忆服务的客户端实现（放 plugins，经 Setup 注入）；Client 在 Service 之上实现已确认的
// 失败处理（重试与超时），供 agent 调用。
package memory

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Kind 是要点的类别。
type Kind string

const (
	KindFact       Kind = "fact"       // 事实
	KindPreference Kind = "preference" // 偏好
	KindEvent      Kind = "event"      // 事件
	KindCommitment Kind = "commitment" // ARiA 作出的承诺（需求 8），Due 为到期时间
)

// Point 是一条待写入记忆的要点（docs/memory/options.md「要点的格式」）。
type Point struct {
	Text     string
	Kind     Kind      // 空 = 未分类
	Speakers []string  // 涉及的说话人名字
	Due      time.Time // 到期时间，只用于 KindCommitment；零值表示无
}

// Source 标明召回条目的来源。
type Source string

const (
	SourceCommitted Source = "committed" // 已提交的正式记忆
	SourceStaged    Source = "staged"    // 未提交的暂存
)

// Item 是一条召回结果。
type Item struct {
	Text      string
	Source    Source
	SessionID string
	At        time.Time // 发生时间；零值表示未知
}

// StageRequest 是一批要点的暂存请求。BatchID 每批唯一，服务端据此拒绝重放；
// Seq 是本会话内暂存批次的序号，从 1 起。
type StageRequest struct {
	Namespace string
	SessionID string
	BatchID   string
	Seq       int
	Points    []Point
}

// SessionRequest 是会话开始（Start）或会话结束（End）的通知，At 为开始或结束时刻；服务端以 SessionID 去重。
type SessionRequest struct {
	Namespace string
	SessionID string
	At        time.Time
}

// RecallRequest 是一次检索请求。
type RecallRequest struct {
	Namespace string
	SessionID string
	Query     string
	Limit     int
}

// Service 是记忆服务的接口。实现必须尊重 ctx 取消；返回错误即本次调用失败，重试与超时由 Client 负责。
type Service interface {
	// Stage 暂存一批要点。
	Stage(ctx context.Context, req StageRequest) error
	// End 通知会话结束；服务端按自身策略提交并合并该会话的暂存。
	End(ctx context.Context, req SessionRequest) error
	// Start 通知会话开始并返回会话开始时的召回结果；服务端把此前未结束的会话视为已结束。
	Start(ctx context.Context, req SessionRequest) ([]Item, error)
	// Recall 在正式记忆与暂存中检索。
	Recall(ctx context.Context, req RecallRequest) ([]Item, error)
}

// 失败处理的缺省值（docs/memory/options.md「记忆服务接口」）。Recall 由 memory_recall 工具调用
// （模型发起，不在组装路径上），超时 3 s。
const (
	DefaultStartTimeout  = 5 * time.Second
	DefaultRecallTimeout = 3 * time.Second
)

// DefaultRetryDelays 是 Stage 与 End 失败后的重试间隔，共重试 3 次；agent 对会话切换时的要点提取使用同一间隔。
var DefaultRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// Client 在 Service 之上实现失败处理：Stage 与 End 失败后按 DefaultRetryDelays 重试；
// Start 超时 DefaultStartTimeout，Recall 超时 DefaultRecallTimeout。日志经 log 输出。
type Client struct {
	svc           Service
	log           *slog.Logger
	retryDelays   []time.Duration
	startTimeout  time.Duration
	recallTimeout time.Duration
}

// NewClient 用缺省的重试间隔与超时包装 svc；log 为 nil 时使用 slog.Default()。
func NewClient(svc Service, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		svc:           svc,
		log:           log,
		retryDelays:   DefaultRetryDelays,
		startTimeout:  DefaultStartTimeout,
		recallTimeout: DefaultRecallTimeout,
	}
}

// SetStartTimeout 在 d > 0 时以 d 替换 Start 的超时（缺省 DefaultStartTimeout）；须在首次调用 Start 前设置。
func (c *Client) SetStartTimeout(d time.Duration) {
	if d > 0 {
		c.startTimeout = d
	}
}

// Stage 暂存一批要点，失败后重试；全部失败时输出 Error 日志 memory: stage failed, points dropped，
// 返回最后一次的错误。
func (c *Client) Stage(ctx context.Context, req StageRequest) error {
	err := Retry(ctx, c.retryDelays, func(ctx context.Context) error { return c.svc.Stage(ctx, req) })
	if err != nil {
		c.log.Error("memory: stage failed, points dropped",
			"session", req.SessionID, "batch", req.BatchID, "seq", req.Seq, "points", len(req.Points), "err", err)
	}
	return err
}

// End 通知会话结束，失败后重试；全部失败时输出 Error 日志 memory: end failed（由下一次 Start 补齐），
// 返回最后一次的错误。
func (c *Client) End(ctx context.Context, req SessionRequest) error {
	err := Retry(ctx, c.retryDelays, func(ctx context.Context) error { return c.svc.End(ctx, req) })
	if err != nil {
		c.log.Error("memory: end failed, left to the next start", "session", req.SessionID, "err", err)
	}
	return err
}

// Start 通知会话开始并返回召回结果，超时 startTimeout；超时或失败时输出 Warn 日志
// memory: start failed, session begins without recall，返回 nil 与错误。
func (c *Client) Start(ctx context.Context, req SessionRequest) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, c.startTimeout)
	defer cancel()
	items, err := c.svc.Start(ctx, req)
	if err != nil {
		c.log.Warn("memory: start failed, session begins without recall", "session", req.SessionID, "err", err)
		return nil, err
	}
	return items, nil
}

// Recall 检索，超时 recallTimeout；超时或失败时输出 Warn 日志 memory: recall failed，返回 nil 与错误。
func (c *Client) Recall(ctx context.Context, req RecallRequest) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, c.recallTimeout)
	defer cancel()
	items, err := c.svc.Recall(ctx, req)
	if err != nil {
		c.log.Warn("memory: recall failed", "session", req.SessionID, "err", err)
		return nil, err
	}
	return items, nil
}

// Retry 执行 call，失败后按 delays 依次等待并重试（共重试 len(delays) 次）；ctx 取消时停止等待，
// 返回 ctx 的错误与最后一次错误。
func Retry(ctx context.Context, delays []time.Duration, call func(context.Context) error) error {
	err := call(ctx)
	for _, d := range delays {
		if err == nil {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(ctx.Err(), err)
		case <-t.C:
		}
		err = call(ctx)
	}
	return err
}
