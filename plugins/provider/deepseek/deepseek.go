// Package deepseek 实现 DeepSeek 官方 API 的流式 Provider 适配器
// （https://api-docs.deepseek.com，OpenAI 兼容但有一族自家语义）。
//
// 选型（2026-09 官方文档）：默认模型 deepseek-flash（DeepSeek-V4.1-Flash）。
// 两个在售模型 deepseek-flash / deepseek-v4-pro 都是 1M 上下文、384K 输出上限、
// 默认思考；flash 便宜一个量级、并发 2500（pro 500）、且支持视觉输入——
// Gowild 场景摄像头帧注入只有 flash 接得住，pro 无视觉。故 DefaultModel=flash。
//
// 为什么不塞进 plugins/provider/openai：DeepSeek 的差异不在「端点不同」，
// 而在协议细节——openai 适配器当同构端点用也能跑，但下面这些只有官方
// 文档写明的行为值得一个专门适配器兜住：
//
//   - 思考等级：reasoning_effort（none/low/high/max）+ thinking 开关双参数
//     同发；思考模式下 temperature 被服务端忽略、tool_choice 不支持
//     required/指定函数（400）——这三种参数一律不下发；
//   - frequency_penalty / presence_penalty 已废弃（no-op）——不下发；
//   - 流式 tool_calls 分片：首个分片带 id/name，后续分片只带 arguments
//     增量且 index 可缺——靠「有 id/name = 开新调用」区分续片与新调用；
//   - 流式 usage 挂在末块（finish_reason 非 null 那块），不单独成块；
//   - finish_reason 自家值：content_filter（内容被过滤）/
//     insufficient_system_resource（资源不足）/ aborted（服务端中止）；
//   - 连续 toolcall：思考模式每轮响应都先出 reasoning_content 再出
//     content/tool_calls，喂回工具结果后下一轮重新思考——reasoning 只产出
//     不回传（toWire 剥除 ThoughtBlock），多轮由 core 飞轮驱动。
//
// 关键义务（02 §5，与 openai 适配器同）：ctx 取消时必须以
// MessageComplete{Interrupted:true} 收尾返回已收内容；残缺 tool calls 丢弃。
package deepseek

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

// DefaultModel 是稳定默认：DeepSeek-V4.1-Flash（工具调用 + 视觉 + 思考，
// 见包注释的选型依据）。
const DefaultModel = "deepseek-flash"

// DefaultBaseURL 是官方端点。/v1 前缀（OpenAI SDK 兼容别名）同样可用，
// 两者都指向 /chat/completions。
const DefaultBaseURL = "https://api.deepseek.com"

type Config struct {
	BaseURL  string // 默认 https://api.deepseek.com（.../v1 亦可）
	APIKey   string // 兜底；优先读 ctx Credentials（01 §1.1）
	CredName string // ctxx.CredentialFrom 的键，默认 "deepseek"
	Model    string // 默认 DefaultModel；Request.Options.Model 可覆盖
	// ThinkingLevel 是思考等级静态兜底（none/low/high/max 及官方别名）；
	// 空 = 不下发 thinking/reasoning_effort，用服务端默认（当前 = 思考开、
	// effort=high）。每请求 Options.ReasoningEffort 优先于本字段。
	// 注意它作用于**走本适配器的所有调用**（含压缩摘要）——摘要想省思考，
	// 在 [llm] 把 reasoning_effort 设为 none 或换 compress.model。
	ThinkingLevel string
	HTTP          *http.Client
}

type Adapter struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Adapter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.CredName == "" {
		cfg.CredName = "deepseek"
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Adapter{cfg: cfg, hc: hc}
}

