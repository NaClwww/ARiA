// Package systemone 是决策模型 API 的共享线上格式（openrouter / typesafe 两个适配器共用）。
//
// TypeSafe 把这类模型与接口叫 System One（取「系统 1」：快速、直觉、不写推理），路径 /v1/systemone；
// OpenRouter 把同一格式叫 Decisions API；别家决策模型（Liquid、Upstage、Inception……）也照这份格式
// 提供服务。两个端点相同的只有请求里的 questions 编码与响应里的 answers 解码，放在这里；URL、
// 模型名、请求附加字段、错误体、请求 ID 的来源各不相同，归各自的适配器。
//
// 另有一个公共的 Post：发请求、按状态码分类错误（错误体由适配器解析）、解码 2xx 响应。不重试——
// 注意力层每车只有 300–500ms，可重试错误包成 provider.RetryableError 交给调用方按兜底判。
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aria/core/provider"
)

const (
	maxErrorBody    = 4 << 10
	maxResponseBody = 4 << 20
)

// ---------- 问题 ----------

// Question 是一个问题的线上形状。
type Question struct {
	Type         provider.QuestionKind `json:"type"`
	Instructions string                `json:"instructions,omitempty"`
	Criteria     any                   `json:"criteria,omitempty"`
}

// Questions 把已校验的问题集编码为 {问题名: Question}。
func Questions(qs []provider.Question) map[string]Question {
	m := make(map[string]Question, len(qs))
	for _, q := range qs {
		m[q.Name] = Question{Type: q.Kind, Instructions: q.Instructions, Criteria: criteria(q)}
	}
	return m
}

// criteria 按题型编码：
//   - choice：{选项名: 描述}，描述为空时为 null（模型按名字理解）；
//   - score：按档位顺序的描述数组（下标即档位），描述为空时用档位名——数组元素不能为 null；
//   - noul：{"true": 描述, "false": 描述}，两项须成对；描述都空时省略。
func criteria(q provider.Question) any {
	switch q.Kind {
	case provider.KindChoice:
		m := make(map[string]*string, len(q.Criteria))
		for _, c := range q.Criteria {
			if c.Description == "" {
				m[c.Name] = nil
			} else {
				d := c.Description
				m[c.Name] = &d
			}
		}
		return m
	case provider.KindScore:
		levels := make([]string, len(q.Criteria))
		for i, c := range q.Criteria {
			levels[i] = c.Description
			if levels[i] == "" {
				levels[i] = c.Name
			}
		}
		return levels
	case provider.KindNoul:
		described := false
		for _, c := range q.Criteria {
			described = described || c.Description != ""
		}
		if !described {
			return nil
		}
		m := make(map[string]string, 2)
		for _, c := range q.Criteria {
			m[c.Name] = c.Description
			if m[c.Name] == "" {
				m[c.Name] = c.Name
			}
		}
		return m
	}
	return nil
}

// ---------- 答案 ----------

