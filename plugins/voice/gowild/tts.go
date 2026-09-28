package gowild

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"aria/core/loop"
	"aria/pkg/message"
)

// TTSConfig 是 TTS 事件订阅者的装配参数。
type TTSConfig struct {
	// Base 是 backend 根地址（POST <Base>/tts/stream_input）。必填。
	Base string
	// Device 是 launcher 控制面根地址（三段式播放 /api/voice/play*）；
	// 空 = 本机 paplay 出声（开发/无设备场景）。
	Device string
}

// TTS 是事件流的渲染消费者（06 §1：一切服务都是事件订阅者）：assistant
// 的文本增量直接透传给 backend 的流式 TTS——其 StreamingSession.feed 内部
// 自带切句，Go 侧不缓冲，首响延迟最低。每条 assistant 消息一次 HTTP 分块
// 请求；新一轮开始或新消息开始时掐掉上一条没放完的音频尾巴（粗粒度抢话
// 的第一步，同时缓解「音箱听到自己的 TTS」的自回声）。
//
// Run 是单消费者事件循环，宿主订阅事件流后交给它；闸门份额由本包按
// AgentStart（生成期）与播放生命周期（请求→排空）持有。
//
// 尚未做：人设标签（[happy] 之类）剥离——人设还没训标签约定，训好后在
// feed 前剥除并映射到 launcher 表情。
type TTS struct {
	cfg     TTSConfig
	log     *slog.Logger
	hc      *http.Client
	gate    Gate
	dev     *deviceClient // 设备播放隧道（单例；本机播放时为 nil）
	newSink func(ctx context.Context, rate, channels int) audioSink
	current *ttsPlayback
}

func NewTTS(cfg TTSConfig, gate Gate, log *slog.Logger) (*TTS, error) {
	if strings.TrimSuffix(cfg.Base, "/") == "" {
		return nil, errors.New("gowild.NewTTS: Base 必填（backend 根地址）")
	}
	if gate == nil {
		return nil, errors.New("gowild.NewTTS: Gate 必填（宿主的半双工闸门）")
	}
	if log == nil {
		log = slog.Default()
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		// 播放闸门份额从请求发出即持有（见 startPlayback），一个挂死的
		// TTS 请求会把闸门占死、堵住输入。响应头由 backend 生成器首个
		// yield（WAV 头）即刻送出，只约束到头的时间不影响流式体。
		ResponseHeaderTimeout: 15 * time.Second,
	}}
	t := &TTS{cfg: cfg, log: log, hc: hc, gate: gate}
	if cfg.Device != "" {
		t.dev = newDeviceClient(cfg.Device, log)
		dev := t.dev
		t.newSink = func(ctx context.Context, rate, channels int) audioSink {
			return &deviceSink{cl: dev, ctx: ctx, rate: rate, channels: channels}
		}
		log.Info("tts: 设备端播放", "device", dev.base)
	} else {
		t.newSink = func(_ context.Context, rate, channels int) audioSink {
			return &paplaySink{rate: rate, channels: channels}
		}
		log.Info("tts: 本机播放（paplay）")
	}
	return t, nil
}

// Run 是单消费者事件循环：全部状态 confinement 在本 goroutine，无锁。
func (t *TTS) Run(ch <-chan loop.Event) {
	for ev := range ch {
		switch data := ev.Data.(type) {
		case loop.MessageStartData:
			if data.Role == message.RoleAssistant {
				t.stop() // 新消息开新会话：掐上一条尾巴
			}
		case loop.MessageUpdateData:
			if data.TextDelta == "" {
				continue // ThoughtDelta 不上嘴：只念正文
			}
			if t.current == nil {
				t.start()
			}
			if t.current != nil {
				t.current.feed(data.TextDelta)
			}
		case loop.MessageEndData:
			if data.Message.Role == message.RoleAssistant {
				t.finishInput() // 输入收口，音频自然排空
			}
		case loop.AgentStartData:
			t.stop()         // 新一轮开始：上一轮没放完的不放
			t.gate.Acquire() // 生成语言期间不接受新输入
		case loop.AgentEndData:
			t.gate.Release() // 轮次结束（播放各自持有自己的份额）
		}
	}
	t.stop()
}

func (t *TTS) start() {
	t.stop()
	p, err := startPlayback(t.cfg.Base, t.hc, t.log, t.newSink, t.gate)
	if err != nil {
		t.log.Error("tts: 启动失败（本条静音，文字照常）", "err", err)
		return
	}
	t.current = p
}

func (t *TTS) finishInput() {
	if t.current != nil {
		t.current.finishInput()
	}
}

func (t *TTS) stop() {
	if t.current != nil {
		t.current.stop()
		t.current = nil
	}
}
