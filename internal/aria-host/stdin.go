package ariahost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"aria/pkg/message"
)

// StdinPlug 是终端输入插头：一行 = 一句已说完的话（06 §2 插头契约，
// 「说完判定」= 按下回车）。[名字] 前缀切换说话人；/quit 退出。
// eofQuits：无 ASR 插头时 EOF 即收工；语音形态要常驻（提示退出方式）。
// quit 由宿主提供且须幂等（信号处理与 /quit 都可能触发退出）。
// ctx 取消时立即返回（满足 app.Service 的契约）：r 的阻塞读不可取消，读取 goroutine
// 保留到读取结束或进程退出，取消后读到的行不再投递。
func StdinPlug(ctx context.Context, r io.Reader, deliver func(text, speaker string), defUser string, eofQuits bool, quit func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLines(ctx, r, deliver, defUser, eofQuits, quit)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// readLines 是 StdinPlug 的读取循环，逐行解析说话人前缀并投递。
func readLines(ctx context.Context, r io.Reader, deliver func(text, speaker string), defUser string, eofQuits bool, quit func()) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if text == "/quit" || text == "/exit" {
			quit()
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
		quit()
		return
	}
	fmt.Fprintln(os.Stderr, "（stdin EOF：ASR 模式常驻；退出用双击 Ctrl+C）")
}
