package window

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/runtime/memory"
)

// DefaultSummaryInstruction 是间隙压缩的默认提示词。
const DefaultSummaryInstruction = "把下面的对话压缩成简洁的摘要，保留：谁说了什么关键信息、" +
	"已达成的结论、未完成的事项、与用户相关的偏好。assistant 的工具调用记录着它" +
	"实际做了什么、说了什么（调用参数就是内容，如语音播报的原文），必须一并保留，" +
	"不要只留「已说完」之类的结果状态。不要添加原文没有的内容。"

// callArgsLimit 是单个工具调用参数的渲染上限（rune）：长 bash 命令保头去尾，
// 摘要要的是「做了什么」的事实，不是命令全文。
const callArgsLimit = 400

// truncRunes 按 rune 截断超长字符串，保头去尾加省略号。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ProviderCompressor 用一次 Provider 调用把「记忆 + 本轮」压成一段摘要（03 §5）。
// 它是 Compressor 的默认 LLM 实现；换压缩策略只需换 Compressor，窗口不动。
// 它同时实现 PointExtractor：窗口注册了要点上报（Window.SetOnPoints）时，压缩调用一并输出要点。
//
// ctx 若带 ctxx.Options（model、temperature…）会作为兜底；本结构体字段优先。
// MaxTokens 例外——不继承 ctx（见 Compress），仅本字段显式设置才生效。
type ProviderCompressor struct {
	Provider provider.Provider

	Model       string // 空 = 用 ctx 里 ctxx.Options 的模型
	MaxTokens   int    // 摘要输出上限；0 = 不限，也不继承主对话的配置
	Instruction string // 空 = DefaultSummaryInstruction

	PointsRules string           // 要点提取规则；空 = DefaultPointsRules()
	Now         func() time.Time // 要点提取提示词中的当前时间；nil = time.Now
}

func (p *ProviderCompressor) Compress(ctx context.Context, mem, turn []message.Message) ([]message.Message, error) {
	if p.Provider == nil {
		return nil, errors.New("window: ProviderCompressor requires Provider")
	}
	body := renderTranscript(append(cloneAll(mem), cloneAll(turn)...))
	if body == "" {
		return nil, nil
	}
	summary, err := p.complete(ctx, p.summaryInstruction(), body)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		return nil, errors.New("window: compressor produced empty summary")
	}
	// 记忆走非 system 的 <memory> 块：内容来自对话，属不可信数据
	// （03 §5 上下文分层——不可信内容永不进 system role）。
	return []message.Message{MemoryMessage(summary)}, nil
}

// summaryInstruction 返回摘要提示词：Instruction 为空时取 DefaultSummaryInstruction。
func (p *ProviderCompressor) summaryInstruction() string {
	if p.Instruction != "" {
		return p.Instruction
	}
	return DefaultSummaryInstruction
}

// renderTranscript 把消息渲染为逐行转写：工具调用为「角色 调用 名称: 参数」，正文为「角色: 正文」；
// 无内容时返回空串。
func renderTranscript(msgs []message.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		// 工具调用先于正文渲染：参数是 assistant 实际做过的事/说过的话。
		// speak 类宿主里 assistant 正文恒空，调用参数就是它的口头回复——
		// 丢了它摘要只剩「已说完」空壳，会凭空推出未完成事项
		// （2026-09-29 真机：摘要据此认定「未确认用户是否被看到」）。
		for _, c := range m.ToolCalls {
			fmt.Fprintf(&b, "%s 调用 %s: %s\n", m.Role, c.Name, truncRunes(string(c.Args), callArgsLimit))
		}
		text := strings.TrimSpace(m.Text())
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, text)
	}
	return b.String()
}

// complete 以 instruction 为 system、body 为用户消息调用一次 Provider，返回去掉首尾空白的回复正文。
func (p *ProviderCompressor) complete(ctx context.Context, instruction, body string) (string, error) {
	// 本结构体字段优先，其次是调用方 ctx 里的 Options（模型覆盖等）。
	// MaxTokens 不继承：主对话的输出上限套在摘要上，思考型模型会把
	// 限额全烧在推理里、正文一字未出即被截断——落进窗口的就是
	// 「空摘要」失败（2026-09-28 真机实测）。上限只有显式给才生效。
	opts, _ := ctxx.OptionsFrom(ctx)
	if p.Model != "" {
		opts.Model = p.Model
	}
	opts.MaxTokens = p.MaxTokens
	req := provider.Request{
		Messages: []message.Message{
			message.NewSystem(instruction),
			message.NewUser(body),
		},
		Options: opts,
	}

	ch, err := p.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var text string
	for ev := range ch {
		switch e := ev.(type) {
		case provider.MessageComplete:
			if e.Interrupted {
				return "", errors.New("window: compression interrupted")
			}
			text = strings.TrimSpace(e.Message.Text())
		case provider.ErrorEvent:
			return "", e.Err
		}
	}
	return text, nil
}

