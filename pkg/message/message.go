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

func (b TextBlock) Clone() TextBlock { return b }

// ImageBlock 的 URL 与 Data 二选一。
type ImageBlock struct {
	URL  string
	Data []byte
	MIME string
}

func (b ImageBlock) Clone() ImageBlock {
	b.Data = cloneBytes(b.Data)
	return b
}

type AudioBlock struct {
	Data []byte
	MIME string
}

func (b AudioBlock) Clone() AudioBlock {
	b.Data = cloneBytes(b.Data)
	return b
}

// FileBlock 携带引用而非内容本体（artifact+ref，03 §1 数据传递第二类）。
type FileBlock struct {
	Name string
	URI  string
	MIME string
}

func (b FileBlock) Clone() FileBlock { return b }

// ThoughtBlock 承载 GLM/R1/o 系 reasoning 内容；默认不入窗，入窗策略随 G2 重论。
type ThoughtBlock struct{ Text string }

func (b ThoughtBlock) Clone() ThoughtBlock { return b }

func (TextBlock) isBlock()    {}
func (ImageBlock) isBlock()   {}
func (AudioBlock) isBlock()   {}
func (FileBlock) isBlock()    {}
func (ThoughtBlock) isBlock() {}

// CloneBlock 深拷贝内建 Block（含指针形态）；未知实现原样返回。
func CloneBlock(b Block) Block {
	switch b := b.(type) {
	case TextBlock:
		return b.Clone()
	case *TextBlock:
		if b == nil {
			return b
		}
		c := b.Clone()
		return &c
	case ImageBlock:
		return b.Clone()
	case *ImageBlock:
		if b == nil {
			return b
		}
		c := b.Clone()
		return &c
	case AudioBlock:
		return b.Clone()
	case *AudioBlock:
		if b == nil {
			return b
		}
		c := b.Clone()
		return &c
	case FileBlock:
		return b.Clone()
	case *FileBlock:
		if b == nil {
			return b
		}
		c := b.Clone()
		return &c
	case ThoughtBlock:
		return b.Clone()
	case *ThoughtBlock:
		if b == nil {
			return b
		}
		c := b.Clone()
		return &c
	default:
		return b
	}
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

func (c ToolCall) Clone() ToolCall {
	c.Args = cloneBytes(c.Args)
	return c
}

type ToolResult struct {
	CallID  string
	Blocks  []Block
	IsError bool
}

func (r ToolResult) Clone() ToolResult {
	r.Blocks = cloneBlocks(r.Blocks)
	return r
}

// ToMessage 是工具结果进入历史的唯一规则（canonical transcript）。
func (r ToolResult) ToMessage() Message {
	r = r.Clone()
	return Message{Role: RoleTool, ToolCallID: r.CallID, Blocks: r.Blocks, IsError: r.IsError}
}

// ToolResultFromMessage 将 canonical tool-role Message 转回工具结果。
func ToolResultFromMessage(m Message) (ToolResult, bool) {
	if m.Role != RoleTool {
		return ToolResult{}, false
	}
	return ToolResult{CallID: m.ToolCallID, Blocks: cloneBlocks(m.Blocks), IsError: m.IsError}, true
}

type Message struct {
	ID          string
	Role        Role
	Blocks      []Block
	ToolCalls   []ToolCall // 仅 assistant：本轮发起的工具调用，重放给 provider 时原样携带
	ToolCallID  string     // 仅 tool：本条结果响应哪个 ToolCall.ID
	IsError     bool       // 仅 tool：工具执行是否失败
	Interrupted bool       // steering 打断时保留的部分输出标记
}

func (m Message) Clone() Message {
	m.Blocks = cloneBlocks(m.Blocks)
	if m.ToolCalls != nil {
		calls := m.ToolCalls
		m.ToolCalls = make([]ToolCall, len(calls))
		for i, call := range calls {
			m.ToolCalls[i] = call.Clone()
		}
	}
	return m
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
// Text 拼接全部 TextBlock（不含 Thought）；值/指针形态都识别。
func (m Message) Text() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		switch t := blk.(type) {
		case TextBlock:
			b.WriteString(t.Text)
		case *TextBlock:
			if t != nil {
				b.WriteString(t.Text)
			}
		}
	}
	return b.String()
}

func (m Message) Thought() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		switch t := blk.(type) {
		case ThoughtBlock:
			b.WriteString(t.Text)
		case *ThoughtBlock:
			if t != nil {
				b.WriteString(t.Text)
			}
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

func cloneBlocks(blocks []Block) []Block {
	if blocks == nil {
		return nil
	}
	cloned := make([]Block, len(blocks))
	for i, block := range blocks {
		cloned[i] = CloneBlock(block)
	}
	return cloned
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
