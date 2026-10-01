package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// ---------- 假 DeepSeek SSE 服务（确定性，无 key 无外网） ----------

// sseServer 按请求次序回放预设的 SSE 响应，并记录**每一次**收到的请求体
// （连续 toolcall 的回放形态断言要看第二轮请求）。
type sseServer struct {
	responses []string // 每个元素是一次完整的 SSE 响应（含 data: 前缀与 [DONE]）
	status    int      // 非 0 时按序返回该错误码
	calls     atomic.Int32
	mu        sync.Mutex
	bodies    [][]byte
}

func (s *sseServer) handler(w http.ResponseWriter, r *http.Request) {
	n := int(s.calls.Add(1)) - 1
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(s.responses[n]))
}

func (s *sseServer) body(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

func sse(chunks ...string) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString("data: " + c + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func textChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, s)
}

func reasoningChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"reasoning_content":%q},"finish_reason":null}]}`, s)
}

func testCtx() context.Context {
	return ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "u", SessionID: "s"})
}

func drainEvents(ch <-chan loop.Event, cancel func()) []loop.Event {
	cancel()
	var out []loop.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// ---------- 端到端：思考流 + 连续 toolcall ----------

// runThinkingToolLoop 跑完整飞轮：第一轮先思考（reasoning_content 流）再出
// tool_call——续片**不带 index**（DeepSeek 官方分片形态）；usage 挂在末块
// 而非独立块。第二轮喂回工具结果后**重新思考**再给最终回答。
func runThinkingToolLoop(t *testing.T) (loop.RunResult, []message.Message, *sseServer) {
	t.Helper()
	weather := tool.NewFake("get_weather", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: `{"temp":22}`}},
	}, 0)

	srv := &sseServer{responses: []string{
		sse(
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			reasoningChunk("用户问天气。"),
			reasoningChunk("需要调用工具。"),
			textChunk("让我查一下。"),
			// 首片：index + id + name + 参数前半
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北"}}]}}]}`,
			// 续片：只有 arguments 增量，index 缺失（官方文档形态）
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"京\"}"}}]}}]}`,
			// 末块：finish_reason 与 usage 同块
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":50}}}`,
		),
		sse(
			reasoningChunk("工具返回 22 度，组织回答。"), // 连续 toolcall：下一轮重新思考
			textChunk("北京今天 22 度。"),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`,
		),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	t.Cleanup(ts.Close)

	adapter := New(Config{BaseURL: ts.URL, APIKey: "test-key"})
	l, err := loop.New(loop.Config{Provider: adapter, Tools: []tool.Tool{weather}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("查北京天气")})
	if err != nil && res.EndReason == "" {
		t.Fatalf("run: %v", err)
	}
	events := drainEvents(ch, cancel)

	var history []message.Message
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case loop.AgentStartData:
			history = append(history, d.InitialInput...)
		case loop.MessageEndData:
			history = append(history, d.Message)
		case loop.ToolExecEndData:
			history = append(history, d.Result.ToMessage())
		}
	}
	return res, history, srv
}

func TestDeepseekThinkingToolLoopEndToEnd(t *testing.T) {
	res, history, _ := runThinkingToolLoop(t)
	if res.EndReason != loop.EndDone {
		t.Fatalf("end: %s", res.EndReason)
	}
	if res.Turns != 2 {
		t.Fatalf("turns: want 2 got %d", res.Turns)
	}
	if len(history) != 4 {
		t.Fatalf("history: %+v", history)
	}
	// 第一轮 assistant：思考块 + 工具调用（reasoning 进 ThoughtBlock）
	assistant := history[1]
	if got := assistant.Thought(); got != "用户问天气。需要调用工具。" {
		t.Fatalf("round-1 thought: %q", got)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("round-1 tool calls: %+v", assistant.ToolCalls)
	}
	if got := string(assistant.ToolCalls[0].Args); got != `{"city":"北京"}` {
		t.Fatalf("args (index 缺失的续片必须拼上): %s", got)
	}
	if got := history[3].Text(); got != "北京今天 22 度。" {
		t.Fatalf("final text: %q", got)
	}
	if got := history[3].Thought(); got != "工具返回 22 度，组织回答。" {
		t.Fatalf("round-2 thought: %q", got)
	}
	if res.Usage.In != 130 || res.Usage.Out != 45 || res.Usage.Cached != 50 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// 连续 toolcall 的回放形态：第二轮请求里 assistant 带 tool_calls 与上一轮的
// reasoning_content（请求带 tools 时官方要求完整回传，缺失返回 400），
// 工具结果以 role=tool 应答；user 消息不带 reasoning_content。
func TestDeepseekReplayCarriesReasoning(t *testing.T) {
	_, _, srv := runThinkingToolLoop(t)
	raw := srv.body(1)
	var req struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages: %+v", req.Messages)
	}
	a := req.Messages[1]
	if a.Role != "assistant" || len(a.ToolCalls) != 1 ||
		a.ToolCalls[0].ID != "call_x" || a.ToolCalls[0].Function.Name != "get_weather" ||
		a.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("assistant 回放: %+v", a)
	}
	if a.ReasoningContent != "用户问天气。需要调用工具。" {
		t.Fatalf("assistant 回放须带上一轮 reasoning_content，得到 %q", a.ReasoningContent)
	}
	if u := req.Messages[0]; u.Role != "user" || u.ReasoningContent != "" {
		t.Fatalf("user 消息不应带 reasoning_content: %+v", u)
	}
	tr := req.Messages[2]
	if tr.Role != "tool" || tr.ToolCallID != "call_x" {
		t.Fatalf("tool 结果应答: %+v", tr)
	}
}

// 不带 tools 的请求不发送 reasoning_content（服务端忽略该字段，例如压缩摘要请求）。
func TestDeepseekOmitsReasoningWithoutTools(t *testing.T) {
	assistant := message.Message{Role: message.RoleAssistant, Blocks: []message.Block{
		message.ThoughtBlock{Text: "思考过程"}, message.TextBlock{Text: "回答"}}}
	body, _, err := streamOnce(t, Config{APIKey: "k"}, provider.Request{
		Messages: []message.Message{message.NewUser("问"), assistant, message.NewUser("再问")}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "reasoning_content") {
		t.Fatalf("不带 tools 的请求不应包含 reasoning_content: %s", body)
	}
}

// ---------- 请求体形状 ----------

// streamOnce 发一次请求并收干事件，返回记录的请求体（请求形状断言用）。
func streamOnce(t *testing.T, cfg Config, req provider.Request) ([]byte, []provider.StreamEvent, error) {
	t.Helper()
	srv := &sseServer{responses: []string{
		sse(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	t.Cleanup(ts.Close)
	if cfg.BaseURL == "" {
		cfg.BaseURL = ts.URL
	}
	adapter := New(cfg)
	ch, err := adapter.Stream(testCtx(), req)
	if err != nil {
		return nil, nil, err
	}
	var evs []provider.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	for _, ev := range evs {
		if e, ok := ev.(provider.ErrorEvent); ok {
			return nil, evs, e.Err
		}
	}
	return srv.body(0), evs, nil
}

func TestDeepseekRequestShape(t *testing.T) {
	temp := 0.5
	raw, _, err := streamOnce(t, Config{APIKey: "k"}, provider.Request{
		Messages: []message.Message{message.NewUser("你好")},
		Options: ctxx.Options{
			Temperature:     &temp,
			ReasoningEffort: "high",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != DefaultModel {
		t.Fatalf("model 默认应为 %s: %v", DefaultModel, m["model"])
	}
	th, _ := m["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("thinking: %v", m["thinking"])
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort: %v", m["reasoning_effort"])
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatalf("stream_options: %v", m["stream_options"])
	}
	if m["user_id"] != "s" { // Scope.SessionID → KVCache 隔离锚点
		t.Fatalf("user_id: %v", m["user_id"])
	}
	// 思考模式被服务端忽略/不支持/已废弃的参数一律不下发
	for _, k := range []string{"temperature", "frequency_penalty", "presence_penalty", "tool_choice", "top_p"} {
		if _, ok := m[k]; ok {
			t.Fatalf("思考模式下不应下发 %s: %v", k, m[k])
		}
	}
}

// 关思考（none）后 temperature 才落地；user_id 做字符集清洗。
func TestDeepseekRequestThinkingOff(t *testing.T) {
	temp := 0.5
	raw, _, err := streamOnce(t, Config{APIKey: "k"}, provider.Request{
		Messages: []message.Message{message.NewUser("你好")},
		Options:  ctxx.Options{Temperature: &temp, ReasoningEffort: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	th, _ := m["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" || m["reasoning_effort"] != "none" {
		t.Fatalf("thinking/effort: %v %v", m["thinking"], m["reasoning_effort"])
	}
	if m["temperature"] != 0.5 {
		t.Fatalf("temperature: %v", m["temperature"])
	}
}

func TestDeepseekUserIDSanitized(t *testing.T) {
	raw, _, err := streamOnce(t, Config{APIKey: "k", Model: "m"}, provider.Request{
		Messages: []message.Message{message.NewUser("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := userID(ctxx.WithScope(context.Background(),
		ctxx.Scope{UserID: "u", SessionID: "会话 #1"})); got != "----1" {
		t.Fatalf("user_id 清洗: %q", got)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["thinking"]; ok {
		t.Fatal("未设思考等级时不应下发 thinking")
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("未设思考等级时不应下发 reasoning_effort")
	}
}

func TestDeepseekLevelResolution(t *testing.T) {
	cases := []struct{ override, fallback, want string }{
		{"", "", ""},
		{"none", "", "none"},
		{"low", "", "low"},
		{"HIGH", "", "high"},
		{"max", "", "max"},
		{"minimal", "", "low"},   // 官方别名
		{"medium", "", "high"},   // 官方别名
		{"xhigh", "", "high"},    // 官方别名
		{"", "medium", "high"},   // 静态兜底
		{"high", "none", "high"}, // 每请求覆盖兜底
	}
	for _, c := range cases {
		got, err := resolveLevel(c.override, c.fallback)
		if err != nil || got != c.want {
			t.Fatalf("resolveLevel(%q,%q) = %q,%v want %q", c.override, c.fallback, got, err, c.want)
		}
	}
	if _, err := resolveLevel("turbo", ""); err == nil || !strings.Contains(err.Error(), "未知思考等级") {
		t.Fatalf("非法等级应报错, got %v", err)
	}
	// 非法等级在发请求前响亮失败，而不是发出去吃服务端 400
	_, _, err := streamOnce(t, Config{APIKey: "k"}, provider.Request{
		Messages: []message.Message{message.NewUser("hi")},
		Options:  ctxx.Options{ReasoningEffort: "turbo"},
	})
	if err == nil || !strings.Contains(err.Error(), "未知思考等级") {
		t.Fatalf("请求前应拦下非法等级: %v", err)
	}
}

// ---------- 并行 tool calls 与分片协议 ----------

// 两个并行调用（index 0/1 交错续片，带 index）都要按发起顺序完整重组。
func TestDeepseekParallelToolCalls(t *testing.T) {
	srv := &sseServer{responses: []string{
		sse(
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"now","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"calc","arguments":"{\"b\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
			// 旧代 usage 字段（prompt_cache_hit_tokens）也要认
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_cache_hit_tokens":7}}`,
		),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		c, ok := ev.(provider.MessageComplete)
		if !ok {
			continue
		}
		if len(c.Message.ToolCalls) != 2 {
			t.Fatalf("tool calls: %+v", c.Message.ToolCalls)
		}
		first, second := c.Message.ToolCalls[0], c.Message.ToolCalls[1]
		if first.Name != "now" || string(first.Args) != `{"a":1}` {
			t.Fatalf("first: %+v", first)
		}
		if second.Name != "calc" || string(second.Args) != `{"b":2}` {
			t.Fatalf("second: %+v", second)
		}
		if c.Usage.Cached != 7 {
			t.Fatalf("旧代缓存字段未识别: %+v", c.Usage)
		}
		return
	}
	t.Fatal("没有收到 MessageComplete")
}

