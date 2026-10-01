package ariahost

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"aria/pkg/message"
)

// StdinPlug 是终端输入插头：一行 = 一句已说完的话（06 §2 插头契约，
// 「说完判定」= 按下回车）。[名字] 前缀切换说话人；/quit 退出。
// eofQuits：无 ASR 插头时 EOF 即收工；语音形态要常驻（提示退出方式）。
func StdinPlug(r io.Reader, deliver func(text, speaker string), defUser string, eofQuits bool, quit chan<- struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if text == "/quit" || text == "/exit" {
			close(quit)
			return
		}
		speaker, utterance := message.SplitSpeaker(text, defUser)
		if utterance == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "%s> %s\n", speaker, utterance)
		deliver(utterance, speaker)
	}
	if eofQuits {
		close(quit)
		return
	}
	fmt.Fprintln(os.Stderr, "（stdin EOF：ASR 模式常驻；退出用双击 Ctrl+C）")
}
