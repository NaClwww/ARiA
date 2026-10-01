package message

import (
	"strings"

	"golang.org/x/text/width"
)

// 说话人文本约定：谁在说话以「[名字] 」前缀写进用户消息正文，而不是结构
// 字段。这条约定横穿 stdin 解析、Sink 标注、人设声明（宿主随 system 下发）
// 与未来的记忆归属，所以定义收在 pkg/message 一处；结构化身份等
// ingress/名册（docs/05 E4）再论。
//
// 前缀规则：名字非空、不含 ']'，方括号内文本的显示宽度不超过 speakerMaxWidth 列
// （宽字符计 2 列，见 runeWidth），即整个「[...]」段不超过 24 列；空名、超宽、
// 未闭合都视为正文的一部分。TagSpeaker 产出的前缀均满足该规则。

// speakerMaxWidth 是方括号内文本的显示宽度上限（列）：11 个汉字或 22 个 ASCII 字符。
const speakerMaxWidth = 22

// TagSpeaker 给用户消息正文标说话人：「[名字] 内容」。所有消息同格式
// （含默认说话人）——模型只见一种格式，靠前缀分辨谁在说话。名字经
// speakerLabel 规整，保证 SplitSpeaker 可还原。
func TagSpeaker(speaker, text string) string {
	return "[" + speakerLabel(speaker) + "] " + text
}

// SplitSpeaker 解析「[名字] 内容」前缀；无合规前缀用默认说话人。
// 与 TagSpeaker 互为往返。
func SplitSpeaker(line, def string) (string, string) {
	if rest, ok := strings.CutPrefix(line, "["); ok {
		if i := strings.Index(rest, "]"); i > 0 && displayWidth(rest[:i]) <= speakerMaxWidth {
			name := strings.TrimSpace(rest[:i])
			if name != "" {
				return name, strings.TrimSpace(rest[i+1:])
			}
		}
	}
	return def, line
}

// speakerLabel 把名字规整为可经 SplitSpeaker 还原的标签：']' 替换为全角 '］'，
// 去除首尾空白，显示宽度超过 speakerMaxWidth 时按字符截断到上限以内。
func speakerLabel(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "]", "］"))
	w := 0
	for i, r := range name {
		w += runeWidth(r)
		if w > speakerMaxWidth {
			return strings.TrimSpace(name[:i])
		}
	}
	return name
}

// displayWidth 返回文本的显示宽度（列），逐字符累加 runeWidth。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 返回单个字符的显示宽度：Unicode East Asian Width 属性为 W（宽）或 F（全角）
// 的字符计 2 列（汉字、假名、谚文音节、CJK 标点、全角形式等），其余计 1 列。
func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}
