package memory

import (
	"context"
	"encoding/json"
	"strings"

	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// memory_recall 工具的取值（docs/memory/options.md「写入与召回流程」已定第 11 项）。
const (
	RecallToolName         = "memory_recall"
	DefaultRecallToolLimit = 10 // limit 缺省值
	MaxRecallToolLimit     = 20 // limit 上限，超过按上限取
	// RecallToolEmpty 是没有检索结果时返回模型的文本。
	RecallToolEmpty = "没有相关的长期记忆。"
)

// RecallToolInstruction 是装入 memory_recall 工具时追加到人设之后的使用规则：工具存在与否是
// 宿主的装配事实，由宿主在配置了记忆服务时追加。
const RecallToolInstruction = "长期记忆：对话涉及家庭成员的经历、偏好、过往约定或 ARiA 答应过的事，" +
	"且当前上下文中没有依据时，先调用 memory_recall 检索再作答；检索结果属于背景资料，可能过时。"

var recallToolParams = json.RawMessage(`{"type":"object","properties":{` +
	`"query":{"type":"string","description":"检索内容：涉及的人名、事件或话题，自然语言"},` +
	`"limit":{"type":"integer","description":"返回条数上限，缺省 10，最大 20"}},` +
	`"required":["query"]}`)

// recallTool 是模型主动检索长期记忆的工具：参数 query（必填）、limit（可选）；经 Client.Recall
// 调用记忆服务（超时 DefaultRecallTimeout），结果经 Render 渲染为逐行文本。namespace 与 session_id
// 取自 ctx 的 Scope；Scope 缺失、query 为空或检索失败时返回 IsError 结果。
type recallTool struct{ c *Client }

// RecallTool 返回 memory_recall 工具；c 为记忆服务的 Client（与 agent 使用同一个 Service）。
func RecallTool(c *Client) tool.Tool { return recallTool{c: c} }

func (t recallTool) Def() tool.Def {
	return tool.Def{
		Name:        RecallToolName,
		Description: "检索长期记忆：家庭成员的事实、偏好、经历与 ARiA 的承诺。参数 query=检索内容，limit=条数上限（缺省 10，最大 20）。",
		Parameters:  recallToolParams,
	}
}

func (t recallTool) Exec(ctx context.Context, call tool.Call) tool.Result {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if args := strings.TrimSpace(string(call.Args)); args != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return errorResult(call.ID, "memory_recall: 参数不是合法 JSON: "+err.Error())
		}
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return errorResult(call.ID, "memory_recall: 缺少 query 参数")
	}
	if in.Limit <= 0 {
		in.Limit = DefaultRecallToolLimit
	} else if in.Limit > MaxRecallToolLimit {
		in.Limit = MaxRecallToolLimit
	}
	sc, ok := ctxx.ScopeFrom(ctx)
	if !ok {
		return errorResult(call.ID, "memory_recall: ctx 缺少 Scope")
	}
	items, err := t.c.Recall(ctx, RecallRequest{
		Namespace: sc.Namespace, SessionID: sc.SessionID, Query: in.Query, Limit: in.Limit,
	})
	if err != nil {
		return errorResult(call.ID, "memory_recall: 检索失败: "+err.Error())
	}
	text := RecallToolEmpty
	if len(items) > 0 {
		text = Render(items)
	}
	return tool.Result{CallID: call.ID, Blocks: []message.Block{message.TextBlock{Text: text}}}
}

func errorResult(callID, text string) tool.Result {
	return tool.Result{CallID: callID, IsError: true, Blocks: []message.Block{message.TextBlock{Text: text}}}
}

// Render 把召回条目渲染为逐行文本：有发生时间的条目带本地时间前缀，暂存条目标注「未合并」。
// 会话开始的召回（agent 写入窗口）与 memory_recall 工具的结果共用。
func Render(items []Item) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		if !it.At.IsZero() {
			b.WriteString("[" + it.At.Local().Format("2006-01-02 15:04") + "] ")
		}
		b.WriteString(it.Text)
		if it.Source == SourceStaged {
			b.WriteString("（未合并）")
		}
	}
	return b.String()
}