// Stream 发起流式 chat/completions 并把 SSE 分片翻译为 provider 事件。
func (a *Adapter) Stream(ctx context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	body, err := a.buildRequest(req, userID(ctx))
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
		e := fmt.Errorf("deepseek: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
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
	last := -1 // 最近一次触及的 tool call 下标（后续分片可不带 index，见下）
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
			ch <- provider.ErrorEvent{Err: fmt.Errorf("deepseek: stream read: %w", err), Retryable: true}
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
				// 官方语义：首个分片带 id/name（index 可缺），后续分片只带
				// arguments 增量（index 可缺）。有 id 或 name = 开新调用；
				// 只有 arguments = 续既有调用（index 缺失时续「最近一个」）。
				fresh := tc.ID != "" || tc.Function.Name != ""
				idx := last
				if tc.Index != nil {
					idx = *tc.Index
				} else if fresh {
					idx = last + 1 // 不带 index 的新调用顺延占下一个槽
				}
				p := calls[idx]
				if p == nil {
					if !fresh {
						// arguments 增量指向不存在的调用：服务端违反分片
						// 协议，响亮报错而不是把增量悄悄丢掉。
						ch <- provider.ErrorEvent{
							Err:       fmt.Errorf("deepseek: 流式 tool_calls 的 arguments 增量没有已开始的调用（index=%d）", idx),
							Retryable: true,
						}
						return
					}
					p = &pendingCall{}
					calls[idx] = p
					order = append(order, idx)
				}
				last = idx
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name += tc.Function.Name // 个别实现拆片发 name
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
		ch <- provider.ErrorEvent{Err: errors.New("deepseek: unexpected EOF before [DONE] or finish_reason"), Retryable: true}
		return
	}
	// DeepSeek 自家 finish_reason：stop/length/tool_calls 走正常收尾，
	// 其余三家语义各异，按可否重试分流（错误里带上已收内容梗概便于排查）。
	switch finish {
	case "content_filter":
		ch <- provider.ErrorEvent{
			Err: fmt.Errorf("deepseek: 内容被安全过滤（finish_reason=content_filter），已收文本：%s",
				snippet(text.String(), 80)),
			Retryable: false, // 同样的输入重试大概率再触发，别烧轮次
		}
		return
	case "insufficient_system_resource":
		ch <- provider.ErrorEvent{
			Err:       errors.New("deepseek: 服务端资源不足（finish_reason=insufficient_system_resource）"),
			Retryable: true,
		}
		return
	case "aborted":
		ch <- provider.ErrorEvent{
			Err:       errors.New("deepseek: 生成被服务端中止（finish_reason=aborted）"),
			Retryable: true,
		}
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
			// 参数必须是合法 JSON——与 openai 适配器同一条纪律（A3）：不合法
			// 绝不当 ToolCall 往下游传，按可重试错误上报（未产出内容时 core
			// 退避重试通常能自愈，重试耗尽以清晰错误收场）。
			if !json.Valid([]byte(args)) {
				ch <- provider.ErrorEvent{
					Err: fmt.Errorf("deepseek: tool_call %q 的 arguments 不是合法 JSON（服务端违反协议）: %s",
						p.name, snippet(args, 120)),
					Retryable: true,
				}
				return
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

// snippet 截断字符串用于错误信息（rune 安全），避免把大段垃圾塞进错误里。
func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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

// ---------- 请求构造（规范形 → DeepSeek 格式） ----------

// resolveLevel 归一思考等级：官方取值 none/low/high/max，官方别名
// minimal→low、medium/xhigh→high。返回 ("", nil) = 不下发（服务端默认）。
func resolveLevel(override, fallback string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(override))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(fallback))
	}
	switch raw {
	case "":
		return "", nil
	case "none":
		return "none", nil
	case "minimal", "low":
		return "low", nil
	case "medium", "xhigh", "high":
		return "high", nil
	case "max":
		return "max", nil
	default:
		return "", fmt.Errorf("deepseek: 未知思考等级 %q（可选 none/low/high/max）", raw)
	}
}

