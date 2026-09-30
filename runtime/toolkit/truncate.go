// Package toolkit 放通用工具装饰器（docs/03 §3 挂点 4/5：结果变换与
// registry 全局包装）。装饰器是普通 Go 组合——无 Hook 抽象。
package toolkit

import (
	"context"
	"strconv"
	"strings"

	"aria/core/tool"
	"aria/pkg/message"
	"aria/runtime/artifact"
)

const (
	// DefaultLimit 是触发截断的默认字符数（rune）。
	DefaultLimit = 4000
	// DefaultPreview 是截断后保留的默认预览字符数（rune）。
	DefaultPreview = 1200
)

// TruncateConfig 配置结果截断装饰器。
type TruncateConfig struct {
	// Store 是全文的存放处；nil → 装饰器原样返回（无存储不截断：宁可长，不可丢）。
	Store artifact.Store

	// Limit 是触发截断的文本字符上限（rune）；<=0 → DefaultLimit。
	Limit int
	// Preview 是截断后保留的预览字符数；<=0 → DefaultPreview（且不超过 Limit）。
	Preview int
	// OpenToolName 是提示文本里给出的读取工具名；空 → artifact.OpenToolName。
	OpenToolName string
}

// Truncate 返回一个工具结果截断装饰器：
//
//	工具结果文本超过 Limit 时，全文存入 artifact Store，只把「预览 + 引用」
//	喂回模型（docs/03 §1 artifact+ref）；模型随后可用读取工具按需取回。
//
// 三条纪律：
//   - 存档失败时不截断（宁可长，不可丢）；
//   - 非文本块（图片等）原样保留（截断只针对文本负载）；
//   - IsError / CallID 原样保留（工具错误是内容，不能被装饰器改写语义）。
func Truncate(cfg TruncateConfig) func(tool.Tool) tool.Tool {
	limit := cfg.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	preview := cfg.Preview
	if preview <= 0 || preview > limit {
		preview = DefaultPreview
		if preview > limit {
			preview = limit
		}
	}
	openName := cfg.OpenToolName
	if openName == "" {
		openName = artifact.OpenToolName
	}
	return func(t tool.Tool) tool.Tool {
		if t == nil || cfg.Store == nil {
			return t
		}
		return &truncating{inner: t, store: cfg.Store, limit: limit, preview: preview, openName: openName}
	}
}

type truncating struct {
	inner    tool.Tool
	store    artifact.Store
	limit    int
	preview  int
	openName string
}

func (t *truncating) Def() tool.Def { return t.inner.Def() }

// Unwrap 交出内层工具（Go 惯例）：装饰器只变换结果负载，不吞内层的
// 可选能力（如 core/tool.Stopper）——消费方按 Unwrap 链解包探测。
// 真机教训（2026-09-29）：没有这一层，stop 工具执行成功 run 却续轮。
func (t *truncating) Unwrap() tool.Tool { return t.inner }

func (t *truncating) Exec(ctx context.Context, call tool.Call) tool.Result {
	res := t.inner.Exec(ctx, call)

	text, ok := joinText(res.Blocks)
	if !ok || len([]rune(text)) <= t.limit {
		return res
	}

	ref, err := t.store.Put(ctx, []byte(text))
	if err != nil {
		// 存档失败：原样返回全文（截断的前提是内容有地方可查）
		return res
	}

	runes := []rune(text)
	preview := string(runes[:t.preview])
	notice := "[" + t.inner.Def().Name + " 输出过长已截断] 完整内容共 " +
		strconv.Itoa(len(runes)) + " 字符，此处为前 " + strconv.Itoa(t.preview) + " 字符。" +
		"继续读取：" + t.openName + "(ref=\"" + string(ref) + "\", offset=" + strconv.Itoa(t.preview) + ", limit=" + strconv.Itoa(t.limit) + ")"

	blocks := make([]message.Block, 0, len(res.Blocks)+1)
	blocks = append(blocks, message.TextBlock{Text: notice + "\n" + preview})
	for _, b := range res.Blocks {
		if isText(b) {
			continue // 文本已被「提示 + 预览」替换
		}
		blocks = append(blocks, b)
	}

	out := res
	out.Blocks = blocks
	return out
}

// joinText 把文本块拼成一份负载（值形态与指针形态都认，见 pkg/message 的教训）。
func joinText(blocks []message.Block) (string, bool) {
	var b strings.Builder
	found := false
	for _, blk := range blocks {
		switch v := blk.(type) {
		case message.TextBlock:
			b.WriteString(v.Text)
			found = true
		case *message.TextBlock:
			if v != nil {
				b.WriteString(v.Text)
				found = true
			}
		}
	}
	return b.String(), found
}

func isText(b message.Block) bool {
	switch b.(type) {
	case message.TextBlock, *message.TextBlock:
		return true
	default:
		return false
	}
}
