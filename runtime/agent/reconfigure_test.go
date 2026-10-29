package agent

import (
	"context"
	"testing"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/message"
	"aria/runtime/window"
)

// Reconfigure：会话与窗口不动，下一轮起用新 Provider、新工具表与新人设；落盘里有 reconfigured。
func TestSessionReconfigureKeepsHistory(t *testing.T) {
	store := &memStore{}
	s, _ := newTestSession(t, Config{
		Provider:     provider.NewFake(provider.FakeStep{Text: []string{"旧回答"}}),
		Compressor:   window.KeepLast(100),
		SystemPrompt: "旧人设",
		Store:        store,
	})
	if _, err := s.Input(context.Background(), message.NewUser("第一句")); err != nil {
		t.Fatal(err)
	}

	next := &recorderProvider{inner: provider.NewFake(provider.FakeStep{Text: []string{"新回答"}})}
	now := tool.NewFake("now", message.ToolResult{}, 0)
	if err := s.Reconfigure(Config{
		Provider:     next,
		Tools:        []tool.Tool{now},
		Compressor:   window.KeepLast(100),
		SystemPrompt: "新人设",
	}, "provider=next"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Input(context.Background(), message.NewUser("第二句")); err != nil {
		t.Fatal(err)
	}

	if next.count() != 1 {
		t.Fatalf("new provider calls = %d, want 1", next.count())
	}
	req := next.at(0)
	got := rendered(req.Messages)
	want := []string{"system:新人设", "user:第一句", "assistant:旧回答", "user:第二句"}
	if len(got) != len(want) {
		t.Fatalf("request after reconfigure:\nwant %v\ngot  %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request after reconfigure:\nwant %v\ngot  %v", want, got)
		}
	}
	hasNow := false
	for _, d := range req.Tools {
		if d.Name == "now" {
			hasNow = true
		}
	}
	if !hasNow {
		t.Fatalf("new tool missing from request: %+v", req.Tools)
	}
	if s.SessionID() != scope().SessionID {
		t.Fatalf("session id changed: %s", s.SessionID())
	}
	waitUntil(t, "reconfigured persisted", func() bool { return store.has(loop.KindReconfigured) })
}

// 非法配置报错，会话保持原配置。
func TestSessionReconfigureInvalid(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider: provider.NewFake(provider.FakeStep{Text: []string{"ok"}}),
	})
	if err := s.Reconfigure(Config{}, ""); err == nil {
		t.Fatal("want error without provider")
	}
	dup := tool.NewFake("d", message.ToolResult{}, 0)
	if err := s.Reconfigure(Config{Provider: provider.NewFake(), Tools: []tool.Tool{dup, dup}}, ""); err == nil {
		t.Fatal("want error on duplicate tool")
	}
	if _, err := s.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	if rp.count() != 1 {
		t.Fatalf("original provider calls = %d, want 1", rp.count())
	}
}

// Memory 变更暂存到会话切换：切换前仍用旧服务；Switch 时旧会话的 End 发给旧服务、新会话的
// Start 发给新服务。
func TestSessionReconfigureMemoryAtSwitch(t *testing.T) {
	oldMem, newMem := &fakeMemory{}, &fakeMemory{}
	prov := provider.NewFake(provider.FakeStep{Text: []string{"a"}}, provider.FakeStep{Text: []string{"b"}})
	s, _ := newTestSession(t, Config{Provider: prov, Memory: oldMem})
	waitUntil(t, "old start", func() bool { return oldMem.has("start:s1") })
	if _, err := s.Input(context.Background(), message.NewUser("1")); err != nil {
		t.Fatal(err)
	}

	if err := s.Reconfigure(Config{Provider: prov, Memory: newMem}, ""); err != nil {
		t.Fatal(err)
	}
	if len(newMem.snapshot()) != 0 {
		t.Fatalf("new memory used before switch: %v", newMem.snapshot())
	}
	if err := s.Switch(); err != nil {
		t.Fatal(err)
	}
	next := s.SessionID()
	if next == "s1" {
		t.Fatal("session id not switched")
	}
	waitUntil(t, "end on old, start on new", func() bool {
		return oldMem.has("end:s1") && newMem.has("start:"+next)
	})
	if oldMem.has("start:"+next) || newMem.has("end:s1") {
		t.Fatalf("calls went to the wrong service: old %v new %v", oldMem.snapshot(), newMem.snapshot())
	}

	// 同一服务再次 Reconfigure 不算变更。
	if err := s.Reconfigure(Config{Provider: prov, Memory: newMem}, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	if pending.memSet {
		t.Fatal("same memory service marked as pending")
	}
}

// Store 变更暂存到会话切换：切换后的事件写进新 store，旧 store 不再收到。
func TestSessionReconfigureStoreAtSwitch(t *testing.T) {
	oldStore, newStore := &memStore{}, &memStore{}
	prov := provider.NewFake(provider.FakeStep{Text: []string{"a"}}, provider.FakeStep{Text: []string{"b"}})
	s, _ := newTestSession(t, Config{Provider: prov, Store: oldStore})
	if _, err := s.Input(context.Background(), message.NewUser("1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconfigure(Config{Provider: prov, Store: newStore}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "old store drained", func() bool { return oldStore.has(loop.KindAgentEnd) })
	oldN := oldStore.count()

	if _, err := s.Input(context.Background(), message.NewUser("2")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "new store receives the next run", func() bool { return newStore.has(loop.KindAgentEnd) })
	if oldStore.count() != oldN {
		t.Fatalf("old store still receiving events: %d → %d", oldN, oldStore.count())
	}
	if s.Err() != nil {
		t.Fatalf("session failed after store swap: %v", s.Err())
	}
}

// Switch 清空窗口：新会话的第一次请求不带上一会话的内容。
func TestSessionSwitchClearsWindow(t *testing.T) {
	s, rp := newTestSession(t, Config{
		Provider:   provider.NewFake(provider.FakeStep{Text: []string{"a"}}, provider.FakeStep{Text: []string{"b"}}),
		Compressor: window.KeepLast(100),
	})
	if _, err := s.Input(context.Background(), message.NewUser("旧话")); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Input(context.Background(), message.NewUser("新话")); err != nil {
		t.Fatal(err)
	}
	if got := rendered(rp.at(1).Messages); len(got) != 1 || got[0] != "user:新话" {
		t.Fatalf("request after switch = %v, want only the new input", got)
	}
	_ = s.Close()
	if err := s.Switch(); err == nil {
		t.Fatal("Switch after Close should fail")
	}
}
