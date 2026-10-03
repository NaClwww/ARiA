package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// recordingService 记录 Recall 请求并返回预设结果；block 为真时阻塞至 ctx 结束。
type recordingService struct {
	reqs  []RecallRequest
	items []Item
	err   error
	block bool
}

func (r *recordingService) Stage(context.Context, StageRequest) error             { return nil }
func (r *recordingService) End(context.Context, SessionRequest) error             { return nil }
func (r *recordingService) Start(context.Context, SessionRequest) ([]Item, error) { return nil, nil }
func (r *recordingService) Recall(ctx context.Context, req RecallRequest) ([]Item, error) {
	r.reqs = append(r.reqs, req)
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.items, r.err
}

func scopedCtx() context.Context {
	return ctxx.WithScope(context.Background(), ctxx.Scope{SessionID: "s1", UserID: "u1", Namespace: "home"})
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func call(args string) message.ToolCall {
	return message.ToolCall{ID: "c1", Args: json.RawMessage(args)}
}

func resultText(t *testing.T, res message.ToolResult) string {
	t.Helper()
	if len(res.Blocks) != 1 {
		t.Fatalf("结果应只有 1 个文本块：%+v", res)
	}
	tb, ok := res.Blocks[0].(message.TextBlock)
	if !ok {
		t.Fatalf("结果应为文本块：%+v", res.Blocks[0])
	}
	return tb.Text
}

// 召回条目的渲染：时间前缀与暂存标注。
func TestRender(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local)
	got := Render([]Item{
		{Text: "喜欢猫"},
		{Text: "改成周日去大阪", At: at, Source: SourceStaged},
	})
	want := "- 喜欢猫\n- [2026-10-01 09:30] 改成周日去大阪（未合并）"
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}

// 工具定义：名称、必填参数。
func TestRecallToolDef(t *testing.T) {
	def := RecallTool(&recordingService{}, nil).Def()
	if def.Name != RecallToolName || !strings.Contains(string(def.Parameters), `"required":["query"]`) {
		t.Fatalf("工具定义不符：%+v", def)
	}
}

// 正常调用：namespace 与 session_id 取自 ctx 的 Scope，limit 缺省 10，结果经 Render 渲染。
func TestRecallToolExec(t *testing.T) {
	svc := &recordingService{items: []Item{{Text: "小明喜欢猫"}}}
	res := RecallTool(svc, quietLog()).Exec(scopedCtx(), call(`{"query":"小明 宠物"}`))
	if res.IsError || res.CallID != "c1" {
		t.Fatalf("应成功：%+v", res)
	}
	if got := resultText(t, res); got != "- 小明喜欢猫" {
		t.Fatalf("结果文本：%q", got)
	}
	want := RecallRequest{Namespace: "home", SessionID: "s1", Query: "小明 宠物", Limit: DefaultRecallToolLimit}
	if len(svc.reqs) != 1 || svc.reqs[0] != want {
		t.Fatalf("请求不符：%+v", svc.reqs)
	}
}

// limit 超过上限按上限取；没有结果时返回 recallToolEmpty。
func TestRecallToolLimitClampAndEmpty(t *testing.T) {
	svc := &recordingService{}
	res := RecallTool(svc, quietLog()).Exec(scopedCtx(), call(`{"query":"x","limit":99}`))
	if res.IsError || resultText(t, res) != recallToolEmpty {
		t.Fatalf("空结果应返回 recallToolEmpty：%+v", res)
	}
	if svc.reqs[0].Limit != maxRecallToolLimit {
		t.Fatalf("limit 应取上限 %d：%d", maxRecallToolLimit, svc.reqs[0].Limit)
	}
}

// 参数错误与 Scope 缺失返回 IsError，不调用服务；服务失败返回 IsError。
func TestRecallToolErrors(t *testing.T) {
	svc := &recordingService{}
	tl := RecallTool(svc, quietLog())
	for _, args := range []string{``, `{}`, `{"query":"  "}`, `{bad`} {
		if res := tl.Exec(scopedCtx(), call(args)); !res.IsError {
			t.Fatalf("args %q 应返回错误结果：%+v", args, res)
		}
	}
	if res := tl.Exec(context.Background(), call(`{"query":"x"}`)); !res.IsError {
		t.Fatalf("Scope 缺失应返回错误结果：%+v", res)
	}
	if len(svc.reqs) != 0 {
		t.Fatalf("参数错误时不应调用服务：%v", svc.reqs)
	}
	svc.err = errors.New("down")
	res := tl.Exec(scopedCtx(), call(`{"query":"x"}`))
	if !res.IsError || !strings.Contains(resultText(t, res), "down") {
		t.Fatalf("服务失败应返回带原因的错误结果：%+v", res)
	}
}

// 服务阻塞时按工具的超时返回 IsError。
func TestRecallToolTimeout(t *testing.T) {
	tl := recallTool{svc: &recordingService{block: true}, log: quietLog(), timeout: 20 * time.Millisecond}
	begin := time.Now()
	res := tl.Exec(scopedCtx(), call(`{"query":"x"}`))
	if !res.IsError || time.Since(begin) > time.Second {
		t.Fatalf("超时应返回错误结果：%+v（耗时 %v）", res, time.Since(begin))
	}
}