// ---------- 要点提取（PointExtractor） ----------

// 要点提取的上限（docs/memory/options.md「要点的格式」）。
const (
	MaxPointsPerBatch = 10  // 每批要点的条数上限，超出部分截去
	SummaryMaxChars   = 800 // 摘要的字数上限，由提示词限定，输出不截断
)

// DefaultPointsRules 返回要点的缺省提取规则（docs/memory/options.md「要点的格式」）。
func DefaultPointsRules() string { return defaultPointsRules }

var defaultPointsRules = "要点按以下规则提取：只记录涉及家庭成员或 ARiA 的事实、偏好、事件，以及 ARiA 作出的承诺；" +
	"不记录访客（未识别说话人）的个人事实；不记录寒暄与无信息量的往来；" +
	fmt.Sprintf("每条要点独立成句并写明主语；最多 %d 条，没有需要记录的内容时 points 为空数组。", MaxPointsPerBatch) +
	"kind 取 fact（事实）、preference（偏好）、event（事件）、commitment（ARiA 的承诺）之一；" +
	"speakers 列出涉及的说话人名字（取自消息开头的 [名字] 标注）；" +
	"commitment 须给出 due（到期时间，RFC 3339 格式，相对时间按当前时间换算），其他类别的 due 留空。"

// summaryLimit 是限定摘要长度的提示词。
var summaryLimit = fmt.Sprintf("摘要不超过 %d 字。", SummaryMaxChars)

const (
	extractionFormat = "只输出一个 JSON 对象，不输出其他内容，格式：" +
		`{"summary": "摘要", "points": [{"text": "要点", "kind": "event", "speakers": ["名字"], "due": ""}]}`
	pointsOnlyFormat = "只输出一个 JSON 对象，不输出其他内容，格式：" +
		`{"points": [{"text": "要点", "kind": "event", "speakers": ["名字"], "due": ""}]}`
)

// sectionEscaper 把渲染内容中与分段标记相同的文本改为方括号形式，以避免对话内容被识别为分段标记。
var sectionEscaper = strings.NewReplacer("【既往摘要】", "[既往摘要]", "【本批对话】", "[本批对话]")

// CompressWithPoints 实现 PointExtractor：一次调用输出新摘要与要点。输入分两段（renderSections）：旧摘要 mem
// 列为「既往摘要」，本批 turn 列为「本批对话」；摘要涵盖两段，要点只从「本批对话」提取，以避免重复提取已暂存
// 的内容。本批没有可渲染的内容时按 Compress 只生成摘要。模型输出不是合法 JSON 对象时返回错误：窗口按压缩失败
// 回退，本批原文在下一批压缩时重新提取。
func (p *ProviderCompressor) CompressWithPoints(ctx context.Context, mem, turn []message.Message) ([]message.Message, []memory.Point, error) {
	if p.Provider == nil {
		return nil, nil, errors.New("window: ProviderCompressor requires Provider")
	}
	body := renderSections(mem, turn)
	if body == "" {
		out, err := p.Compress(ctx, mem, turn)
		return out, nil, err
	}
	instruction := p.summaryInstruction() + "摘要须涵盖「既往摘要」与「本批对话」两段的内容。" + summaryLimit +
		"\n\n同时提取需要长期记住的要点：要点只从「本批对话」提取，「既往摘要」只用于理解上下文。" + p.pointsRules() +
		"\n\n" + extractionFormat + p.nowLine()
	text, err := p.complete(ctx, instruction, body)
	if err != nil {
		return nil, nil, err
	}
	ext, err := parseExtraction(text)
	if err != nil {
		return nil, nil, err
	}
	summary := strings.TrimSpace(ext.summary)
	if summary == "" {
		return nil, nil, errors.New("window: compressor produced empty summary")
	}
	return []message.Message{MemoryMessage(summary)}, ext.points, nil
}

