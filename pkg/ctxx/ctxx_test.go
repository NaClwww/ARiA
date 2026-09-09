package ctxx

import (
	"context"
	"sync"
	"testing"

	"aria/pkg/message"
)

// 01 §3：Budget 并发扣减的专门竞争测试（-race 下运行）。
func TestBudgetConcurrentConsume(t *testing.T) {
	b := NewBudget(1000, 10.0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Consume(message.Usage{In: 1, Out: 1, Cost: 0.001})
			}
		}()
	}
	wg.Wait()

	tokens, costMicro := b.Used()
	if tokens != 1600 || costMicro != 800000 { // 8×100×2 tokens；8×100×0.001 → 800000 微单位
		t.Fatalf("used: tokens=%d costMicro=%d", tokens, costMicro)
	}
	if !b.Exceeded() {
		t.Fatal("budget should be exceeded")
	}
}

func TestScopeFailClosed(t *testing.T) {
	if _, ok := ScopeFrom(context.Background()); ok {
		t.Fatal("empty ctx must not yield scope")
	}
	if _, ok := ScopeFrom(WithScope(context.Background(), Scope{UserID: "u"})); ok {
		t.Fatal("scope without SessionID must fail-closed")
	}
	if _, ok := ScopeFrom(WithScope(context.Background(), Scope{SessionID: "s"})); !ok {
		t.Fatal("scope with SessionID must pass")
	}
}

func TestDerivationKeepsValues(t *testing.T) {
	ctx := WithScope(testRoot(), Scope{SessionID: "s"})
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if s, ok := ScopeFrom(child); !ok || s.SessionID != "s" {
		t.Fatalf("derived ctx lost scope: %+v ok=%v", s, ok)
	}
}

func testRoot() context.Context { return context.Background() }
