// Package openai 实现 OpenAI 兼容的流式 Provider 适配器（docs/04 §2）。
// 兼容官方 OpenAI 及 DeepSeek/GLM/Kimi/Ollama 等同构端点。
//
// 关键义务（02 §5）：ctx 取消时必须以 MessageComplete{Interrupted:true}
// 收尾返回已收内容；残缺 tool calls 一律丢弃（A3 默认）。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

type Config struct {
	BaseURL  string // 默认 https://api.openai.com/v1
	APIKey   string // 兜底；优先读 ctx Credentials（01 §1.1）
	CredName string // ctxx.CredentialFrom 的键，默认 "openai"
	Model    string // 默认模型；Request.Options.Model 可覆盖
	HTTP     *http.Client
}

type Adapter struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Adapter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.CredName == "" {
		cfg.CredName = "openai"
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Adapter{cfg: cfg, hc: hc}
}

// Stream 发起流式 chat/completions 并把 SSE 分片翻译为 provider 事件。
func (a *Adapter) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if key := a.apiKey(ctx); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := a.hc.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, provider.RetryableError{Err: err} // 连接层故障默认可重试
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		e := fmt.Errorf("openai: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, provider.RetryableError{Err: e}
		}
		return nil, e
	}

	ch := make(chan provider.StreamEvent, 32)
	go a.read(ctx, resp, ch)
	return ch, nil
}

func (a *Adapter) apiKey(ctx context.Context) string {
	if k, ok := ctxx.CredentialFrom(ctx, a.cfg.CredName); ok && k != "" {
		return k
	}
	return a.cfg.APIKey
}

// ---------- SSE 读取与翻译 ----------

type pendingCall struct {
	id, name, args string
}

func (a *Adapter) read(ctx context.Context, resp *http.Response, ch chan<- provider.StreamEvent) {
	defer close(ch)
	defer resp.Body.Close()

	var text, thought strings.Builder
	calls := map[int]*pendingCall{}
	var order []int
	var usage message.Usage
	finish := ""
	done := false

	r := bufio.NewReader(resp.Body)
	for {
		if ctx.Err() != nil {
			break
		}
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
			ch <- provider.ErrorEvent{Err: fmt.Errorf("openai: stream read: %w", err), Retryable: true}
			return
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			break
		}
		var c wireChunk
		if json.Unmarshal([]byte(payload), &c) != nil {
			continue // 忽略无法解析的行（部分代理会夹带心跳）
		}
		if c.Usage != nil {
			usage = c.Usage.canonical()
		}
		for _, choice := range c.Choices {
			if s, ok := choice.FinishReason.(string); ok && s != "" {
				finish = s
			}
			d := choice.Delta
			if d.ReasoningContent != "" {
				thought.WriteString(d.ReasoningContent)
				ch <- provider.ThoughtDelta{Text: d.ReasoningContent}
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				ch <- provider.PartDelta{Text: d.Content}
			}
			for _, tc := range d.ToolCalls {
				p := calls[tc.Index]
				if p == nil {
					p = &pendingCall{}
					calls[tc.Index] = p
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name += tc.Function.Name
				}
				p.args += tc.Function.Arguments
			}
		}
	}

	if ctx.Err() != nil {
		// 部分结果义务：已收文本原样返回，残缺 tool calls 丢弃（A3 默认）
		ch <- provider.MessageComplete{Message: partial(text, thought), Interrupted: true}
		return
	}
	if !done && finish == "" {
		ch <- provider.ErrorEvent{Err: errors.New("openai: unexpected EOF before [DONE] or finish_reason"), Retryable: true}
		return
	}

	msg := message.Message{Role: message.RoleAssistant}
	if thought.Len() > 0 {
		msg.Blocks = append(msg.Blocks, message.ThoughtBlock{Text: thought.String()})
	}
	if text.Len() > 0 {
		msg.Blocks = append(msg.Blocks, message.TextBlock{Text: text.String()})
	}
	if finish == "tool_calls" || len(order) > 0 {
		for _, i := range order {
			p := calls[i]
			args := p.args
			if args == "" {
				args = "{}"
			}
			id := p.id
			if id == "" {
				id = fmt.Sprintf("call_%d", i) // 个别实现不回 id
			}
			msg.ToolCalls = append(msg.ToolCalls, message.ToolCall{ID: id, Name: p.name, Args: json.RawMessage(args)})
		}
	}
	ch <- provider.MessageComplete{Message: msg, Usage: usage}
}