// arguments 增量指向不存在的调用：协议违规要响亮报错，不能悄悄丢增量。
func TestDeepseekOrphanArgumentsDelta(t *testing.T) {
	srv := &sseServer{responses: []string{
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	var retryable bool
	for ev := range ch {
		if e, ok := ev.(provider.ErrorEvent); ok {
			gotErr, retryable = e.Err, e.Retryable
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "没有已开始的调用") {
		t.Fatalf("err = %v", gotErr)
	}
	if !retryable {
		t.Error("服务端协议违规应标记可重试")
	}
}

// 服务端返回残缺的 tool_call arguments：适配器层拦下（A3），合法/空参数放行。
func TestDeepseekToolArgumentsGuard(t *testing.T) {
	cases := []struct {
		name    string
		chunks  []string
		wantErr bool
		want    string
	}{
		{
			name:    "非法 JSON 拦截",
			chunks:  []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"now","arguments":"{\"a\": 北京}"}}]},"finish_reason":"tool_calls"}]}`},
			wantErr: true,
		},
		{
			name:   "空参数归一为 {}",
			chunks: []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"now","arguments":""}}]},"finish_reason":"tool_calls"}]}`},
			want:   "{}",
		},
		{
			name: "无 id 的调用补合成 id",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"now","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			want: "{}",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := &sseServer{responses: []string{sse(c.chunks...)}}
			ts := httptest.NewServer(http.HandlerFunc(srv.handler))
			defer ts.Close()

			ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(context.Background(), provider.Request{})
			if err != nil {
				t.Fatal(err)
			}
			var gotErr error
			for ev := range ch {
				switch e := ev.(type) {
				case provider.ErrorEvent:
					gotErr = e.Err
				case provider.MessageComplete:
					if c.wantErr {
						t.Fatalf("残缺参数被放行: %+v", e.Message.ToolCalls)
					}
					if len(e.Message.ToolCalls) != 1 || string(e.Message.ToolCalls[0].Args) != c.want {
						t.Fatalf("args = %s", e.Message.ToolCalls[0].Args)
					}
					if e.Message.ToolCalls[0].ID == "" {
						t.Fatal("应补合成 id")
					}
					return
				}
			}
			if c.wantErr && (gotErr == nil || !strings.Contains(gotErr.Error(), "不是合法 JSON")) {
				t.Fatalf("err = %v", gotErr)
			}
		})
	}
}

