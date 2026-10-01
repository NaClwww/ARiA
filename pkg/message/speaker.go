package message

import "strings"

// 说话人文本约定：谁在说话以「[名字] 」前缀写进用户消息正文，而不是结构
// 字段。这条约定横穿 stdin 解析、Sink 标注、人设声明（宿主随 system 下发）
// 与未来的记忆归属，所以定义收在 pkg/message 一处；结构化身份等
// ingress/名册（docs/05 E4）再论。
//
// 前缀规则：名字非空、不含 ']'、整个「[...]」段 ≤24 字符；空名/超长/未
// 闭合都视为正文的一部分（宁可漏认不误认）。

// TagSpeaker 给用户消息正文标说话人：「[名字] 内容」。所有消息同格式
// （含默认说话人）——模型只见一种格式，靠前缀分辨谁在说话。
func TagSpeaker(speaker, text string) string {
	return "[" + speaker + "] " + text
}

// SplitSpeaker 解析「[名字] 内容」前缀；无合规前缀用默认说话人。
// 与 TagSpeaker 互为往返。
func SplitSpeaker(line, def string) (string, string) {
	if strings.HasPrefix(line, "[") {
		if i := strings.Index(line, "]"); i > 0 && i <= 24 {
			name, rest := strings.TrimSpace(line[1:i]), strings.TrimSpace(line[i+1:])
			if name != "" {
				return name, rest
			}
		}
	}
	return def, line
}
