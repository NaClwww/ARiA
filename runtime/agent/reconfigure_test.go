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
