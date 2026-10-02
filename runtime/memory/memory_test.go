package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeService 的 Stage 与 End 在前 failures 次调用失败；Start 与 Recall 阻塞至 ctx 结束（block 为真时）。
type fakeService struct {
	mu       sync.Mutex
	calls    int
	failures int
	block    bool
}

func (f *fakeService) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeService) call() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return errors.New("unavailable")
	}
	return nil
}

func (f *fakeService) Stage(context.Context, StageRequest) error { return f.call() }
func (f *fakeService) End(context.Context, SessionRequest) error { return f.call() }

func (f *fakeService) Start(ctx context.Context, _ SessionRequest) ([]Item, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []Item{{Text: "要点"}}, nil
}

func (f *fakeService) Recall(ctx context.Context, _ RecallRequest) ([]Item, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, nil
}

func fastClient(svc Service) *Client {
	c := NewClient(svc, quiet())
	c.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c.startTimeout = 20 * time.Millisecond
	c.recallTimeout = 20 * time.Millisecond
	return c
}

// Stage 与 End 失败后重试，重试中成功即返回 nil。
func TestClientRetriesUntilSuccess(t *testing.T) {
	svc := &fakeService{failures: 2}
	if err := fastClient(svc).Stage(context.Background(), StageRequest{}); err != nil {
		t.Fatalf("第 3 次调用成功，应返回 nil：%v", err)
	}
	if n := svc.count(); n != 3 {
		t.Fatalf("应调用 3 次，实际 %d 次", n)
	}
}

// 重试 3 次后仍失败：共调用 4 次，返回错误。
func TestClientGivesUpAfterThreeRetries(t *testing.T) {
	svc := &fakeService{failures: 100}
	if err := fastClient(svc).End(context.Background(), SessionRequest{}); err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	if n := svc.count(); n != 4 {
		t.Fatalf("应调用 4 次（1 次 + 重试 3 次），实际 %d 次", n)
	}
}

// 重试等待期间 ctx 取消：立即返回，不再重试。
func TestClientRetryStopsOnCancel(t *testing.T) {
	svc := &fakeService{failures: 100}
	c := NewClient(svc, quiet())
	c.retryDelays = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Stage(ctx, StageRequest{}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回取消错误：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Stage 未返回")
	}
}

// Start 与 Recall 超时后返回错误与空结果。
func TestClientStartAndRecallTimeout(t *testing.T) {
	c := fastClient(&fakeService{block: true})
	if items, err := c.Start(context.Background(), SessionRequest{}); err == nil || items != nil {
		t.Fatalf("Start 超时应返回错误与 nil：items %v err %v", items, err)
	}
	if items, err := c.Recall(context.Background(), RecallRequest{}); err == nil || items != nil {
		t.Fatalf("Recall 超时应返回错误与 nil：items %v err %v", items, err)
	}
}
