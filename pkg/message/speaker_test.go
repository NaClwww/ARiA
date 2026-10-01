package message

import "testing"

func TestSplitSpeaker(t *testing.T) {
	cases := []struct {
		in          string
		def         string
		wantSpeaker string
		wantText    string
	}{
		{"你好", "user", "user", "你好"},
		{"[小明] 你好呀", "user", "小明", "你好呀"},
		{"[小明]你好", "user", "小明", "你好"},
		{"[] 空", "user", "user", "[] 空"}, // 空名不算前缀
		{"[ 这是一个超长的名字肯定不是说话人] 嗯", "user", "user", "[ 这是一个超长的名字肯定不是说话人] 嗯"}, // >24 字符
		{"[未闭合 你好", "user", "user", "[未闭合 你好"},
	}
	for _, c := range cases {
		sp, text := SplitSpeaker(c.in, c.def)
		if sp != c.wantSpeaker || text != c.wantText {
			t.Errorf("SplitSpeaker(%q) = (%q,%q)，想要 (%q,%q)", c.in, sp, text, c.wantSpeaker, c.wantText)
		}
	}
}

func TestTagSpeakerRoundTrip(t *testing.T) {
	// 统一前缀：默认说话人也带标记（模型只见同一种格式）
	if got := TagSpeaker("user", "你好"); got != "[user] 你好" {
		t.Fatalf("TagSpeaker(user) = %q", got)
	}
	if got := TagSpeaker("nacl", "早"); got != "[nacl] 早" {
		t.Fatalf("TagSpeaker(nacl) = %q", got)
	}
	// 解析与标注互为往返
	sp, text := SplitSpeaker("[nacl] 早", "user")
	if sp != "nacl" || text != "早" {
		t.Fatalf("SplitSpeaker = %q,%q", sp, text)
	}
	if TagSpeaker(sp, text) != "[nacl] 早" {
		t.Fatal("往返不一致")
	}
}
