package ariahost

import (
	"fmt"
	"io"
	"strings"

	"aria/core/loop"
	"aria/pkg/message"
)

// ConsumeTerminal 是事件流的终端渲染：stdout 收回答增量，stderr 收工具
// 轨迹（开始/被拒/失败——成功的工具不打，保持安静）。阻塞到通道关闭；
// 宿主自行起 goroutine 并跟踪完成。
func ConsumeTerminal(ch <-chan loop.Event, out, errw io.Writer) {
	for ev := range ch {
		switch d := ev.Data.(type) {
		case loop.MessageUpdateData:
			fmt.Fprint(out, d.TextDelta)
		case loop.MessageEndData:
			if d.Message.Role == message.RoleAssistant && d.Message.Text() != "" {
				fmt.Fprintln(out)
			}
		case loop.ToolExecStartData:
			fmt.Fprintf(errw, "  · 工具 %s(%s)\n", d.Call.Name, TruncStr(string(d.Call.Args), 60))
		case loop.ToolExecEndData:
			if d.Denied {
				fmt.Fprintf(errw, "  · 已拒绝 %s\n", d.Call.Name)
			} else if d.Result.IsError {
				fmt.Fprintf(errw, "  · 失败 %s：%s\n", d.Call.Name, TruncStr(FirstLine(d.Result), 80))
			}
		}
	}
}

// FirstLine 取工具结果的首行（截断到 80 字），失败轨迹单行足够。
func FirstLine(r message.ToolResult) string {
	text := strings.TrimSpace(r.ToMessage().Text())
	if i := strings.IndexByte(text, '\n'); i > 0 {
		text = text[:i]
	}
	return TruncStr(text, 80)
}

// TruncStr 按 rune 截断加省略号（终端轨迹不刷屏）。
func TruncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
