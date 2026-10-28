// Package opencode 实现 OpenCode Console 的 Provider 适配器（docs/04 §2），覆盖两种计费方式：
//
//   - Zen（按量付费）：https://opencode.ai/inference/openai/v1
//   - Go（订阅制，Go / Go Plus）：https://opencode.ai/inference/go/openai/v1
//
// 两者都走 OpenAI 兼容的 chat/completions（Console 在各 API 风格间互译，Claude/GPT 等
// 原生走 messages/responses 的模型也可经此调用），因此请求与 SSE 翻译完全复用
// plugins/provider/openai；本包只做 OpenCode 要求的请求头：
//
//   - User-Agent：用客户端自己的标识，不用通用 SDK/HTTP 库名；
//   - x-opencode-session：每个会话稳定的标识（路由与提示词缓存用），取 ctx Scope 的
//     SessionID——ARiA 会话切换后自然换新标识。
//
// 请求头在 http.RoundTripper 层按请求 ctx 注入（openai 适配器以 NewRequestWithContext
// 发请求，ctx 原样到达）。
//
// 模型卡：OpenCode 的 /v1/models 只给模型 ID，元数据取自 pkg/modelcard（models.dev，
// provider 键 opencode / opencode-go）。New 在后台拉取一次，Limits / Card 不阻塞——拉取
// 完成前（或失败时）当作未知，agent 每轮重新询问 Limits，拉到后下一轮起按上下文用量压缩。
// Config 的 ContextWindow / MaxOutput 非零时优先。
package opencode

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/modelcard"
	openai "aria/plugins/provider/openai"
)

// Plan 是 OpenCode 的计费方式，决定默认端点。
type Plan string

const (
	PlanZen Plan = "zen" // 按量付费
	PlanGo  Plan = "go"  // 订阅制
)

const (
	ZenBaseURL = "https://opencode.ai/inference/openai/v1"
	GoBaseURL  = "https://opencode.ai/inference/go/openai/v1"

	DefaultUserAgent = "aria/1.0"
	SessionHeader    = "x-opencode-session"

	catalogTimeout = 30 * time.Second
)

type Config struct {
	Plan      Plan   // 空 = PlanZen
	BaseURL   string // 空 = Plan 对应的默认端点
	APIKey    string // 兜底；优先读 ctx Credentials 的 "opencode" 键
	Model     string // Request.Options.Model 可覆盖
	UserAgent string // 空 = DefaultUserAgent
	HTTP      *http.Client

	// ContextWindow / MaxOutput 覆盖 Model 这一个模型的模型卡限额（Card / Limits 都返回覆盖后
	// 的值）；0 = 用模型卡。经 Request.Options.Model 用到的其他模型不受影响，按各自的卡。
	ContextWindow int
	MaxOutput     int

	// CatalogURL 是模型卡目录地址：空 = modelcard.DefaultURL；"-" = 不拉取
	// （Card 只有 ID 与上面两项配置值）。
	CatalogURL string
	Log        *slog.Logger // 模型卡拉取结果的日志；nil = slog.Default()
}

// Adapter 是 OpenCode 适配器：请求与流式翻译由内嵌的 openai.Adapter 完成，Limits 取自生效模型卡。
type Adapter struct {
	*openai.Adapter
	cfg      Config
	override modelcard.Limit                 // 配置的限额，非零项覆盖模型卡
	cards    atomic.Pointer[modelcard.Cards] // nil = 模型卡尚未就绪
}

// New 返回 OpenCode 适配器，并在后台拉取模型卡（CatalogURL = "-" 时不拉取）。
func New(cfg Config) *Adapter {
	base := cfg.BaseURL
	if base == "" {
		base = ZenBaseURL
		if cfg.Plan == PlanGo {
			base = GoBaseURL
		}
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	hc := &http.Client{Timeout: 5 * time.Minute} // 与 openai 默认一致
	if cfg.HTTP != nil {
		c := *cfg.HTTP
		hc = &c
	}
	next := hc.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	hc.Transport = headerTransport{next: next, userAgent: ua}

	a := &Adapter{
		Adapter: openai.New(openai.Config{
			BaseURL:  base,
			APIKey:   cfg.APIKey,
			CredName: "opencode",
			Model:    cfg.Model,
			HTTP:     hc,
		}),
		cfg:      cfg,
		override: modelcard.Limit{Context: cfg.ContextWindow, Output: cfg.MaxOutput},
	}
	if cfg.CatalogURL != "-" {
		go a.loadCatalog()
	}
	return a
}

// Card 返回 model 的生效模型卡：model 为 Config.Model 时目录卡叠加配置的 ContextWindow /
// MaxOutput（非零项优先，见 modelcard.Card.Override），其他模型返回目录原卡；model 为空指
// Config.Model。第二个返回值报告目录中是否有该
// 模型——没有（含尚未就绪）时返回的卡只有 ID 与配置的限额。
func (a *Adapter) Card(model string) (modelcard.Card, bool) {
	if model == "" {
		model = a.cfg.Model
	}
	c, ok := a.catalogCard(model)
	if !ok {
		c = modelcard.Card{ID: model}
	}
	if model == a.cfg.Model {
		c = c.Override(a.override)
	}
	return c, ok
}

// catalogCard 返回目录中的原始模型卡（不叠加配置）。
func (a *Adapter) catalogCard(model string) (modelcard.Card, bool) {
	cards := a.cards.Load()
	if cards == nil {
		return modelcard.Card{}, false
	}
	c, ok := (*cards)[model]
	return c, ok
}

// Limits 报告 model 的生效限额（取自 Card）；model 为空指 Config.Model。
func (a *Adapter) Limits(model string) provider.Limits {
	c, _ := a.Card(model)
	return provider.Limits{ContextWindow: c.Limit.Context, MaxOutput: c.Limit.Output}
}

// catalogKey 是 Plan 在模型卡目录中的 provider 键。
func (p Plan) catalogKey() string {
	if p == PlanGo {
		return "opencode-go"
	}
	return "opencode"
}

func (a *Adapter) loadCatalog() {
	log := a.cfg.Log
	if log == nil {
		log = slog.Default()
	}
	url := a.cfg.CatalogURL
	if url == "" {
		url = modelcard.DefaultURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	cards, err := modelcard.Fetch(ctx, nil, url, a.cfg.Plan.catalogKey())
	if err != nil {
		log.Warn("opencode: 模型卡拉取失败，未配置 context_window 时不按上下文用量压缩", "url", url, "err", err)
		return
	}
	a.cards.Store(&cards)
	if raw, ok := cards[a.cfg.Model]; ok {
		// 记生效值（配置非零项覆盖模型卡），并附目录原值便于对照。
		c, _ := a.Card("")
		log.Info("opencode: 模型卡已获取", "model", a.cfg.Model,
			"context_window", c.Limit.Context, "max_output", c.Limit.Output,
			"card_context", raw.Limit.Context, "card_output", raw.Limit.Output, "input", c.Modalities.Input)
	} else {
		log.Warn("opencode: 目录中没有该模型的模型卡", "model", a.cfg.Model, "provider", a.cfg.Plan.catalogKey())
	}
}

// headerTransport 注入 OpenCode 要求的请求头。
type headerTransport struct {
	next      http.RoundTripper
	userAgent string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context()) // RoundTripper 不得改动调用方的请求
	req.Header.Set("User-Agent", t.userAgent)
	if s, ok := ctxx.ScopeFrom(req.Context()); ok {
		req.Header.Set(SessionHeader, s.SessionID)
	}
	return t.next.RoundTrip(req)
}
