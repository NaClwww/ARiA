// Package message 定义 provider 无关的规范消息模型（docs/01 §2，A1 定稿版）。
package message

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Block 是多模态内容块。实现：TextBlock / ImageBlock / AudioBlock / FileBlock / ThoughtBlock。
type Block interface{ isBlock() }

type TextBlock struct{ Text string }

// ImageBlock 的 URL 与 Data 二选一。
type ImageBlock struct {
	URL  string
	Data []byte
	MIME string
}

type AudioBlock struct {
	Data []byte
	MIME string
}

// FileBlock 携带引用而非内容本体（artifact+ref，03 §1 数据传递第二类）。
type FileBlock struct {
	Name string
	URI  string
	MIME string
}

// ThoughtBlock 承载 GLM/R1/o 系 reasoning 内容；默认不入窗，入窗策略随 G2 重论。
type ThoughtBlock struct{ Text string }

func (TextBlock) isBlock()    {}
func (ImageBlock) isBlock()   {}
func (AudioBlock) isBlock()   {}
func (FileBlock) isBlock()    {}
func (ThoughtBlock) isBlock() {}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type ToolResult struct {
	CallID  string
	Blocks  []Block
	IsError bool
}

// ToMessage 是工具结果进入历史的唯一规则（canonical transcript）。
func (r ToolResult) ToMessage() Message {
	return Message{Role: RoleTool, ToolCallID: r.CallID, Blocks: r.Blocks}
}

type Message struct {
	ID          string
	Role        Role
	Blocks      []Block
	ToolCalls   []ToolCall // 仅 assistant：本轮发起的工具调用，重放给 provider 时原样携带
	ToolCallID  string     // 仅 tool：本条结果响应哪个 ToolCall.ID
	Interrupted bool       // steering 打断时保留的部分输出标记
}

func NewSystem(text string) Message {
	return Message{Role: RoleSystem, Blocks: []Block{TextBlock{Text: text}}}
}

func NewUser(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{TextBlock{Text: text}}}
}

func NewAssistant(text string) Message {
	return Message{Role: RoleAssistant, Blocks: []Block{TextBlock{Text: text}}}
}

// Text 拼接全部 TextBlock（不含 Thought）。
func (m Message) Text() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func (m Message) Thought() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if t, ok := blk.(ThoughtBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

type Usage struct {
	In     int
	Out    int
	Cached int
	Cost   float64
}

func (u Usage) Add(v Usage) Usage {
	return Usage{
		In:     u.In + v.In,
		Out:    u.Out + v.Out,
		Cached: u.Cached + v.Cached,
		Cost:   u.Cost + v.Cost,
	}
}
