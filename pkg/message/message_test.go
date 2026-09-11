package message

import (
	"encoding/json"
	"testing"
)

func TestToolResultMessageRoundTrip(t *testing.T) {
	r := ToolResult{CallID: "call-1", Blocks: []Block{TextBlock{Text: "failed"}}, IsError: true}
	m := r.ToMessage()
	if m.Role != RoleTool || !m.IsError {
		t.Fatalf("ToMessage lost tool error: %+v", m)
	}
	got, ok := ToolResultFromMessage(m)
	if !ok || got.CallID != r.CallID || !got.IsError || got.Blocks[0].(TextBlock).Text != "failed" {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
	if _, ok := ToolResultFromMessage(NewAssistant("no")); ok {
		t.Fatal("assistant message must not convert to ToolResult")
	}
}

func TestCloneDeepCopiesMutableData(t *testing.T) {
	m := Message{
		Role: RoleAssistant,
		Blocks: []Block{
			ImageBlock{Data: []byte{1, 2}},
			AudioBlock{Data: []byte{3, 4}},
		},
		ToolCalls: []ToolCall{{ID: "call-1", Args: json.RawMessage(`{"city":"x"}`)}},
	}
	clone := m.Clone()
	m.Blocks[0].(ImageBlock).Data[0] = 9
	m.Blocks[1].(AudioBlock).Data[0] = 9
	m.ToolCalls[0].Args[0] = 'x'
	m.Blocks = append(m.Blocks, TextBlock{Text: "later"})
	m.ToolCalls = append(m.ToolCalls, ToolCall{ID: "later"})

	if got := clone.Blocks[0].(ImageBlock).Data[0]; got != 1 {
		t.Fatalf("image data aliases original: %d", got)
	}
	if got := clone.Blocks[1].(AudioBlock).Data[0]; got != 3 {
		t.Fatalf("audio data aliases original: %d", got)
	}
	if got := clone.ToolCalls[0].Args[0]; got != '{' {
		t.Fatalf("tool args alias original: %q", got)
	}
	if len(clone.Blocks) != 2 || len(clone.ToolCalls) != 1 {
		t.Fatalf("slices alias original: blocks=%d calls=%d", len(clone.Blocks), len(clone.ToolCalls))
	}
}

func TestToolResultCloneDeepCopiesBlocks(t *testing.T) {
	r := ToolResult{Blocks: []Block{ImageBlock{Data: []byte{1}}}}
	clone := r.Clone()
	r.Blocks[0].(ImageBlock).Data[0] = 2
	if clone.Blocks[0].(ImageBlock).Data[0] != 1 {
		t.Fatal("ToolResult.Clone data aliases original")
	}
	if (Message{}).Clone().Blocks != nil || (ToolResult{}).Clone().Blocks != nil {
		t.Fatal("Clone must preserve nil slices")
	}
}

// 指针形态的 Block 同样实现 Block 接口，CloneBlock 不得让其绕过深拷贝。
func TestCloneBlockPointerForms(t *testing.T) {
	tb := &TextBlock{Text: "a"}
	m := Message{Blocks: []Block{tb, &ImageBlock{Data: []byte{1}}}}
	clone := m.Clone()
	tb.Text = "b"
	if clone.Blocks[0].(*TextBlock).Text != "a" {
		t.Fatal("pointer TextBlock aliases original across Clone")
	}
	if _, ok := clone.Blocks[0].(*TextBlock); !ok {
		t.Fatal("pointer block form must be preserved")
	}
	clone.Blocks[1].(*ImageBlock).Data[0] = 9
	if tb2, ok := m.Blocks[1].(*ImageBlock); !ok || tb2.Data[0] != 1 {
		t.Fatal("pointer ImageBlock data aliases original across Clone")
	}
}

// 读取方法不得静默忽略指针形态的 Block（与 CloneBlock 指针分支同一契约）。
func TestReadMethodsSeePointerBlocks(t *testing.T) {
	m := Message{Blocks: []Block{
		&TextBlock{Text: "你好"},
		&ThoughtBlock{Text: "推理"},
		TextBlock{Text: "世界"},
	}}
	if got := m.Text(); got != "你好世界" {
		t.Fatalf("Text missed pointer block: %q", got)
	}
	if got := m.Thought(); got != "推理" {
		t.Fatalf("Thought missed pointer block: %q", got)
	}
	var nilText *TextBlock
	if got := (Message{Blocks: []Block{nilText}}).Text(); got != "" {
		t.Fatalf("nil pointer block must be skipped: %q", got)
	}
}
