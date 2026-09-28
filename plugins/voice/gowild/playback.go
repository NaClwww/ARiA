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
}

func startPlayback(base string, hc *http.Client, log *slog.Logger,
	newSink func(ctx context.Context, rate, channels int) audioSink,
	gate Gate) (*ttsPlayback, error) {
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

	p := &ttsPlayback{cancel: cancel, pw: pw}
	go func() {
		// 播放份额从「TTS 请求发出」就持有（而非首个音频到达后）：
		// 正文生成完到音频开始之间可能隔着整个合成排队（RVC 忙时以十秒
		// 计），这段空窗里轮次份额已还、闸门清空——灯提前掉回待机、
		// 半双工放行，等音频来了再跳回去。份额现在覆盖 请求→排空 全程。
		gate.Acquire()
		defer gate.Release()
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("tts: 请求失败", "err", err)
			}
			_ = pw.CloseWithError(err) // feed 侧随即 fin
			return
		}
		defer resp.Body.Close()

		// 流首 WAV 头：解析真实采样率/声道（模型原生率，如 40k），
		// 非法头响亮失败——不猜默认值。
		header := make([]byte, wavHeaderLen)
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			log.Error("tts: 流头读取失败", "err", err)
			return
		}
		rate, channels, err := parseWavHeader(header)
		if err != nil {
			log.Error("tts: 非法 WAV 流头", "err", err)
			return
		}
		sink := newSink(ctx, rate, channels)
		if err := sink.begin(); err != nil {
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
				break // io.EOF：音频全量送达
			}
		}
		sink.end()
		sink.waitDrain() // 等设备放完（end 之后设备侧还在排空）
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
