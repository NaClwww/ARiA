package ctxx

import (
	"context"
	"math"
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

func TestOptionsStopIsIsolated(t *testing.T) {
	stop := []string{"first"}
	ctx := WithOptions(context.Background(), Options{Stop: stop})
	stop[0] = "mutated"

	got, ok := OptionsFrom(ctx)
	if !ok || got.Stop[0] != "first" {
		t.Fatalf("stored options changed: %+v ok=%v", got, ok)
	}
	got.Stop[0] = "returned mutation"
	again, _ := OptionsFrom(ctx)
	if again.Stop[0] != "first" {
		t.Fatalf("returned Stop aliases stored options: %+v", again.Stop)
	}
}

func TestBudgetCostConversionBoundaries(t *testing.T) {
	if got := NewBudget(0, 0.0000004).MaxCostMicro; got != 1 {
		t.Fatalf("positive limit became unlimited: %d", got)
	}
	if got := NewBudget(0, 0.0000015).MaxCostMicro; got != 2 {
		t.Fatalf("limit was not rounded: %d", got)
	}
	if got := NewBudget(0, math.MaxFloat64).MaxCostMicro; got != math.MaxInt64 {
		t.Fatalf("large limit not clamped: %d", got)
	}
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		b := NewBudget(0, 1)
		b.Consume(message.Usage{In: -1, Out: -2, Cost: invalid})
		tokens, cost := b.Used()
		if tokens != 0 || cost != 0 {
			t.Fatalf("invalid usage %v was counted: tokens=%d cost=%d", invalid, tokens, cost)
		}
	}
	b := NewBudget(0, 1)
	b.Consume(message.Usage{Cost: 0.0000015})
	_, cost := b.Used()
	if cost != 2 {
		t.Fatalf("usage was not rounded: %d", cost)
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

func TestOptionsTemperatureIsIsolated(t *testing.T) {
	temp := 0.7
	ctx := WithOptions(context.Background(), Options{Temperature: &temp})
	temp = 1.5 // 写入后改外部指针不得影响 ctx 内值

	got, ok := OptionsFrom(ctx)
	if !ok || got.Temperature == nil || *got.Temperature != 0.7 {
		t.Fatalf("stored temperature changed: ok=%v temp=%v", ok, got.Temperature)
	}
	*got.Temperature = 2.0 // 读侧改返回值不得污染 ctx 内值
	again, _ := OptionsFrom(ctx)
	if *again.Temperature != 0.7 {
		t.Fatalf("returned temperature aliases stored value: %v", *again.Temperature)
	}
}