// ExtractPoints 实现 PointExtractor：只提取要点、不生成摘要（会话切换时）。mem 为会话内的压缩摘要，列为
// 「既往摘要」只用于理解上下文（例如解决指代）；要点只从「本批对话」turn 提取。模型输出不是合法 JSON 对象时
// 返回错误。
func (p *ProviderCompressor) ExtractPoints(ctx context.Context, mem, turn []message.Message) ([]memory.Point, error) {
	if p.Provider == nil {
		return nil, errors.New("window: ProviderCompressor requires Provider")
	}
	body := renderSections(mem, turn)
	if body == "" {
		return nil, nil
	}
	instruction := "从「本批对话」中提取需要长期记住的要点，「既往摘要」只用于理解上下文。" + p.pointsRules() +
		"\n\n" + pointsOnlyFormat + p.nowLine()
	text, err := p.complete(ctx, instruction, body)
	if err != nil {
		return nil, err
	}
	ext, err := parseExtraction(text)
	if err != nil {
		return nil, err
	}
	return ext.points, nil
}

// renderSections 渲染要点提取的输入：mem 有内容时先列「既往摘要」段，再列「本批对话」段，两段内容中与
// 分段标记相同的文本经 sectionEscaper 改写；turn 没有可渲染的内容时返回空串。
func renderSections(mem, turn []message.Message) string {
	batch := renderTranscript(turn)
	if batch == "" {
		return ""
	}
	var b strings.Builder
	if m := renderTranscript(mem); m != "" {
		b.WriteString("【既往摘要】\n")
		b.WriteString(sectionEscaper.Replace(m))
		b.WriteString("\n")
	}
	b.WriteString("【本批对话】\n")
	b.WriteString(sectionEscaper.Replace(batch))
	return b.String()
}

func (p *ProviderCompressor) pointsRules() string {
	if p.PointsRules != "" {
		return p.PointsRules
	}
	return defaultPointsRules
}

// nowLine 返回提示词末尾的当前时间行（RFC 3339），供模型换算相对时间。
func (p *ProviderCompressor) nowLine() string {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	return "\n当前时间：" + now().Format(time.RFC3339)
}

// extraction 是解析后的模型输出。
type extraction struct {
	summary string
	points  []memory.Point
}

// parseExtraction 从模型输出中第一个「{」起解码一个 JSON 对象，对象之后的文本（例如代码块的结束标记）忽略；
// 不是合法 JSON 对象时返回错误。要点逐条解码与校验：字段类型不符的条目跳过；空文本跳过；未知类别置空；
// due 只对 commitment 按 RFC 3339 解析，不合法时置零；超过 MaxPointsPerBatch 的部分截去。
func parseExtraction(text string) (extraction, error) {
	start := strings.Index(text, "{")
	if start < 0 {
		return extraction{}, errors.New("window: points output has no JSON object")
	}
	var wire struct {
		Summary string            `json:"summary"`
		Points  []json.RawMessage `json:"points"`
	}
	if err := json.NewDecoder(strings.NewReader(text[start:])).Decode(&wire); err != nil {
		return extraction{}, fmt.Errorf("window: points output is not a valid JSON object: %w", err)
	}
	out := extraction{summary: wire.Summary}
	for _, raw := range wire.Points {
		var wp struct {
			Text     string   `json:"text"`
			Kind     string   `json:"kind"`
			Speakers []string `json:"speakers"`
			Due      string   `json:"due"`
		}
		if json.Unmarshal(raw, &wp) != nil {
			continue
		}
		t := strings.TrimSpace(wp.Text)
		if t == "" {
			continue
		}
		pt := memory.Point{Text: t, Speakers: wp.Speakers}
		switch k := memory.Kind(wp.Kind); k {
		case memory.KindFact, memory.KindPreference, memory.KindEvent, memory.KindCommitment:
			pt.Kind = k
		}
		if pt.Kind == memory.KindCommitment && wp.Due != "" {
			if due, err := time.Parse(time.RFC3339, wp.Due); err == nil {
				pt.Due = due
			}
		}
		out.points = append(out.points, pt)
		if len(out.points) == MaxPointsPerBatch {
			break
		}
	}
	return out, nil
}
