package ariahost

import (
	"strings"
	"testing"
)

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

func TestTruncStr(t *testing.T) {
	if got := TruncStr("短句", 10); got != "短句" {
		t.Errorf("短句不应截断：%q", got)
	}
	// 按 rune 截断：5 个汉字截到 3 个 + 省略号
	long := strings.Repeat("汉", 5)
	if got := TruncStr(long, 3); got != "汉汉汉…" {
		t.Errorf("rune 截断不符：%q", got)
	}
}
