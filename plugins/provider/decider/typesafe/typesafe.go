// Package typesafe 是 TypeSafe 官方 System One API（POST {base}/v1/systemone）的 Decider 适配器。
// 自部署的开源决策模型（Clef、Kev 等暴露同一路径）也用它，换 BaseURL 即可。
//
// 与 OpenRouter（plugins/provider/decider/openrouter）的差别，都在本包处理：
//
//   - 模型名不带前缀：jev-latest / jev-preview 是官方文档里的别名，具体版本名以 GET /v1/models 为准；
//   - 请求 ID 在响应头 x-typesafe-request-id（响应体没有 id）；usage 只有 token 数，没有花费；
//   - 错误体是 FastAPI 风格：400 {detail: 字符串或对象}，422 {detail: [{loc, msg, type}]}；
//     key 无效实际回 403（文档写 401）；429 可能带 retry-after-ms / retry-after；
//   - 长上下文（单请求 64k → 262k token，更慢、准确率更低）要显式带 TypeSafe-Danger-Zone-Long-Context
//     请求头，不会自动切换（OpenRouter 会）；
//   - 图片（官方标为原型阶段）要带 TypeSafe-Danger-Zone-Images 请求头；按官方推荐走 multipart：
//     第一段 request 是 JSON，assets 里每张图用 part 指向后面的文件段（比 base64 小三分之一，
//     请求体上限 32 MiB）。
//
// 请求头约定参照官方 Python SDK（typesafe-ai/typesafe-sdk-python）。编解码见 systemone 包；不重试。
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/plugins/provider/decider/systemone"
)

const (
	DefaultBaseURL   = "https://api.typesafe.ai"
	DefaultModel     = "jev-latest"
	DefaultCredName  = "typesafe"
	DefaultUserAgent = "aria/1.0"
	Endpoint         = "typesafe"

	Path              = "/v1/systemone"
	RequestIDHeader   = "x-typesafe-request-id"
	LongContextHeader = "TypeSafe-Danger-Zone-Long-Context"
	ImagesHeader      = "TypeSafe-Danger-Zone-Images"
)

type Config struct {
	BaseURL   string // 空 = DefaultBaseURL；请求发往 BaseURL + Path
	APIKey    string // 兜底；优先读 ctx Credentials[CredName]
	CredName  string // 空 = DefaultCredName
	Model     string // 空 = DefaultModel
	UserAgent string // 空 = DefaultUserAgent
	HTTP      *http.Client

	// LongContext 为 true 时每个请求都带长上下文头。注意力层的 state 远小于默认上限，通常不需要。
	LongContext bool

	// Caps 是模型能力；零值 = 未知，只按协议上限校验。
	Caps provider.DeciderCaps
}

// Adapter 是 TypeSafe 官方端点的 Decider。并发安全（无可变状态）。
type Adapter struct {
	cfg Config
	url string
	hc  *http.Client
}

var _ provider.Decider = (*Adapter)(nil)

// APIError 是非 2xx 响应（可重试的外面另包 provider.RetryableError）。
type APIError = systemone.APIError

func New(cfg Config) *Adapter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
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
	cfg.Caps.Kinds = append([]provider.QuestionKind(nil), cfg.Caps.Kinds...)
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second} // 兜底；实际时限由调用方 ctx 决定
	}
	return &Adapter{cfg: cfg, url: strings.TrimSuffix(cfg.BaseURL, "/") + Path, hc: hc}
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
}

// asset 指向 multipart 里的一个文件段（format 由服务端按字节识别，可省）。
type asset struct {
	Type string `json:"type"` // "image"
	Part string `json:"part"`
}

type response struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   systemone.Usage            `json:"usage"`
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
	call := systemone.Call{
		Endpoint:  Endpoint,
		URL:       a.url,
		Body:      request{Model: a.cfg.Model, State: state, Questions: systemone.Questions(qs)},
		ErrorText: errorText,
		RequestID: func(h http.Header) string { return h.Get(RequestIDHeader) },
	}
	h := http.Header{}
	h.Set("User-Agent", a.cfg.UserAgent)
	if key := a.apiKey(ctx); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	if a.cfg.LongContext {
		h.Set(LongContextHeader, "true")
	}
	if len(images) > 0 {
		h.Set(ImagesHeader, "true")
		req := call.Body.(request)
		if call.Raw, call.ContentType, err = multipartBody(req, images); err != nil {
			return provider.Decision{}, err
		}
	}
	call.Header = h

	var resp response
	hdr, err := systemone.Post(ctx, a.hc, call, &resp)
	if err != nil {
		return provider.Decision{}, err
	}
	answers, missing := systemone.Answers(qs, resp.Answers)
	d := provider.Decision{
		Answers: answers,
		Missing: missing,
		Model:   resp.Model,
		ID:      hdr.Get(RequestIDHeader),
		Usage:   message.Usage{In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens},
	}
	if d.Model == "" {
		d.Model = a.cfg.Model
	}
	return d, nil
}

// multipartBody 编码官方推荐的 multipart 请求：request 段是 JSON（assets 指向文件段），
// 之后每张图一个文件段 asset-<i>。
func multipartBody(req request, images []systemone.Image) ([]byte, string, error) {
	req.Assets = make(map[string]asset, len(images))
	for i, img := range images {
		req.Assets[img.Ref] = asset{Type: "image", Part: partName(i)}
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="request"`)
	h.Set("Content-Type", "application/json")
	pw, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if err := json.NewEncoder(pw).Encode(req); err != nil {
		return nil, "", fmt.Errorf("typesafe: 请求无法 JSON 编码: %w", err)
	}
	for i, img := range images {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s.%s"`, partName(i), partName(i), img.Format))
		h.Set("Content-Type", img.MIME)
		pw, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := pw.Write(img.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func partName(i int) string { return fmt.Sprintf("asset-%d", i) }

func (a *Adapter) apiKey(ctx context.Context) string {
	if k, ok := ctxx.CredentialFrom(ctx, a.cfg.CredName); ok && k != "" {
		return k
	}
	return a.cfg.APIKey
}

// errorText 解析 FastAPI 风格的错误体：{"detail": "…"} / {"detail": {"message": "…"}} /
// {"detail": [{"loc": ["body", …], "msg": "…"}]}（422，路径去掉 body 前缀后拼成 a.b.c: msg）。
func errorText(b []byte) string {
	var body struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(b, &body) != nil || len(body.Detail) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(body.Detail, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body.Detail, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var list []struct {
		Loc []any  `json:"loc"`
		Msg string `json:"msg"`
	}
	if json.Unmarshal(body.Detail, &list) != nil {
		return ""
	}
	var parts []string
	for _, e := range list {
		if e.Msg == "" {
			continue
		}
		var path []string
		for _, l := range e.Loc {
			if l != "body" {
				path = append(path, fmt.Sprint(l))
			}
		}
		if len(path) > 0 {
			parts = append(parts, strings.Join(path, ".")+": "+e.Msg)
		} else {
			parts = append(parts, e.Msg)
		}
	}
	return strings.Join(parts, "; ")
}
