package provider

import (
	"context"
	"encoding/json"
	"testing"

	"aria/pkg/message"
)

// 通用估算：中日韩字符每个 1 token，其他非空白字符每个 0.34 token，图片每个 1024 token，
// 每条消息另加 4 token；工具调用的名称与参数计入，空白不计。
func TestEstimateTokensCountsByScript(t *testing.T) {
	cases := []struct {
		name string
		msgs []message.Message
		want int
	}{
		{"中文", []message.Message{message.NewUser("今天天气")}, 4 + 4},
		{"英文与空白", []message.Message{message.NewUser("ab c")}, 4 + 2}, // 3 × 0.34 = 1.02，合计向上取整为 6
		{"图片", []message.Message{{Role: message.RoleUser, Blocks: []message.Block{message.ImageBlock{}}}}, 4 + 1024},
		{"工具调用", []message.Message{{Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{Name: "now", Args: json.RawMessage(`{}`)}}}}, 4 + 2}, // 5 × 0.34 = 1.7
		{"空", nil, 0},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.msgs); got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// CountTokens：provider 提供估算时采用其结果，返回负数时改用通用估算。
func TestCountTokensFallsBackToEstimate(t *testing.T) {
	msgs := []message.Message{message.NewUser("早安")}
	if got := CountTokens(fixedCount(7), "", msgs); got != 7 {
		t.Fatalf("应采用 provider 的估算 7，got %d", got)
	}
	if got, want := CountTokens(NewFake(), "", msgs), EstimateTokens(msgs); got != want {
		t.Fatalf("provider 不提供估算时应改用通用估算 %d，got %d", want, got)
	}
}

// Fake 缺省窗口未知；WithLimits 设置后对所有模型返回同一限额。
func TestFakeLimits(t *testing.T) {
	f := NewFake()
	if l := f.Limits(""); l != (Limits{}) {
		t.Fatalf("缺省应为零值，got %+v", l)
	}
	want := Limits{ContextWindow: 100, MaxOutput: 10}
	if l := f.WithLimits(want).Limits("any"); l != want {
		t.Fatalf("got %+v want %+v", l, want)
	}
}

type fixedCount int

func (fixedCount) Stream(context.Context, Request) (<-chan StreamEvent, error) { return nil, nil }
func (fixedCount) Limits(string) Limits                                        { return Limits{} }
func (n fixedCount) CountTokens(string, []message.Message) int                 { return int(n) }