// ---------- finish_reason 特例与错误分类 ----------

func TestDeepseekFinishReasons(t *testing.T) {
	cases := []struct {
		finish    string
		wantRetry bool
	}{
		{"content_filter", false},
		{"insufficient_system_resource", true},
		{"aborted", true},
	}
	for _, c := range cases {
		t.Run(c.finish, func(t *testing.T) {
			srv := &sseServer{responses: []string{
				sse(
					textChunk("部分"),
					fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, c.finish),
				),
			}}
			ts := httptest.NewServer(http.HandlerFunc(srv.handler))
			defer ts.Close()

			ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(context.Background(), provider.Request{})
			if err != nil {
				t.Fatal(err)
			}
			var gotErr error
			var retry, completed bool
			for ev := range ch {
				switch e := ev.(type) {
				case provider.ErrorEvent:
					gotErr, retry = e.Err, e.Retryable
				case provider.MessageComplete:
					completed = true
				}
			}
			if gotErr == nil || !strings.Contains(gotErr.Error(), c.finish) {
				t.Fatalf("err = %v（应含 %s）", gotErr, c.finish)
			}
			if retry != c.wantRetry {
				t.Fatalf("retryable = %v, want %v", retry, c.wantRetry)
			}
			if completed {
				t.Fatal("报错后不应再产出 MessageComplete")
			}
		})
	}
}

