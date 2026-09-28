package toolkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aria/core/tool"
	"aria/pkg/message"
	"aria/runtime/artifact"
)

type staticTool struct {
	name string
	res  tool.Result
}

func (s staticTool) Def() tool.Def { return tool.Def{Name: s.name} }

func (s staticTool) Exec(_ context.Context, call tool.Call) tool.Result {
	out := s.res
	if out.CallID == "" {
		out.CallID = call.ID // 与真实工具一致：结果回填调用 ID
	}
	return out
}

func text(t string) tool.Result {
	return tool.Result{Blocks: []message.Block{message.TextBlock{Text: t}}}
}

func lastText(t *testing.T, res tool.Result) string {
	t.Helper()
	for i := len(res.Blocks) - 1; i >= 0; i-- {
		switch b := res.Blocks[i].(type) {
		case message.TextBlock:
			return b.Text
		case *message.TextBlock:
			return b.Text
		}
	}
	t.Fatal("no text block")
	return ""
}

// 未超限：结果原样透传（连内容都不改）。
func TestTruncatePassesShortResultThrough(t *testing.T) {
	store := artifact.NewMemory(0)
	inner := staticTool{name: "t", res: text("短结果")}
	out := Truncate(TruncateConfig{Store: store, Limit: 100})(inner).Exec(context.Background(), tool.Call{ID: "c1"})

	if len(out.Blocks) != 1 || lastText(t, out) != "短结果" {
		t.Fatalf("short result must pass through: %+v", out.Blocks)
	}
	if store.Len() != 0 {
		t.Fatal("short result must not be stored")
	}
}

// 超限：全文入库、模型只看到预览 + 引用，且 IsError/CallID 保留。
func TestTruncateStoresFullTextAndPointsToRef(t *testing.T) {
	store := artifact.NewMemory(0)
	full := strings.Repeat("数据块", 500) // 1500 runes
	res := text(full)
	res.IsError = true
	inner := staticTool{name: "search", res: res}

	out := Truncate(TruncateConfig{Store: store, Limit: 100, Preview: 20})(inner).
		Exec(context.Background(), tool.Call{ID: "c7"})

	if !out.IsError || out.CallID != "c7" {
		t.Fatalf("semantics must be preserved: %+v", out)
	}
	got := lastText(t, out)
	if !strings.Contains(got, "输出过长已截断") || !strings.Contains(got, "共 1500 字符") {
		t.Fatalf("notice: %q", got)
	}
	if !strings.Contains(got, `artifact.open(ref="`) || !strings.Contains(got, "offset=20") {
		t.Fatalf("hint must include a usable call: %q", got)
	}
	if strings.Count(got, "数据块") > 8 {
		t.Fatalf("preview too long: %q", got)
	}
	if store.Len() != 1 {
		t.Fatalf("full text must be stored once: %d", store.Len())
	}

	// 提示里的参数真的要能用：提取 ref 后能读回原文
	ref := extractRef(t, got)
	back, err := store.Get(context.Background(), artifact.Ref(ref))
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != full {
		t.Fatal("stored text must equal the original full text")
	}
}

// 非文本块（图片）原样保留，只替换文本负载。
func TestTruncateKeepsNonTextBlocks(t *testing.T) {
	store := artifact.NewMemory(0)
	img := message.ImageBlock{MIME: "image/png", Data: []byte{1, 2, 3}}
	inner := staticTool{name: "shot", res: tool.Result{Blocks: []message.Block{
		message.TextBlock{Text: strings.Repeat("x", 5000)},
		img,
	}}}

	out := Truncate(TruncateConfig{Store: store, Limit: 100})(inner).Exec(context.Background(), tool.Call{ID: "c1"})
	if len(out.Blocks) != 2 {
		t.Fatalf("blocks: %d", len(out.Blocks))
	}
	if b, ok := out.Blocks[1].(message.ImageBlock); !ok || len(b.Data) != 3 {
		t.Fatalf("image must survive: %+v", out.Blocks[1])
	}
}

// 存档失败：不截断（宁可长，不可丢）。
func TestTruncateKeepsFullWhenStoreFails(t *testing.T) {
	full := strings.Repeat("y", 5000)
	inner := staticTool{name: "t", res: text(full)}

	out := Truncate(TruncateConfig{Store: failingStore{}, Limit: 100})(inner).
		Exec(context.Background(), tool.Call{ID: "c1"})

	if lastText(t, out) != full {
		t.Fatal("store failure must not truncate")
	}
}

// 指针形态的 TextBlock 也要被识别（pkg/message 的历史教训）。
func TestTruncateSeesPointerTextBlocks(t *testing.T) {
	store := artifact.NewMemory(0)
	inner := staticTool{name: "t", res: tool.Result{Blocks: []message.Block{
		&message.TextBlock{Text: strings.Repeat("z", 5000)},
	}}}

	out := Truncate(TruncateConfig{Store: store, Limit: 100})(inner).Exec(context.Background(), tool.Call{ID: "c1"})
	if store.Len() != 1 {
		t.Fatal("pointer text block must trigger truncation")
	}
	if !strings.Contains(lastText(t, out), "输出过长已截断") {
		t.Fatalf("notice missing: %q", lastText(t, out))
	}
}

// 无 Store：原样返回工具（无存放处就不截断）。
func TestTruncateWithoutStoreIsNoop(t *testing.T) {
	inner := staticTool{name: "t", res: text(strings.Repeat("q", 5000))}
	wrapped := Truncate(TruncateConfig{})(inner)
	if _, ok := wrapped.(staticTool); !ok {
		t.Fatalf("must return the original tool, got %T", wrapped)
	}
}

type failingStore struct{}

func (failingStore) Put(context.Context, []byte) (artifact.Ref, error) {
	return "", errors.New("store down")
}

func (failingStore) Get(context.Context, artifact.Ref) ([]byte, error) {
	return nil, errors.New("store down")
}

func extractRef(t *testing.T, hint string) string {
	t.Helper()
	const marker = `ref="`
	i := strings.Index(hint, marker)
	if i < 0 {
		t.Fatalf("no ref in hint: %q", hint)
	}
	rest := hint[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated ref in hint: %q", hint)
	}
	return rest[:j]
}
