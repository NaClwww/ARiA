package gowild

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
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

// holdSink 是 speak FIFO 队列预取位的缓冲闸：下一段的 TTS 会话提前开跑，
// 音频先攒在内存；上一段放完的瞬间 release()——此刻才建真 sink、整块
// 灌入、之后直通。段与段之间不再有「等下一句首音频」的合成空窗。
//
// 并发：release/stop 可能与播放 goroutine 的 write/end/waitDrain 交错，
// 全部经 mu 串行；waitDrain 用 readyCh 等待，绝不持锁阻塞。
type holdSink struct {
	factory func() audioSink // 流头解析后由 setFactory 注入；真 sink 延迟到 release 才建

	mu       sync.Mutex
	released bool
	stopped  bool
	ended    bool // end() 先于 release 到达：建好 inner 后补投
	buf      bytes.Buffer
	inner    audioSink
	// pending 是已 begin、正在补写缓冲的真 sink：补写期间 inner 保持 nil，write 继续写入 buf、
	// end 只记入 ended，缓冲清空后在锁内切换为 inner，保证写入顺序与 end 位置；stop 同时作用于 pending。
	pending audioSink

	readyCh   chan struct{} // inner 建好且缓冲灌完，或已死：waitDrain 的等待点
	readyOnce sync.Once
	startOnce sync.Once
}

var errHoldStopped = errors.New("hold sink: stopped")

func newHoldSink() *holdSink {
	return &holdSink{readyCh: make(chan struct{})}
}

func (h *holdSink) markReady() { h.readyOnce.Do(func() { close(h.readyCh) }) }

// setFactory 由播放 goroutine 在流头解析后调用（startPlayback 的 newSink
// 回调里）。若 release 已先到（上一段恰好在这瞬间放完），立即补建。
func (h *holdSink) setFactory(f func() audioSink) {
	h.mu.Lock()
	h.factory = f
	need := h.released && !h.stopped
	h.mu.Unlock()
	if need {
		h.startInner()
	}
}

func (h *holdSink) begin() error { return nil } // 真 begin 延迟到 release

func (h *holdSink) write(pcm []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.stopped:
		return errHoldStopped
	case h.inner == nil: // 未 release 或真 sink 未建：继续攒
		_, _ = h.buf.Write(pcm)
		return nil
	default:
		return h.inner.write(pcm)
	}
}

func (h *holdSink) end() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.stopped:
		return nil
	case h.inner != nil:
		return h.inner.end()
	default:
		h.ended = true
		return nil
	}
}

// release 放行：建真 sink、begin、整块灌缓冲。幂等；stop 之后是 no-op。
func (h *holdSink) release() {
	h.mu.Lock()
	if h.released || h.stopped {
		h.mu.Unlock()
		return
	}
	h.released = true
	h.mu.Unlock()
	h.startInner()
}

// stop 丢弃缓冲；已建的 inner 一并掐断。幂等，可与 write/begin 并发。
func (h *holdSink) stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		h.markReady()
		return
	}
	h.stopped = true
	inner := h.inner
	if inner == nil {
		inner = h.pending
	}
	h.buf = bytes.Buffer{}
	h.mu.Unlock()
	if inner != nil {
		inner.stop()
	}
	h.markReady()
}

// startInner 建 inner 并灌缓冲；setFactory 与 release 都可能触发，只跑一次
// （factory 未到时不消耗 once，留给 setFactory 重试）。
func (h *holdSink) startInner() {
	h.mu.Lock()
	f := h.factory
	h.mu.Unlock()
	if f == nil {
		return
	}
	h.startOnce.Do(func() {
		defer h.markReady()
		inner := f()
		if err := inner.begin(); err != nil {
			h.mu.Lock()
			h.stopped = true
			h.mu.Unlock()
			return
		}
		h.mu.Lock()
		if h.stopped { // stop 与 begin 并发：丢弃刚建好的
			h.mu.Unlock()
			inner.stop()
			return
		}
		h.pending = inner
		h.mu.Unlock()
		// 补写循环：每次在锁内取出缓冲的全部字节、锁外写入；补写期间新到的 PCM 追加到 buf 尾部，
		// 由下一次循环按序写出。缓冲为空时在同一临界区内切换 inner，此后 write/end 直通。
		for {
			h.mu.Lock()
			if h.stopped { // stop 已经对 pending 执行 stop，此处不重复执行
				h.pending = nil
				h.mu.Unlock()
				return
			}
			if h.buf.Len() == 0 {
				h.pending, h.inner = nil, inner
				ended := h.ended
				h.mu.Unlock()
				if ended {
					_ = inner.end()
				}
				return
			}
			chunk := h.buf.Bytes() // 取走底层数组的所有权，buf 换为新实例，不复制
			h.buf = bytes.Buffer{}
			h.mu.Unlock()
			if err := inner.write(chunk); err != nil {
				h.stop() // 写入失败：本段止播；stop 经 pending 停止 inner 并释放其播放份额
				return
			}
		}
	})
}

// waitDrain 等到 release 建好 inner 且缓冲灌完（或已死），再等 inner 真正
// 放完。未 release 的预取段会在此挂住——放行权在队列手里。
func (h *holdSink) waitDrain() {
	<-h.readyCh
	h.mu.Lock()
	inner := h.inner
	h.mu.Unlock()
	if inner != nil {
		inner.waitDrain()
	}
}