// Usage 是响应里的用量；Cost 只有 OpenRouter 给（美元）。
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// answer 覆盖三种题型；指针区分「缺字段」与零值。probabilities / confidence 在协议里都是可选的。
type answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        *string            `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Answers 按提问顺序解码并规范化答案；漏答、类型不符、缺必需字段、经 NormalizeAnswer 拒收的
// 问题记进 missing。没被问到的答案（含 box 与将来新增的题型）忽略。
func Answers(qs []provider.Question, raw map[string]json.RawMessage) (map[string]provider.Answer, []string) {
	out := make(map[string]provider.Answer, len(qs))
	var missing []string
	for _, q := range qs {
		r, ok := toRaw(q, raw[q.Name])
		if ok {
			var a provider.Answer
			if a, ok = provider.NormalizeAnswer(q, r); ok {
				out[q.Name] = a
				continue
			}
		}
		missing = append(missing, q.Name)
	}
	return out, missing
}

// toRaw 把线上答案转成 NormalizeAnswer 的原始输入。score 的分布在线上按下标字符串作键，
// 这里按发出的 criteria 顺序换算为档位名（不依赖 legend：legend 的值是我们发出的描述，不是名字）。
func toRaw(q provider.Question, b json.RawMessage) (provider.Answer, bool) {
	if len(b) == 0 {
		return provider.Answer{}, false
	}
	var w answer
	if err := json.Unmarshal(b, &w); err != nil || provider.QuestionKind(w.Type) != q.Kind {
		return provider.Answer{}, false
	}
	raw := provider.Answer{Kind: q.Kind}
	if w.Confidence != nil {
		raw.Confidence = *w.Confidence // 只在厂商没给分布时用到
	}
	switch q.Kind {
	case provider.KindNoul:
		if w.Noul == nil {
			return provider.Answer{}, false
		}
		raw.P = *w.Noul
	case provider.KindChoice:
		if w.Choice == nil && len(w.Probabilities) == 0 {
			return provider.Answer{}, false
		}
		if w.Choice != nil {
			raw.Choice = *w.Choice
		}
		raw.Probs = w.Probabilities
	case provider.KindScore:
		if w.Score == nil && len(w.Probabilities) == 0 {
			return provider.Answer{}, false
		}
		if w.Score != nil {
			raw.Score = *w.Score
		}
		if len(w.Probabilities) > 0 {
			raw.Probs = make(map[string]float64, len(w.Probabilities))
			for k, p := range w.Probabilities {
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(q.Criteria) {
					return provider.Answer{}, false
				}
				raw.Probs[q.Criteria[i].Name] = p
			}
		}
	}
	return raw, true
}

// ---------- HTTP ----------

// APIError 是端点返回的非 2xx 响应。可重试的状态（408/429/5xx）外面另包 provider.RetryableError。
type APIError struct {
	Endpoint   string // "openrouter" / "typesafe"
	Status     int
	Message    string        // 从错误体提取；提取不到时为截断的原文
	RequestID  string        // 有则带上，便于向厂商报障
	RetryAfter time.Duration // retry-after-ms / retry-after 响应头；0 = 未给（供上层限流参考）
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s: status %d: %s", e.Endpoint, e.Status, e.Message)
	if e.RequestID != "" {
		s += " (request_id=" + e.RequestID + ")"
	}
	return s
}

// Retryable 报告状态码是否值得重试：408、429、5xx（含 OpenRouter 的 524 边缘超时、529 上游过载）。
func Retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// Call 是一次 POST 的参数。
type Call struct {
	Endpoint string // 错误前缀
	URL      string
	Header   http.Header // 鉴权、UA 等；Content-Type / Accept 由 Post 设
	Body     any         // JSON 编码后发出；Raw 非空时忽略
	// Raw / ContentType：已编码好的请求体（multipart 等）；空 = 发 Body 的 JSON。
	Raw         []byte
	ContentType string
	ErrorText   func(body []byte) string // 从错误体提取信息（各端点形状不同）
	RequestID   func(h http.Header) string
}

// Post 发请求并把 2xx 响应解码进 out；返回响应头（取请求 ID 等）。
// 连接失败 → RetryableError；ctx 取消 / 超时 → ctx.Err()；非 2xx → *APIError（可重试的另包一层）。
func Post(ctx context.Context, hc *http.Client, c Call, out any) (http.Header, error) {
	body, contentType := c.Raw, c.ContentType
	if body == nil {
		var err error
		if body, err = json.Marshal(c.Body); err != nil {
			return nil, fmt.Errorf("%s: 请求无法 JSON 编码: %w", c.Endpoint, err)
		}
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range c.Header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, provider.RetryableError{Err: fmt.Errorf("%s: %w", c.Endpoint, err)} // 连接层故障
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		msg := c.ErrorText(b)
		if msg == "" {
			msg = Snippet(b)
		}
		e := &APIError{Endpoint: c.Endpoint, Status: resp.StatusCode, Message: msg, RetryAfter: retryAfter(resp.Header)}
		if c.RequestID != nil {
			e.RequestID = c.RequestID(resp.Header)
		}
		if Retryable(resp.StatusCode) {
			return resp.Header, provider.RetryableError{Err: e}
		}
		return resp.Header, e
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return resp.Header, fmt.Errorf("%s: 响应无法解码: %w", c.Endpoint, err)
	}
	return resp.Header, nil
}

// retryAfter 读 retry-after-ms（毫秒）或 retry-after（秒；HTTP 日期形式忽略）。
func retryAfter(h http.Header) time.Duration {
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("retry-after-ms")), 64); err == nil && v > 0 {
		return time.Duration(v * float64(time.Millisecond))
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("retry-after")), 64); err == nil && v > 0 {
		return time.Duration(v * float64(time.Second))
	}
	return 0
}

// Snippet 返回截断的原文（错误体不是预期形状时用）。
func Snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(无响应体)"
	}
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
