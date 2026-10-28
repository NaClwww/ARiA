package assemble

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"aria/core/provider"
	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/runtime/agent"
)

// liveFixture 建一个会话与 Live：Build 按 cfg.Provider.Model 从 provs 里取 Fake provider。
type liveFixture struct {
	sess   *agent.Session
	live   *Live
	logs   *bytes.Buffer
	builds int
	provs  map[string]*provider.Fake
	fail   error // 非 nil 时 Build 返回它
}

func newLiveFixture(t *testing.T, cur config.Config) *liveFixture {
	t.Helper()
	f := &liveFixture{logs: &bytes.Buffer{}, provs: map[string]*provider.Fake{}}
	build := func(cfg config.Config) (agent.Config, string, error) {
		f.builds++
		if f.fail != nil {
			return agent.Config{}, "", f.fail
		}
		p, ok := f.provs[cfg.Provider.Model]
		if !ok {
			p = provider.NewFake(provider.FakeStep{Text: []string{cfg.Provider.Model}})
			f.provs[cfg.Provider.Model] = p
		}
		return agent.Config{Provider: p}, "model=" + cfg.Provider.Model, nil
	}
	ac, _, err := build(cur)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	f.sess, err = ag.NewSession(ctxx.Scope{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.sess.Close() })
	f.live = NewLive(f.sess, build, cur, slog.New(slog.NewTextHandler(f.logs, nil)))
	return f
}

func (f *liveFixture) reply(t *testing.T) string {
	t.Helper()
	if _, err := f.sess.Input(context.Background(), message.NewUser("hi")); err != nil {
		t.Fatal(err)
	}
	_, recent := f.sess.History()
	return recent[len(recent)-1].Text()
}

// 配置变化：用 Build 整份重新装配，下一轮起生效；相同配置不重复装配；装配失败保持原配置。
func TestLiveRebuildsOnChange(t *testing.T) {
	cfg := config.Config{Provider: config.Provider{Model: "a"}}
	f := newLiveFixture(t, cfg)

	f.live.Apply(cfg)
	if f.builds != 1 {
		t.Fatalf("builds after identical Apply = %d, want 1 (startup only)", f.builds)
	}

	cfg.Provider.Model = "b"
	f.live.Apply(cfg)
	if got := f.reply(t); got != "b" {
		t.Fatalf("reply after reconfigure = %q, want from provider b", got)
	}

	f.fail = errors.New("boom")
	cfg.Provider.Model = "c"
	f.live.Apply(cfg)
	if !strings.Contains(f.logs.String(), "重新装配失败") {
		t.Fatalf("missing failure log:\n%s", f.logs)
	}
	if _, ok := f.provs["c"]; ok {
		t.Fatal("provider c built despite failure")
	}
}

// 只在启动时生效的段：记「需重启」；会话里热生效的项（idle / recall_wait）不算。
func TestLiveRestartOnlyWarnings(t *testing.T) {
	cfg := config.Config{Provider: config.Provider{Model: "a"}}
	f := newLiveFixture(t, cfg)
	cfg.Memory.Engine = "hindsight"
	cfg.Session.IdleTimeoutS = 60
	cfg.Session.RecallWaitMs = 100
	f.live.Apply(cfg)
	out := f.logs.String()
	if !strings.Contains(out, "section=memory") || strings.Contains(out, "section=session") {
		t.Fatalf("restart-only logs wrong:\n%s", out)
	}
}

// ProviderCache：[provider] 不变复用实例，变了重建；装配失败不替换缓存。
func TestProviderCache(t *testing.T) {
	c := &ProviderCache{APIKey: "k"}
	cfg := config.Config{Provider: config.Provider{Kind: "openai", BaseURL: "http://127.0.0.1:1", Model: "m"}}
	r1, err := c.Get(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLM.MaxTokens = 10 // 非 [provider] 段
	if r2, _ := c.Get(cfg); r2.Impl != r1.Impl {
		t.Fatal("rebuilt although [provider] unchanged")
	}
	cfg.Provider.Model = "m2"
	r3, err := c.Get(cfg)
	if err != nil || r3.Impl == r1.Impl || c.Model() != "m2" {
		t.Fatalf("not rebuilt on change: %v, model %q", err, c.Model())
	}
	cfg.Provider.Kind = "nope"
	if _, err := c.Get(cfg); err == nil {
		t.Fatal("want error for unknown kind")
	}
	if c.Model() != "m2" {
		t.Fatalf("cache replaced on failure: %q", c.Model())
	}
}
