// Package persist 是 runtime 的事件溯源写路（docs/03 §5）：把 core 的 durable
// 事件按序追加进 Store。v1 只写不恢复——「跨天续聊」由窗口的压缩记忆支撑，
// 事件重放（resume）在有真实需求前不做。
package persist

import (
	"context"
	"errors"
	"log/slog"

	"aria/core/loop"
)

// Store 是会话历史的落盘能力。窄接口定义在消费方（03 §1），
// 实现（SQLite/JSONL）在 plugins，由 Setup 注入——runtime 不认具体驱动。
type Store interface {
	Append(ctx context.Context, sessionID string, ev loop.Event) error
}

// Recorder 把单个会话的 durable 事件按序写入 Store。
// 单 goroutine 消费 = 单写者，顺序来自事件通道本身（03 §1 存储分账）。
type Recorder struct {
	store Store
	log   *slog.Logger
}

func New(store Store, log *slog.Logger) (*Recorder, error) {
	if store == nil {
		return nil, errors.New("persist: Store is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{store: store, log: log}, nil
}

// Consume 阻塞消费 events 直到通道关闭或 ctx 取消。
//
// 事件记在所属轮次的会话下：AgentStart 携带的 Scope.SessionID 自该事件起生效，直至下一个
// AgentStart；首个 AgentStart 之前（或其 SessionID 为空时）使用 sessionID。会话切换只发生在
// 轮次之间，同一轮次的事件因此记在同一会话下。
//
// volatile 事件（增量渲染）不落盘：MessageEnd 永远带全文（02 §3.1）。
// 写入失败直接返回错误、不再继续消费——durable 的「不丢」承诺一旦破掉，
// 静默吞掉只会让损失更大，必须让调用方看见。
func (r *Recorder) Consume(ctx context.Context, sessionID string, events <-chan loop.Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if !ev.Kind.Durable() {
				continue
			}
			if d, ok := ev.Data.(loop.AgentStartData); ok && d.Scope.SessionID != "" {
				sessionID = d.Scope.SessionID
			}
			if err := r.store.Append(ctx, sessionID, ev); err != nil {
				r.log.Error("persist: append failed",
					"session", sessionID, "kind", string(ev.Kind), "err", err)
				return err
			}
		}
	}
}
