package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"aria/core/tool"
	"aria/pkg/message"
)

func TestMemoryPutGetAndNotFound(t *testing.T) {
	m := NewMemory(0)
	ctx := context.Background()

	ref, err := m.Put(ctx, []byte("hello 世界"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello 世界" {
		t.Fatalf("content: %q", got)
	}
	if _, err := m.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// 取回的是副本：改写不影响存储
	got[0] = 'X'
	again, _ := m.Get(ctx, ref)
	if string(again) != "hello 世界" {
		t.Fatal("Memory leaked internal buffer")
	}
}

func TestMemoryEvictsOldest(t *testing.T) {
	m := NewMemory(2)
	ctx := context.Background()
	r1, _ := m.Put(ctx, []byte("one"))
	r2, _ := m.Put(ctx, []byte("two"))
	r3, _ := m.Put(ctx, []byte("three"))

	if m.Len() != 2 {
		t.Fatalf("len: %d", m.Len())
	}
	if _, err := m.Get(ctx, r1); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest must be evicted")
	}
	for _, r := range []Ref{r2, r3} {
		if _, err := m.Get(ctx, r); err != nil {
			t.Fatalf("ref %s should survive: %v", r, err)
		}
	}
}

func TestMemoryConcurrentAccess(t *testing.T) {
	m := NewMemory(0)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref, err := m.Put(ctx, []byte(fmt.Sprintf("v%d", i)))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := m.Get(ctx, ref); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

// ---------- artifact.open ----------

func openExec(t *testing.T, store Store, args string) tool.Result {
	t.Helper()
	return OpenTool(store).Exec(context.Background(), tool.Call{
		ID:   "c1",
		Name: OpenToolName,
		Args: []byte(args),
	})
}

func resultText(t *testing.T, res tool.Result) string {
	t.Helper()
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks: %+v", res.Blocks)
	}
	tb, ok := res.Blocks[0].(message.TextBlock)
	if !ok {
		t.Fatalf("block type: %T", res.Blocks[0])
	}
	return tb.Text
}

// 分段读取：offset/limit 以字符（rune）计，中文不被切断。
func TestOpenToolReadsRuneSlices(t *testing.T) {
	m := NewMemory(0)
	full := strings.Repeat("中文abc", 100) // 500 runes
	ref, _ := m.Put(context.Background(), []byte(full))

	res := openExec(t, m, fmt.Sprintf(`{"ref":%q,"offset":0,"limit":4}`, ref))
	if got := resultText(t, res); !strings.Contains(got, "中文ab") || strings.Contains(got, "中文abc") {
		t.Fatalf("first slice: %q", got)
	}

	res = openExec(t, m, fmt.Sprintf(`{"ref":%q,"offset":4,"limit":2}`, ref))
	if got := resultText(t, res); !strings.Contains(got, "c中") {
		t.Fatalf("second slice: %q", got)
	}

	// 尾部截断：offset+limit 超界时取到末尾为止
	res = openExec(t, m, fmt.Sprintf(`{"ref":%q,"offset":498,"limit":100}`, ref))
	if got := resultText(t, res); !strings.Contains(got, "字符 498-500") {
		t.Fatalf("tail slice header: %q", got)
	}
}

// 默认 limit 生效（不传 limit 时读 8000 字符上限内的全文）。
func TestOpenToolDefaultsLimit(t *testing.T) {
	m := NewMemory(0)
	ref, _ := m.Put(context.Background(), []byte("短内容"))
	res := openExec(t, m, fmt.Sprintf(`{"ref":%q}`, ref))
	if got := resultText(t, res); !strings.Contains(got, "短内容") {
		t.Fatalf("default read: %q", got)
	}
}

// 参数错误与坏引用都是「内容」而不是 panic（03：工具错误喂回模型）。
func TestOpenToolErrorsAreResults(t *testing.T) {
	m := NewMemory(0)
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bad json", `{`, "参数无法解析"},
		{"missing ref", `{}`, "缺少 ref"},
		{"unknown ref", `{"ref":"deadbeef"}`, "引用不存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := openExec(t, m, c.args)
			if !res.IsError {
				t.Fatal("must be an error result")
			}
			if got := resultText(t, res); !strings.Contains(got, c.want) {
				t.Fatalf("want %q in %q", c.want, got)
			}
		})
	}
	// offset 超过总长度：明确提示，不报错
	ref, _ := m.Put(context.Background(), []byte("abc"))
	res := openExec(t, m, fmt.Sprintf(`{"ref":%q,"offset":99}`, ref))
	if res.IsError || !strings.Contains(resultText(t, res), "offset 已到末尾") {
		t.Fatalf("out-of-range offset: %+v", res)
	}
}

func TestOpenToolDefIsValidJSONSchema(t *testing.T) {
	def := OpenTool(NewMemory(0)).Def()
	if def.Name != OpenToolName {
		t.Fatalf("name: %s", def.Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		t.Fatalf("parameters must be valid JSON schema: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema: %v", schema)
	}
}
