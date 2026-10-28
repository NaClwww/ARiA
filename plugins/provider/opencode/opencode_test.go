package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

const okSSE = `data: {"choices":[{"index":0,"delta":{"content":"嗯"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"

// stream 发起一次流式请求并读到 MessageComplete。
func stream(t *testing.T, ctx context.Context, a provider.Provider) message.Message {
	t.Helper()
	ch, err := a.Stream(ctx, provider.Request{
		Messages: []message.Message{{Role: message.RoleUser, Blocks: []message.Block{message.TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for ev := range ch {
		switch e := ev.(type) {
		case provider.MessageComplete:
			return e.Message
		case provider.ErrorEvent:
			t.Fatalf("stream error: %v", e.Err)
		}
	}
	t.Fatal("stream closed without MessageComplete")
	return message.Message{}
}

// 请求头：User-Agent、x-opencode-session（取 Scope.SessionID）、Bearer key；路径为 chat/completions。
func TestHeaders(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "kimi-k3", CatalogURL: "-"})
	ctx := ctxx.WithScope(context.Background(), ctxx.Scope{SessionID: "aria-20261028-120000.000"})
	stream(t, ctx, a)

	if got.URL.Path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if ua := got.Header.Get("User-Agent"); ua != DefaultUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, DefaultUserAgent)
	}
	if s := got.Header.Get(SessionHeader); s != "aria-20261028-120000.000" {
		t.Errorf("%s = %q", SessionHeader, s)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer k" {
		t.Errorf("Authorization = %q", auth)
	}
}

// 无 Scope 时不带会话头；自定义 User-Agent 与 HTTP 客户端的 Transport 被保留。
func TestNoScopeCustomClient(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()

	var used bool
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}
	a := New(Config{BaseURL: srv.URL, Model: "m", UserAgent: "x/2", HTTP: hc, CatalogURL: "-"})
	stream(t, context.Background(), a)

	if !used {
		t.Error("custom transport not used")
	}
	if _, ok := hc.Transport.(headerTransport); ok {
		t.Error("caller's http.Client was modified")
	}
	if ua := got.Header.Get("User-Agent"); ua != "x/2" {
		t.Errorf("User-Agent = %q", ua)
	}
	if _, ok := got.Header[http.CanonicalHeaderKey(SessionHeader)]; ok {
		t.Errorf("unexpected %s without Scope", SessionHeader)
	}
}

const catalogJSON = `{
  "openai": {"models": {"gpt-x": {"limit": {"context": 1, "output": 1}}}},
  "opencode": {"models": {"glm-5.3-flash": {"limit": {"context": 1000000, "output": 131072}}}},
  "opencode-go": {"models": {
    "glm-5.3-flash": {"limit": {"context": 1000000, "output": 131072}},
    "kimi-k3": {"limit": {"context": 1048576, "output": 131072}}
  }}
}`

func catalogServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(catalogJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitLimits 等后台目录拉取把 model 的窗口填上。
func waitLimits(t *testing.T, a *Adapter, model string) provider.Limits {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lim := a.Limits(model); lim.ContextWindow > 0 {
			return lim
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Limits(%q) not filled from catalog", model)
	return provider.Limits{}
}

// 目录按 Plan 取对应 provider 键；model 为空指 Config.Model；Options.Model 覆盖的模型也能查。
func TestLimitsFromCatalog(t *testing.T) {
	srv := catalogServer(t, http.StatusOK)
	a := New(Config{Plan: PlanGo, Model: "kimi-k3", CatalogURL: srv.URL})
	if lim := waitLimits(t, a, ""); lim != (provider.Limits{ContextWindow: 1048576, MaxOutput: 131072}) {
		t.Errorf("Limits(\"\") = %+v", lim)
	}
	if lim := a.Limits("glm-5.3-flash"); lim.ContextWindow != 1000000 {
		t.Errorf("Limits(glm-5.3-flash) = %+v", lim)
	}
	if lim := a.Limits("nope"); lim != (provider.Limits{}) {
		t.Errorf("unknown model Limits = %+v, want zero", lim)
	}

	if c, ok := a.Card(""); !ok || c.Limit.Context != 1048576 {
		t.Errorf("Card(\"\") = %+v, %v", c, ok)
	}

	zen := New(Config{Model: "glm-5.3-flash", CatalogURL: srv.URL})
	waitLimits(t, zen, "")
	if lim := zen.Limits("kimi-k3"); lim != (provider.Limits{}) {
		t.Errorf("zen Limits(kimi-k3) = %+v, want zero (only in opencode-go)", lim)
	}
}

// 配置值优先于目录（两项独立覆盖）；CatalogURL="-" 只用配置值。
func TestLimitsConfigOverride(t *testing.T) {
	srv := catalogServer(t, http.StatusOK)
	a := New(Config{Plan: PlanGo, Model: "kimi-k3", CatalogURL: srv.URL, MaxOutput: 8192})
	if lim := waitLimits(t, a, ""); lim != (provider.Limits{ContextWindow: 1048576, MaxOutput: 8192}) {
		t.Errorf("Limits = %+v", lim)
	}
	if c, ok := a.Card(""); !ok || c.Limit.Context != 1048576 || c.Limit.Output != 8192 {
		t.Errorf("Card = %+v, %v; want catalog context + configured output", c.Limit, ok)
	}

	// 配置只作用于 Config.Model：其他模型按各自的卡。
	if lim := a.Limits("glm-5.3-flash"); lim != (provider.Limits{ContextWindow: 1000000, MaxOutput: 131072}) {
		t.Errorf("other model Limits = %+v, want catalog values", lim)
	}

	off := New(Config{Model: "kimi-k3", CatalogURL: "-", ContextWindow: 200000})
	if lim := off.Limits(""); lim != (provider.Limits{ContextWindow: 200000}) {
		t.Errorf("no-catalog Limits = %+v", lim)
	}
	if c, ok := off.Card(""); ok || c.ID != "kimi-k3" || c.Limit.Context != 200000 {
		t.Errorf("no-catalog Card = %+v, %v; want ID + configured limit, false", c, ok)
	}
}

// 模型卡就绪前 Card 为未知、Limits 只有配置值；拉取失败时保持如此。
func TestCardUnavailable(t *testing.T) {
	a := New(Config{Model: "glm-5.3-flash", CatalogURL: catalogServer(t, http.StatusInternalServerError).URL, ContextWindow: 1000})
	if _, ok := a.Card(""); ok {
		t.Error("Card ok before/without catalog")
	}
	if lim := a.Limits(""); lim != (provider.Limits{ContextWindow: 1000}) {
		t.Errorf("Limits = %+v", lim)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 真实端点冒烟：设 OPENCODE_API_KEY 才跑；OPENCODE_PLAN=go 走 Go 订阅，OPENCODE_MODEL 指定模型。
func TestLive(t *testing.T) {
	key := os.Getenv("OPENCODE_API_KEY")
	if key == "" {
		t.Skip("OPENCODE_API_KEY not set; skipping live test")
	}
	model := os.Getenv("OPENCODE_MODEL")
	if model == "" {
		model = "glm-5.3-flash" // 便宜、零留存；beam-free 有地区限制
	}
	a := New(Config{Plan: Plan(os.Getenv("OPENCODE_PLAN")), APIKey: key, Model: model})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = ctxx.WithScope(ctx, ctxx.Scope{SessionID: "aria-live-test"})
	msg := stream(t, ctx, a)
	var text strings.Builder
	for _, b := range msg.Blocks {
		if tb, ok := b.(message.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	if text.Len() == 0 {
		t.Fatal("empty reply")
	}
}
