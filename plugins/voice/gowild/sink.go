package gowild

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
)

// audioSink 是 TTS 音频的去处：每条 assistant 消息一个 sink 实例，按流首
// 头解析出的采样率/声道配置。write 的阻塞即播放背压（paplay 管道满 /
// 设备端队列满），反压整条链。waitDrain 等实际放完（闸门据此放行输入）。
type audioSink interface {
	begin() error
	write(pcm []byte) error
	end() error // 输入收口：排空自然收尾
	stop()      // 立即掐断（幂等，可与 write 并发调用）
	waitDrain() // 阻塞到声音真正放完/会话已死
}

// paplaySink 从本机声卡出声（开发/无设备场景）。采样率/声道来自流首
// WAV 头（自适应），PCM16 固定。
type paplaySink struct {
	rate     int
	channels int
	stdin    io.WriteCloser
	cmd      *exec.Cmd
}

func (s *paplaySink) begin() error {
	if _, err := exec.LookPath("paplay"); err != nil {
		return fmt.Errorf("找不到 paplay（PipeWire/Pulse）：%w", err)
	}
	cmd := exec.Command("paplay", "--raw", "--format=s16le",
		"--rate="+strconv.Itoa(s.rate),
		"--channels="+strconv.Itoa(s.channels))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.stdin = cmd, stdin
	return nil
}

func (s *paplaySink) write(pcm []byte) error {
	_, err := s.stdin.Write(pcm)
	return err
}

func (s *paplaySink) end() error {
	return s.stdin.Close() // 进程退出交给 waitDrain 的 Wait
}

func (s *paplaySink) stop() {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// waitDrain 等播放进程自然退出（stdin 已关，放完即退）；被 stop 杀掉时
// Wait 立即返回。
func (s *paplaySink) waitDrain() {
	if s.cmd != nil {
		_ = s.cmd.Wait()
	}
}
