// Package openrouter 是 OpenRouter Decisions API（/api/alpha/decisions）的 Decider 适配器。
// 约 30 个决策模型（Jev、Perplexity Decider、Liquid d1、Clef……）都走这一个端点，换 Model 即可。
//
// 与 TypeSafe 官方端点（plugins/provider/decider/typesafe）的差别，都在本包处理：
//
//   - 模型名带厂商前缀：typesafe/jev-1.14；~typesafe/jev-latest 是跟随最新版的别名（不建议，结果会随升级漂移）；
//   - 响应体带 id（gen-dec-…）与 usage.cost（美元）；
//   - 错误体是 {error:{code,message,metadata}}，metadata 里可能有上游 provider 名与原始错误；
//     另有 402（余额不足）、524（边缘超时）、529（上游过载）；
//   - 请求可带 provider 路由偏好、session_id（只用于 OpenRouter 侧观测，不转发给模型方）；
//   - 图片只能以 base64 放进 assets（不支持 multipart，不需要特殊请求头）；Jev 以外的模型只认
//     State 顶层、整个值就是引用串的写法（见 provider.DecideInput）。
//
// 隐私：state 里是家里人说的话，默认要求零数据保留（zdr）且不允许上游拿去训练（data_collection=deny）。
// 代价是可选的上游变少，个别模型可能因此无可用上游——Config.AllowDataRetention 显式关掉。
//
// 编解码见 systemone 包；不重试（见 systemone.Post）。
package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/plugins/provider/decider/systemone"
)

const (
	DefaultURL       = "https://openrouter.ai/api/alpha/decisions"
	DefaultModel     = "typesafe/jev-1.14" // 钉住版本，升级显式改
	DefaultCredName  = "openrouter"
	DefaultUserAgent = "aria/1.0"
	Endpoint         = "openrouter"
)

type Config struct {
	URL       string // 空 = DefaultURL
	APIKey    string // 兜底；优先读 ctx Credentials[CredName]
	CredName  string // 空 = DefaultCredName
	Model     string // 空 = DefaultModel
	UserAgent string // 空 = DefaultUserAgent
	HTTP      *http.Client

	// AllowDataRetention 为 true 时不发隐私偏好（zdr / data_collection），任由 OpenRouter 路由到任意上游。
	AllowDataRetention bool
	// Only 限定上游 provider（如 ["typesafe"]）；空 = 不限。
	Only []string

	// Caps 是模型能力；零值 = 未知，只按协议上限校验。以后从 OpenRouter 模型目录补上下文与收图能力。
	Caps provider.DeciderCaps
}

// Adapter 是 OpenRouter 的 Decider。并发安全（无可变状态）。
type Adapter struct {
	cfg Config
	hc  *http.Client
}

var _ provider.Decider = (*Adapter)(nil)

// APIError 是非 2xx 响应（可重试的外面另包 provider.RetryableError）。
type APIError = systemone.APIError

func New(cfg Config) *Adapter {
	if cfg.URL == "" {
		cfg.URL = DefaultURL
	}
	if cfg.CredName == "" {
		cfg.CredName = DefaultCredName
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	cfg.Only = append([]string(nil), cfg.Only...)
	cfg.Caps.Kinds = append([]provider.QuestionKind(nil), cfg.Caps.Kinds...)
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second} // 兜底；实际时限由调用方 ctx 决定
	}
	return &Adapter{cfg: cfg, hc: hc}
}

// Caps 返回 Config.Caps（与 model 无关）。
func (a *Adapter) Caps(string) provider.DeciderCaps {
	c := a.cfg.Caps
	c.Kinds = append([]provider.QuestionKind(nil), a.cfg.Caps.Kinds...)
	return c
}

// Model 返回配置的模型名（日志用）。
func (a *Adapter) Model() string { return a.cfg.Model }

type request struct {
	Model     string                        `json:"model"`
	State     json.RawMessage               `json:"state"`
	Questions map[string]systemone.Question `json:"questions"`
	Assets    map[string]asset              `json:"assets,omitempty"`
	Provider  *preferences                  `json:"provider,omitempty"`
	SessionID string                        `json:"session_id,omitempty"`
}

type asset struct {
	Type       string `json:"type"`   // "image"
	Format     string `json:"format"` // png | jpeg
	DataBase64 string `json:"data_base64"`
}

type preferences struct {
	ZDR            *bool    `json:"zdr,omitempty"`
	DataCollection string   `json:"data_collection,omitempty"`
	Only           []string `json:"only,omitempty"`
}

type response struct {
	ID       string                     `json:"id"`
	Model    string                     `json:"model"`
	Provider string                     `json:"provider"`
	Answers  map[string]json.RawMessage `json:"answers"`
	Usage    systemone.Usage            `json:"usage"`
}

func (a *Adapter) Decide(ctx context.Context, in provider.DecideInput, qs []provider.Question) (provider.Decision, error) {
	state, err := provider.ValidateInput(in, qs, a.Caps(""))
	if err != nil {
		return provider.Decision{}, err
	}
	images, err := systemone.PrepareImages(in.Images)
	if err != nil {
		return provider.Decision{}, err
	}
	body := request{Model: a.cfg.Model, State: state, Questions: systemone.Questions(qs), Provider: a.preferences()}
	if len(images) > 0 {
		body.Assets = make(map[string]asset, len(images))
		for _, img := range images {
			body.Assets[img.Ref] = asset{Type: "image", Format: img.Format, DataBase64: base64.StdEncoding.EncodeToString(img.Data)}
		}
	}
	if s, ok := ctxx.ScopeFrom(ctx); ok && len(s.SessionID) <= 256 { // 协议上限 256，超了不发
		body.SessionID = s.SessionID
	}
	h := http.Header{}
	h.Set("User-Agent", a.cfg.UserAgent)
	if key := a.apiKey(ctx); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}

	var resp response
	if _, err := systemone.Post(ctx, a.hc, systemone.Call{
		Endpoint: Endpoint, URL: a.cfg.URL, Header: h, Body: body, ErrorText: errorText,
	}, &resp); err != nil {
		return provider.Decision{}, err
	}
	answers, missing := systemone.Answers(qs, resp.Answers)
	d := provider.Decision{
		Answers: answers,
		Missing: missing,
		Model:   resp.Model,
		ID:      resp.ID,
		Usage:   message.Usage{In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens, Cost: resp.Usage.Cost},
	}
	if d.Model == "" {
		d.Model = a.cfg.Model
	}
	return d, nil
}

func (a *Adapter) preferences() *preferences {
	p := &preferences{Only: a.cfg.Only}
	if !a.cfg.AllowDataRetention {
		zdr := true
		p.ZDR, p.DataCollection = &zdr, "deny"
	}
	if p.ZDR == nil && len(p.Only) == 0 {
		return nil
	}
	return p
}

func (a *Adapter) apiKey(ctx context.Context) string {
	if k, ok := ctxx.CredentialFrom(ctx, a.cfg.CredName); ok && k != "" {
		return k
	}
	return a.cfg.APIKey
}

// errorText 解析 {error:{code,message,metadata}}；metadata 里有上游 provider 名时附上（定位是哪家出错）。
func errorText(b []byte) string {
	var body struct {
		Error struct {
			Message  string `json:"message"`
			Metadata struct {
				ProviderName string `json:"provider_name"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &body) != nil || body.Error.Message == "" {
		return ""
	}
	if p := body.Error.Metadata.ProviderName; p != "" {
		return body.Error.Message + " [" + p + "]"
	}
	return body.Error.Message
}
