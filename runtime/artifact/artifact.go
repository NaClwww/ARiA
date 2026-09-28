// Package artifact 实现 docs/03 §1 的「artifact + ref」数据传递：大中间产物
// （超长工具结果、搜索结果等）存进外部存储，契约里只传引用，按需取回。
//
// 窄接口定义在消费方（03 §1）：runtime 只认 Store，内存/文件/对象存储的实现
// 由 Setup 注入；本包自带一个内存实现供主线与测试使用。
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"

	"aria/core/tool"
	"aria/pkg/message"
)

// Ref 是 artifact 的引用（不透明字符串，内容由 Store 决定）。
type Ref string

// ErrNotFound 表示引用不存在（或已被淘汰）。
var ErrNotFound = errors.New("artifact: ref not found")

// Store 是 artifact 的存放能力。Put 返回新引用；Get 取回全文。
// 实现必须尊重 ctx 取消，并且并发安全（工具可能并行调用）。
type Store interface {
	Put(ctx context.Context, content []byte) (Ref, error)
	Get(ctx context.Context, ref Ref) ([]byte, error)
}

// ---------- 内存实现 ----------

// Memory 是进程内 artifact 存储：超过 maxEntries 条时按插入顺序淘汰最旧的。
// 它是主线的默认实现；长期保存、跨进程共享应换持久化实现（plugins）。
type Memory struct {
	mu    sync.Mutex
	max   int // <=0 表示不淘汰
	order []Ref
	data  map[Ref][]byte
}

func NewMemory(maxEntries int) *Memory {
	return &Memory{max: maxEntries, data: make(map[Ref][]byte)}
}

func (m *Memory) Put(_ context.Context, content []byte) (Ref, error) {
	buf := make([]byte, len(content))
	copy(buf, content)
	ref := Ref(newRef())
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[ref] = buf
	m.order = append(m.order, ref)
	for m.max > 0 && len(m.order) > m.max {
		old := m.order[0]
		m.order = m.order[1:]
		delete(m.data, old)
	}
	return ref, nil
}

func (m *Memory) Get(_ context.Context, ref Ref) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.data[ref]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// Len 返回当前条数（观测与测试用）。
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

func newRef() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退化为时间无关的固定长度串没有意义，
		// 这里直接 panic 会让整个进程受影响，因此用空串前缀兜底。
		return "ref"
	}
	return hex.EncodeToString(b)
}

// ---------- 读取工具 ----------

// OpenToolName 是读取 artifact 的工具名（截断提示里引用它）。
const OpenToolName = "artifact_open"

// DefaultOpenLimit 是单次读取的默认字符数（rune）。
const DefaultOpenLimit = 8000

// MaxOpenLimit 是单次读取的字符上限（防止一次读回天文数字）。
const MaxOpenLimit = 64000

// OpenTool 返回读取 artifact 的工具：按 ref 取回内容，支持 offset/limit
// 分段读取（字符为单位，UTF-8 安全）。它必须由宿主装配进工具表——通常由
// agent 在启用截断时自动注册（见 runtime/agent）。
func OpenTool(store Store) tool.Tool { return &openTool{store: store} }

type openTool struct{ store Store }

func (*openTool) Def() tool.Def {
	return tool.Def{
		Name: OpenToolName,
		Description: "读取被截断或存档的工具输出（artifact）。按 ref 取回内容，" +
			"可用 offset/limit 分段继续读取；截断提示里给出的参数可直接照用。",
		Parameters: []byte(`{
  "type": "object",
  "properties": {
    "ref":    {"type": "string",  "description": "artifact 引用（截断提示中给出）"},
    "offset": {"type": "integer", "description": "起始字符偏移（rune），默认 0"},
    "limit":  {"type": "integer", "description": "读取字符数（rune），默认 8000"}
  },
  "required": ["ref"]
}`),
	}
}

func (t *openTool) Exec(ctx context.Context, call tool.Call) tool.Result {
	var args struct {
		Ref    string `json:"ref"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return errResult(call, "artifact_open: 参数无法解析: "+err.Error())
		}
	}
	if args.Ref == "" {
		return errResult(call, "artifact_open: 缺少 ref")
	}
	if args.Offset < 0 {
		args.Offset = 0
	}
	if args.Limit <= 0 {
		args.Limit = DefaultOpenLimit
	}
	if args.Limit > MaxOpenLimit {
		args.Limit = MaxOpenLimit
	}

	buf, err := t.store.Get(ctx, Ref(args.Ref))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return errResult(call, "artifact_open: 引用不存在或已过期（ref="+args.Ref+"）")
		}
		return errResult(call, "artifact_open: 读取失败: "+err.Error())
	}

	runes := []rune(string(buf))
	total := len(runes)
	if args.Offset >= total {
		return textResult(call, notice(args.Ref, total, total)+"\n（offset 已到末尾）")
	}
	end := args.Offset + args.Limit
	if end > total {
		end = total
	}
	body := string(runes[args.Offset:end])
	return textResult(call, notice(args.Ref, args.Offset, end)+"\n"+body)
}

func notice(ref string, start, end int) string {
	return "[" + OpenToolName + " ref=" + ref + " 字符 " +
		strconv.Itoa(start) + "-" + strconv.Itoa(end) + "]"
}

func textResult(call tool.Call, text string) tool.Result {
	return tool.Result{
		CallID: call.ID,
		Blocks: []message.Block{message.TextBlock{Text: text}},
	}
}

func errResult(call tool.Call, text string) tool.Result {
	return tool.Result{
		CallID:  call.ID,
		IsError: true,
		Blocks:  []message.Block{message.TextBlock{Text: text}},
	}
}