func (a *Adapter) buildRequest(req provider.Request, uid string) ([]byte, error) {
	model := a.cfg.Model
	if req.Options.Model != "" {
		model = req.Options.Model
	}

	level, err := resolveLevel(req.Options.ReasoningEffort, a.cfg.ThinkingLevel)
	if err != nil {
		return nil, err
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
		MaxTokens: req.Options.MaxTokens,
		Stop:      req.Options.Stop,
		UserID:    uid,
	}
	// 思考等级双参数同发（thinking 开关 + effort 强度），避免依赖服务端
	// 对单一参数的默认解释；none 是唯一的「关」。
	if level == "none" {
		payload.ReasoningEffort = "none"
		payload.Thinking = &wireThinking{Type: "disabled"}
	} else if level != "" {
		payload.ReasoningEffort = level
		payload.Thinking = &wireThinking{Type: "enabled"}
	}
	// 思考模式下 temperature 被服务端忽略（官方文档），设了也不发——发一个
	// 必然无效的旋钮只会误导排查。level 为空同样不发：服务端默认思考开，
	// temperature 一样无效。只有显式关思考（none）才让这个旋钮落地。
	if req.Options.Temperature != nil && level == "none" {
		payload.Temperature = req.Options.Temperature
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

// userID 把 Scope.SessionID 折算成 DeepSeek user_id（官方字符集
// [a-zA-Z0-9-_]，≤512）：内容安全/KVCache 隔离的锚点——同会话同 id，
// 上下文缓存命中（价格 1/50）才稳定。scope 缺失返回空（不下发）。
func userID(ctx context.Context) string {
	s, ok := ctxx.ScopeFrom(ctx)
	if !ok || s.SessionID == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s.SessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	u := b.String()
	if len(u) > 512 { // 构造保证纯 ASCII，按字节截即可
		u = u[:512]
	}
	return u
}

// imageURL 计算 ImageBlock 的 wire URL：URL 直用，否则 Data 转 data: URI。
// 两者都没有是**畸形块**：返回错误而不是发一个空 url 出去——否则服务端只回
// 一个含糊的 400，排查不到是我们自己造了坏请求。
func imageURL(b message.ImageBlock) (string, error) {
	if b.URL != "" {
		return b.URL, nil
	}
	if len(b.Data) > 0 {
		return "data:" + b.MIME + ";base64," + base64.StdEncoding.EncodeToString(b.Data), nil
	}
	return "", errors.New("deepseek: ImageBlock 既无 URL 也无 Data")
}

// toWire 转换单条消息。ThoughtBlock 剥除——DeepSeek 语义：reasoning_content
// 只产出、不回放（回传会被拒收或无视；官方对「思维链作为输入」只开了 Beta
// 前缀续写，常规多轮一律剥除）。连续 toolcall 的回放形态：assistant 带
// tool_calls（不带 reasoning），工具结果以 role=tool + tool_call_id 应答。
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
			case *message.TextBlock:
				// 指针形态与值形态同为合法 Block（CloneBlock 契约含指针），nil 跳过
				if b != nil {
					parts = append(parts, wirePart{Type: "text", Text: b.Text})
				}
			case message.ImageBlock:
				url, err := imageURL(b)
				if err != nil {
					return wireMessage{}, err
				}
				textOnly = false
				parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImgURL{URL: url}})
			case *message.ImageBlock:
				if b != nil {
					url, err := imageURL(*b)
					if err != nil {
						return wireMessage{}, err
					}
					textOnly = false
					parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImgURL{URL: url}})
				}
			case message.ThoughtBlock, *message.ThoughtBlock:
				// 剥除
			default:
				return wireMessage{}, fmt.Errorf("deepseek: unsupported block %T in %s message", blk, m.Role)
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
		return wireMessage{}, fmt.Errorf("deepseek: unknown role %q", m.Role)
	}
}

// ---------- wire 类型 ----------

// wireRequest 是 DeepSeek chat/completions 请求体。刻意**没有**这些字段：
//
//	frequency_penalty / presence_penalty —— 已废弃（官方：无任何效果）
//	top_p        —— 仅思考模式生效且有效域 0.95–1.0，语义怪异，不碰
//	tool_choice  —— 思考模式下 required/指定函数直接 400，auto 是默认，
//	               下发没有任何收益只有踩雷面
//	response_format / logprobs —— 本适配器的用法用不上
type wireRequest struct {
	Model           string        `json:"model"`
	Messages        []wireMessage `json:"messages"`
	Thinking        *wireThinking `json:"thinking,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Tools           []wireTool    `json:"tools,omitempty"`
	Stream          bool          `json:"stream"`
	StreamOptions   *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens 不设（0）时用服务端默认：非思考 8K / 思考 64K
	// （effort=max 时 128K）。思考模式下别手动限额——推理 token 一起计，
	// 限额太低会出现「正文一字未出先截断」。
	MaxTokens int      `json:"max_tokens,omitempty"`
	Stop      []string `json:"stop,omitempty"`
	UserID    string   `json:"user_id,omitempty"`
}

type wireThinking struct {
	Type string `json:"type"` // enabled | disabled
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// 刻意没有 reasoning_content：思维链不回传（见 toWire）。
}

type wirePart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *wireImgURL `json:"image_url,omitempty"`
}

type wireImgURL struct {
	URL string `json:"url"`
	// Detail 不下发：low/high/original 是省钱/保真旋钮，默认 auto 由
	// 服务端按分辨率自决，符合「宿主不替业务做图像策略」的分层。
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
			// Index 是指针：官方文档说 tool_calls 后续分片「只带 arguments
			// 增量」——index 可缺。值形态分不清「下标 0」与「没带」。
			ToolCalls []wireToolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	// Usage 挂在末块（finish_reason 非 null 那块），不单独成块——OpenAI
	// 风格的「纯 usage 块」不发，但解析对两种形态都兼容。
	Usage *wireUsage `json:"usage"`
}

type wireToolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireUsage 同时解析两代缓存字段：官方 DeepSeek 形态 prompt_cache_hit_tokens
// 与 OpenAI 兼容形态 prompt_tokens_details.cached_tokens（V4 文册用的是后者）。
type wireUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsage) canonical() message.Usage {
	cached := u.PromptCacheHitTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return message.Usage{
		In:     u.PromptTokens,
		Out:    u.CompletionTokens,
		Cached: cached,
		// Cost 需 model registry 计价（05 A4），M1 置 0
	}
}