// 5xx → RetryableError → 飞轮退避重试后成功（02 §7）。
func TestDeepseekRetryOn5xx(t *testing.T) {
	var failedOnce atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failedOnce.CompareAndSwap(false, true) {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"恢复"},"finish_reason":"stop"}]}`)))
	}))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	l, _ := loop.New(loop.Config{Provider: adapter})
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	if err != nil || res.EndReason != loop.EndDone {
		t.Fatalf("want done, got %s err=%v", res.EndReason, err)
	}
}

// 401 → 不可重试 → EndError。
func TestDeepseekAuthErrorNotRetried(t *testing.T) {
	srv := &sseServer{}
	srv.status = 401
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "bad", Model: "m"})
	l, _ := loop.New(loop.Config{Provider: adapter})
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("hi")})
	if res.EndReason != loop.EndError || err == nil {
		t.Fatalf("want EndError, got %s err=%v", res.EndReason, err)
	}
}

// ctx 取消 → 部分结果义务：Interrupted=true 的已收文本（02 §5）。
func TestDeepseekInterruptPartial(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			_, _ = w.Write([]byte("data: " + textChunk(fmt.Sprintf("片段%d", i)) + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(testCtx())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(ctx, provider.Request{
		Messages: []message.Message{message.NewUser("讲长的")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got *message.Message
	for ev := range ch {
		if c, ok := ev.(provider.MessageComplete); ok {
			m := c.Message
			got = &m
		}
	}
	if got == nil || !got.Interrupted || got.Text() == "" {
		t.Fatalf("interrupted 部分结果: %+v", got)
	}
}

func TestDeepseekUnexpectedEOF(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + textChunk("partial") + "\n\n"))
	}))
	defer ts.Close()

	ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(testCtx(), provider.Request{
		Messages: []message.Message{message.NewUser("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for ev := range ch {
		if e, ok := ev.(provider.ErrorEvent); ok {
			sawError = strings.Contains(e.Err.Error(), "unexpected EOF")
		}
	}
	if !sawError {
		t.Fatal("truncated stream must end with unexpected EOF ErrorEvent")
	}
}

// ---------- 真·集成测试（有 key 才跑，自动 skip；02 §10 验收） ----------

// 设置 DEEPSEEK_API_KEY（必填）、DEEPSEEK_MODEL（可选，默认 deepseek-flash）
// 后运行：  DEEPSEEK_API_KEY=sk-xx go test -run TestLive -v
// 验证点：思考流（reasoning_content）+ 工具调用 + 连续轮次回传 reasoning_content 后服务端不返回 400。
func TestLiveDeepseek(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live test")
	}
	cfg := Config{APIKey: key}
	if v := os.Getenv("DEEPSEEK_MODEL"); v != "" {
		cfg.Model = v
	}

	weather := tool.NewFake("get_weather", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: `{"city":"北京","temp_c":22,"condition":"晴"}`}},
	}, 0)

	adapter := New(cfg)
	l, err := loop.New(loop.Config{Provider: adapter, Tools: []tool.Tool{weather}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)

	ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "test", SessionID: "live-deepseek"})
	ctx = ctxx.WithCredentials(ctx, ctxx.Credentials{"deepseek": key})
	ctx = ctxx.WithOptions(ctx, ctxx.Options{ReasoningEffort: "low"}) // 活检用 low 省时省钱

	res, err := l.Run(ctx, []message.Message{
		message.NewUser("北京今天天气怎么样？必须用 get_weather 工具查一下，然后用一句话告诉我。"),
	})
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	t.Logf("end=%s turns=%d usage=%+v", res.EndReason, res.Turns, res.Usage)

	if n, _, _ := weather.Calls(); n < 1 {
		t.Fatal("model did not call the tool")
	}
	var thoughtLen int
	for _, ev := range drainEvents(ch, cancel) {
		if d, ok := ev.Data.(loop.MessageEndData); ok {
			thoughtLen += len(d.Message.Thought())
			if ev.Turn == res.Turns-1 {
				t.Logf("final: %s", d.Message.Text())
			}
		}
	}
	t.Logf("thought chars: %d", thoughtLen)
}

// 限额：在售模型按内置表返回（窗口 1M、缺省输出 64K），model 为空指 Config.Model；
// Config 的 ContextWindow / MaxOutput 覆盖内置表；表中没有的模型名返回零值。
func TestDeepseekLimits(t *testing.T) {
	a := New(Config{})
	want := provider.Limits{ContextWindow: 1_000_000, MaxOutput: 64 * 1024}
	if got := a.Limits(""); got != want {
		t.Fatalf("默认模型限额：got %+v want %+v", got, want)
	}
	if got := a.Limits("deepseek-v4-pro"); got != want {
		t.Fatalf("pro 限额：got %+v want %+v", got, want)
	}
	if got := a.Limits("unknown-model"); got != (provider.Limits{}) {
		t.Fatalf("未知模型应为零值：got %+v", got)
	}
	o := New(Config{Model: "unknown-model", ContextWindow: 200_000, MaxOutput: 8192})
	if got := o.Limits(""); got != (provider.Limits{ContextWindow: 200_000, MaxOutput: 8192}) {
		t.Fatalf("配置覆盖：got %+v", got)
	}
}

// token 估算按官方换算比例：中文 0.6、英文字符 0.3、每条消息另加 4。
func TestDeepseekCountTokens(t *testing.T) {
	a := New(Config{})
	msgs := []message.Message{message.NewUser("今天天气abcd")} // 4 × 0.6 + 4 × 0.3 = 3.6
	if got := a.CountTokens("", msgs); got != 8 {
		t.Fatalf("got %d want 8", got)
	}
}
