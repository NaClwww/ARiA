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
		{"[ 这是一个超长的名字肯定不是说话人] 嗯", "user", "user", "[ 这是一个超长的名字肯定不是说话人] 嗯"}, // 方括号内 33 列 > 22
		{"[未闭合 你好", "user", "user", "[未闭合 你好"},
		{"[王小明的同桌张三] 早上好", "user", "王小明的同桌张三", "早上好"},                         // 8 个汉字 = 16 列
		{"[一二三四五六七八九十甲] 嗯", "user", "一二三四五六七八九十甲", "嗯"},                       // 11 个汉字 = 22 列，恰好上限
		{"[一二三四五六七八九十甲乙] 嗯", "user", "user", "[一二三四五六七八九十甲乙] 嗯"},              // 12 个汉字 = 24 列 > 22
		{"[speaker_0123456789abcd] 嗯", "user", "speaker_0123456789abcd", "嗯"}, // 22 个 ASCII 字符
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

// TagSpeaker 对含 ']' 与超宽的名字规整后，SplitSpeaker 仍能还原出同一标签。
func TestTagSpeakerSanitizesForRoundTrip(t *testing.T) {
	cases := []struct{ name, wantLabel string }{
		{"王小明的同桌张三", "王小明的同桌张三"},
		{"a]b", "a］b"},
		{"  空白  ", "空白"},
		{"0123456789abcdef0123456789abcdef", "0123456789abcdef012345"}, // 32 个 ASCII 截到 22 列
		{"一二三四五六七八九十甲乙丙", "一二三四五六七八九十甲"},                               // 13 个汉字截到 11 个
	}
	for _, c := range cases {
		tagged := TagSpeaker(c.name, "内容")
		sp, text := SplitSpeaker(tagged, "user")
		if sp != c.wantLabel || text != "内容" {
			t.Errorf("TagSpeaker(%q) = %q，SplitSpeaker 得到 (%q,%q)，想要 (%q,%q)", c.name, tagged, sp, text, c.wantLabel, "内容")
		}
	}
}
