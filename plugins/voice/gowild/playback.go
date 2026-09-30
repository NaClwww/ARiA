package gowild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wavHeaderLen 是 backend 流式响应开头的 WAV 头长度（标准 44 字节，RIFF
// 占位长度）。采样率/声道从头部自适应解析——backend 按模型原生率出流
// （如 40k 的 RVC），写死 16k 会慢放降调。
const wavHeaderLen = 44

// parseWavHeader 解析流首 WAV 头的采样率与声道（backend 契约：流首必带
// WAV 头且为真值）。bits 必须 PCM16；头不合法即报错——不猜默认值，
// 猜错了就是又一次慢放降调。
func parseWavHeader(h []byte) (rate, channels int, err error) {
	if len(h) < wavHeaderLen || string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return 0, 0, errors.New("missing RIFF/WAVE magic")
	}
	channels = int(h[22]) | int(h[23])<<8
	rate = int(h[24]) | int(h[25])<<8 | int(h[26])<<16 | int(h[27])<<24
	bits := int(h[34]) | int(h[35])<<8
	if channels < 1 || channels > 2 {
		return 0, 0, fmt.Errorf("bad channels %d", channels)
	}
	if rate < 8000 || rate > 96000 {
		return 0, 0, fmt.Errorf("bad sample rate %d", rate)
	}
	if bits != 16 {
		return 0, 0, fmt.Errorf("unsupported bits %d (want PCM16)", bits)
	}
	return rate, channels, nil
}

// ttsPlayback 是一条 assistant 消息的 TTS 会话：文本增量写入请求体管道；
// 音频响应先解析流首 WAV 头（自适应采样率/声道）再建 sink、逐块送播。
type ttsPlayback struct {
	cancel context.CancelFunc
	pw     *io.PipeWriter // 请求体：文本增量
	mu     sync.Mutex
	sink   audioSink // 头解析后创建；stop 可能先到
	fin    bool      // 输入已收口（或请求已死）：后续增量丢弃
	killed bool      // stop() 已到：整条掐断。与 fin 分开——收口是正常生命
	// 周期（音频还要继续放完），死亡才要掐；共用一个标志会让「先收口、
	// 后建好 sink」（合成排队慢时必现）被误判成掐断，整条静音。

	// done 在播放 goroutine 结束后关闭；err 是失败原因（写 err 与
	// close done 都在 goroutine 内，等待方经 done 读到的即定值）——
	// speak 工具阻塞等的就是 done。
	done chan struct{}
	err  error

	// chain 是 FIFO 队列的收尾钩子（nil = 无队列）：播放 goroutine 退出
	// 前恰好调用一次，成功路径在 waitDrain 后（返回值决定是否还要回声
	// 静默窗——下一段紧跟着播就没有静默可言），失败路径经 defer 兜底
	// （死段也要推进队列，否则 FIFO 卡死）。仅播放 goroutine 触碰。
	chain   func() bool
	chained bool
}

func startPlayback(base string, hc *http.Client, log *slog.Logger,
	newSink func(ctx context.Context, rate, channels int) audioSink,
	gate Gate, echoMute time.Duration,
	chain func() bool) (*ttsPlayback, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(base, "/")+"/tts/stream_input", pr)
	if err != nil {
		cancel()
		pw.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	p := &ttsPlayback{cancel: cancel, pw: pw, done: make(chan struct{}), chain: chain}
	go func() {
		// 注册在最前 = 最后执行：gate.Release() 先走、done 后关——等待方
		// 看到 done 时播放份额已还。
		defer close(p.done)
		// 播放份额从「TTS 请求发出」就持有（而非首个音频到达后）：
		// 正文生成完到音频开始之间可能隔着整个合成排队（RVC 忙时以十秒
		// 计），这段空窗里轮次份额已还、闸门清空——灯提前掉回待机、
		// 半双工放行，等音频来了再跳回去。份额现在覆盖 请求→排空 全程。
		gate.Acquire()
		defer gate.Release()
		if chain != nil {
			// 失败路径的兜底推进（成功路径在 waitDrain 后已内联调用）：
			// 注册在 Release 之后 = 先于它执行，队列不等闸门归还。
			defer func() {
				if !p.chained {
					p.chained = true
					chain()
				}
			}()
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("tts: 请求失败", "err", err)
			}
			p.err = err
			_ = pw.CloseWithError(err) // feed 侧随即 fin
			return
		}
		defer resp.Body.Close()

		// 流首 WAV 头：解析真实采样率/声道（模型原生率，如 40k），
		// 非法头响亮失败——不猜默认值。
		header := make([]byte, wavHeaderLen)
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			p.err = fmt.Errorf("流头读取失败: %w", err)
			log.Error("tts: 流头读取失败", "err", err)
			return
		}
		rate, channels, err := parseWavHeader(header)
		if err != nil {
			p.err = err
			log.Error("tts: 非法 WAV 流头", "err", err)
			return
		}
		sink := newSink(ctx, rate, channels)
		if err := sink.begin(); err != nil {
			p.err = fmt.Errorf("播放启动失败: %w", err)
			log.Error("tts: 播放启动失败（本条静音，文字照常）", "err", err,
				"rate", rate, "channels", channels)
			sink.stop()
			return
		}
		p.mu.Lock()
		if p.killed { // stop() 已先到：刚建好的 sink 直接掐（收口不算）
			p.mu.Unlock()
			sink.stop()
			return
		}
		p.sink = sink
		p.mu.Unlock()

		buf := make([]byte, 32768)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if werr := sink.write(buf[:n]); werr != nil {
					break // sink 已被掐/死亡：本条止播
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					p.err = fmt.Errorf("音频流中断: %w", rerr) // EOF=正常收流，其余是截断
				}
				break
			}
		}
		sink.end()
		sink.waitDrain() // 等设备放完（end 之后设备侧还在排空）
		// FIFO 收尾钩子：段放完时推进队列（放行预取段的缓冲直灌）。
		// 返回 false = 下一段无缝紧跟，跳过回声静默窗——接缝处声音
		// 不断，VAD 无从把尾音误成轮。
		mute := true
		if chain != nil {
			p.chained = true
			mute = chain()
		}
		// 回声静默窗：闸门再续持一会儿，吞掉 VAD 对播放尾音的迟到成轮
		// ——final 若恰好在闸门放行后到达，就成了自问自答的种子。打断
		// （ctx 取消）时立即让位，不拖响应。
		if mute && echoMute > 0 {
			select {
			case <-time.After(echoMute):
			case <-ctx.Done():
			}
		}
	}()

	return p, nil
}

func (p *ttsPlayback) feed(delta string) {
	if p.fin {
		return
	}
	if _, err := p.pw.Write([]byte(delta)); err != nil {
		p.fin = true // 请求已死：本条静音，等下一条消息
	}
}

// finishInput 结束请求体（backend finish() 把缓冲切完并放完音频）。
// 播放对象留在 driver 手里：排空期间新一轮到来仍可 stop() 掐断。
func (p *ttsPlayback) finishInput() {
	p.fin = true
	_ = p.pw.Close()
}

func (p *ttsPlayback) stop() {
	p.mu.Lock()
	p.fin = true
	p.killed = true
	s := p.sink
	p.mu.Unlock()
	p.cancel() // 掐 HTTP 请求
	_ = p.pw.Close()
	if s != nil {
		s.stop()
	}
}
