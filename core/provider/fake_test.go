package provider

import (
	"context"
	"testing"
)

// Fake 最终消息必须携带思考内容（与 MessageComplete 契约一致），不得只拼文本。
func TestFakeFinalMessageKeepsThought(t *testing.T) {
	f := NewFake(FakeStep{Thought: []string{"想", "一下"}, Text: []string{"答"}})
	ch, err := f.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	var final MessageComplete
	for ev := range ch {
		if c, ok := ev.(MessageComplete); ok {
			final = c
		}
	}
	if final.Message.Thought() != "想一下" {
		t.Fatalf("thought lost in final message: %q", final.Message.Thought())
	}
	if final.Message.Text() != "答" {
		t.Fatalf("text: %q", final.Message.Text())
	}
}
