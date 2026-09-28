package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// ---------- 假 OpenAI SSE 服务（确定性，无 key 无外网） ----------

// sseServer 按请求次序回放预设的 SSE 响应，并记录收到的请求体。
type sseServer struct {
	responses []string // 每个元素是一次完整的 SSE 响应（含 data: 前缀与 [DONE]）
	status    int      // 非 0 时按序返回该错误码
	calls     atomic.Int32
	lastBody  atomic.Value // []byte
}

func (s *sseServer) handler(w http.ResponseWriter, r *http.Request) {
	n := int(s.calls.Add(1)) - 1
	body, _ := io2bytes(r.Body)
	s.lastBody.Store(body)
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(s.responses[n]))
}

func io2bytes(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return []byte(sb.String()), err
		}
	}
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

// 跑完整 loop 的辅助：服务端第一轮回 tool call（参数分两片测累加），第二轮回全文。
func runToolLoop(t *testing.T) (loop.RunResult, []message.Message) {
	t.Helper()
	weather := tool.NewFake("get_weather", message.ToolResult{
		Blocks: []message.Block{message.TextBlock{Text: `{"temp":22}`}},
	}, 0)

	srv := &sseServer{responses: []string{
		sse(
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			textChunk("让我查一下。"),
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"京\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`,
		),
		sse(
			`{"choices":[{"index":0,"delta":{"content":"北京"}}]}`,
			textChunk("今天 22 度。"),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":4}}`,
		),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "test-key", Model: "test-model"})
	l, err := loop.New(loop.Config{Provider: adapter, Tools: []tool.Tool{weather}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := l.Subscribe(1024)
	ctx := ctxx.WithCredentials(testCtx(), ctxx.Credentials{"openai": "ctx-key"})
	res, err := l.Run(ctx, []message.Message{message.NewUser("查北京天气")})
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
	return res, history
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

// ---------- 确定性测试 ----------

// 完整链路：SSE 分片 → 适配器 → 飞轮多轮工具循环 → 事件重建。
func TestAdapterToolLoopEndToEnd(t *testing.T) {
	res, history := runToolLoop(t)
	if res.EndReason != loop.EndDone {
		t.Fatalf("end: %s", res.EndReason)
	}
	if res.Turns != 2 {
		t.Fatalf("turns: want 2 got %d", res.Turns)
	}
	if len(history) != 4 {
		t.Fatalf("history: %+v", history)
	}
	// 第二轮 assistant：工具结果喂回后的最终回答
	if got := history[3].Text(); got != "北京今天 22 度。" {
		t.Fatalf("final text: %q", got)
	}
	if res.Usage.In != 30 || res.Usage.Out != 9 || res.Usage.Cached != 3 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// 请求体校验：tools、stream_options、tool 结果重放形态。
func TestAdapterRequestShape(t *testing.T) {
	srv := &sseServer{responses: []string{
		sse(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	weather := tool.NewFake("get_weather", message.ToolResult{}, 0)
	adapter := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	l, _ := loop.New(loop.Config{Provider: adapter, Tools: []tool.Tool{weather}})
	_, err := l.Run(testCtx(), []message.Message{message.NewUser("你好")})
	if err != nil {
		t.Fatal(err)
	}

	var req wireRequest
	if err := json.Unmarshal(srv.lastBody.Load().([]byte), &req); err != nil {
		t.Fatal(err)
	}
	if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("stream options: %+v", req.StreamOptions)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools: %+v", req.Tools)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "你好" {
		t.Fatalf("messages: %+v", req.Messages)
	}
}

// 指针形态 Block 与值形态同为合法输入（CloneBlock 契约），toWire 必须一并接受。
func TestAdapterPointerBlocks(t *testing.T) {
	m := message.Message{Role: message.RoleUser, Blocks: []message.Block{
		&message.TextBlock{Text: "看看这张"},
		&message.ImageBlock{URL: "https://example.com/a.png", MIME: "image/png"},
		&message.ThoughtBlock{Text: "不应回传"},
		(*message.TextBlock)(nil),
	}}
	w, err := toWire(m)
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := w.Content.([]wirePart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts: %+v", w.Content)
	}
	if parts[0].Text != "看看这张" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/a.png" {
		t.Fatalf("parts: %+v", parts)
	}
}

// 5xx → RetryableError → 飞轮退避重试后成功（02 §7）。
func TestAdapterRetryOn5xx(t *testing.T) {
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

// ctx 取消 → 部分结果义务：Interrupted=true 的已收文本（02 §5）。
func TestAdapterInterruptPartial(t *testing.T) {
	chunks := make([]string, 0, 12)
	for i := 0; i < 10; i++ {
		chunks = append(chunks, textChunk(fmt.Sprintf("片段%d", i)))
	}
	// 服务端慢速吐片：一个 chunk 一个 flush，间隔 50ms
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	l, _ := loop.New(loop.Config{Provider: adapter})
	ch, cancel := l.Subscribe(1024)
	go func() {
		time.Sleep(120 * time.Millisecond)
		l.Interrupt()
	}()
	res, err := l.Run(testCtx(), []message.Message{message.NewUser("讲长的")})
	if err != nil {
		t.Fatal(err)
	}
	if res.EndReason != loop.EndInterrupted {
		t.Fatalf("end: %s", res.EndReason)
	}
	for _, ev := range drainEvents(ch, cancel) {
		if d, ok := ev.Data.(loop.MessageEndData); ok && d.Message.Interrupted && d.Message.Text() == "" {
			t.Fatal("interrupted message must carry partial text")
		}
	}
}

func TestAdapterUnexpectedEOF(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + textChunk("partial") + "\n\n"))
	}))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	ch, err := adapter.Stream(testCtx(), provider.Request{Messages: []message.Message{message.NewUser("hi")}})
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

// 401 → 不可重试 → EndError。
func TestAdapterAuthErrorNotRetried(t *testing.T) {
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

// ---------- 真·集成测试（有 key 才跑，自动 skip；02 §10 验收） ----------

// 设置 OPENAI_API_KEY（必填）、OPENAI_BASE_URL（可选，DeepSeek/GLM/Kimi/Ollama 等）、
// OPENAI_MODEL（可选）后运行：  OPENAI_API_KEY=sk-xx go test -run TestLive -v
func TestLiveOpenAICompatible(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set; skipping live test")
	}
	cfg := Config{APIKey: key, Model: "gpt-4o-mini"}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("OPENAI_MODEL"); v != "" {
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

	ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: "test", SessionID: "live-1"})
	ctx = ctxx.WithCredentials(ctx, ctxx.Credentials{"openai": key})

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
	for _, ev := range drainEvents(ch, cancel) {
		if d, ok := ev.Data.(loop.MessageEndData); ok && ev.Turn == res.Turns-1 {
			t.Logf("final: %s", d.Message.Text())
		}
	}
}

// 服务端返回残缺的 tool_call arguments（模型输出非法 JSON 很常见）：
// 必须在适配器层拦下——不合法参数绝不作为 ToolCall 流进领域模型（否则工具、
// Guard、落盘、重放全部以「Args 是合法 JSON」为前提而被击穿）。
func TestAdapterRejectsInvalidToolArguments(t *testing.T) {
	srv := &sseServer{responses: []string{
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":"{\"city\": 北京}"}}]},"finish_reason":"tool_calls"}]}`),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	adapter := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	ch, err := adapter.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	var retryable bool
	var completed bool
	for ev := range ch {
		switch e := ev.(type) {
		case provider.ErrorEvent:
			gotErr, retryable = e.Err, e.Retryable
		case provider.MessageComplete:
			completed = true
			if len(e.Message.ToolCalls) > 0 {
				t.Fatalf("非法参数的 tool call 被放行：%+v", e.Message.ToolCalls)
			}
		}
	}
	if gotErr == nil {
		t.Fatal("残缺 arguments 未被拦截")
	}
	if !retryable {
		t.Error("应标记为可重试（未产出内容时 core 会退避重试，通常自愈）")
	}
	if !strings.Contains(gotErr.Error(), "不是合法 JSON") {
		t.Fatalf("错误信息应说明原因：%v", gotErr)
	}
	if completed {
		t.Error("报错后不应再产出 MessageComplete")
	}
}

// 合法参数的正常路径不受影响（含空参数 → {}）。
func TestAdapterAcceptsValidToolArguments(t *testing.T) {
	srv := &sseServer{responses: []string{
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"now","arguments":""}}]},"finish_reason":"tool_calls"}]}`),
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	ch, err := New(Config{BaseURL: ts.URL, APIKey: "k", Model: "m"}).Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		switch e := ev.(type) {
		case provider.ErrorEvent:
			t.Fatalf("合法/空参数不应报错：%v", e.Err)
		case provider.MessageComplete:
			if len(e.Message.ToolCalls) != 1 || string(e.Message.ToolCalls[0].Args) != "{}" {
				t.Fatalf("tool calls = %+v", e.Message.ToolCalls)
			}
			return
		}
	}
	t.Fatal("没有收到 MessageComplete")
}

// 畸形图片块（既无 URL 也无 Data）在本地就报错，而不是发个空 url 让服务端回 400。
func TestAdapterRejectsEmptyImageBlock(t *testing.T) {
	m := message.Message{Role: message.RoleUser, Blocks: []message.Block{message.ImageBlock{MIME: "image/png"}}}
	if _, err := toWire(m); err == nil || !strings.Contains(err.Error(), "ImageBlock") {
		t.Fatalf("err = %v, want 关于 ImageBlock 的显式错误", err)
	}
	// Data 形态仍正常转成 data: URI。
	ok := message.Message{Role: message.RoleUser, Blocks: []message.Block{
		message.ImageBlock{MIME: "image/png", Data: []byte{1, 2}},
	}}
	w, err := toWire(ok)
	if err != nil {
		t.Fatal(err)
	}
	parts := w.Content.([]wirePart)
	if parts[0].ImageURL == nil || !strings.HasPrefix(parts[0].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("parts = %+v", parts)
	}
}