func partial(text, thought strings.Builder) message.Message {
	m := message.Message{Role: message.RoleAssistant, Interrupted: true}
	if thought.Len() > 0 {
		m.Blocks = append(m.Blocks, message.ThoughtBlock{Text: thought.String()})
	}
	if text.Len() > 0 {
		m.Blocks = append(m.Blocks, message.TextBlock{Text: text.String()})
	}
	return m
}

// ---------- 请求构造（规范形 → 厂商格式） ----------

func (a *Adapter) buildRequest(req provider.Request) ([]byte, error) {
	model := a.cfg.Model
	if req.Options.Model != "" {
		model = req.Options.Model
	}
	if model == "" {
		return nil, errors.New("openai: model not set (Config.Model or Options.Model)")
	}

	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		w, err := toWire(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, w)
	}

	payload := wireRequest{
		Model:    model,
		Messages: msgs,
		Stream:   true,
		StreamOptions: &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true},
		Temperature: req.Options.Temperature,
		MaxTokens:   req.Options.MaxTokens,
		Stop:        req.Options.Stop,
	}
	for _, t := range req.Tools {
		payload.Tools = append(payload.Tools, wireTool{
			Type: "function",
			Function: wireFn{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return json.Marshal(payload)
}

// toWire 转换单条消息；ThoughtBlock 剥除——reasoning 内容不回传
// （DeepSeek/GLM 语义：reasoning_content 只产出、不回放）。
func toWire(m message.Message) (wireMessage, error) {
	switch m.Role {
	case message.RoleAssistant:
		w := wireMessage{Role: "assistant", Content: m.Text()}
		for _, tc := range m.ToolCalls {
			args := string(tc.Args)
			if args == "" {
				args = "{}"
			}
			w.ToolCalls = append(w.ToolCalls, wireToolCall{
				ID: tc.ID, Type: "function",
				Function: wireFnBody{Name: tc.Name, Arguments: args},
			})
		}
		return w, nil
	case message.RoleTool:
		return wireMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Text()}, nil
	case message.RoleSystem, message.RoleUser:
		var parts []wirePart
		textOnly := true
		for _, blk := range m.Blocks {
			switch b := blk.(type) {
			case message.TextBlock:
				parts = append(parts, wirePart{Type: "text", Text: b.Text})
			case message.ImageBlock:
				textOnly = false
				url := b.URL
				if url == "" && b.Data != nil {
					url = "data:" + b.MIME + ";base64," + base64.StdEncoding.EncodeToString(b.Data)
				}
				parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImgURL{URL: url}})
			case message.ThoughtBlock:
				// 剥除
			default:
				return wireMessage{}, fmt.Errorf("openai: unsupported block %T in %s message", blk, m.Role)
			}
		}
		if len(parts) == 0 {
			parts = append(parts, wirePart{Type: "text"})
		}
		w := wireMessage{Role: string(m.Role), Content: parts}
		if textOnly && len(parts) == 1 {
			w.Content = parts[0].Text // 纯文本退化为 string，兼容最严格的服务端
		}
		return w, nil
	default:
		return wireMessage{}, fmt.Errorf("openai: unknown role %q", m.Role)
	}
}

// ---------- wire 类型 ----------

type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Tools         []wireTool    `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wirePart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *wireImgURL `json:"image_url,omitempty"`
}

type wireImgURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function wireFnBody `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function wireFn `json:"function"`
}

type wireFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireFnBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsage) canonical() message.Usage {
	return message.Usage{
		In:     u.PromptTokens,
		Out:    u.CompletionTokens,
		Cached: u.PromptTokensDetails.CachedTokens,
		// Cost 需 model registry 计价（05 A4），M1 置 0
	}
}
