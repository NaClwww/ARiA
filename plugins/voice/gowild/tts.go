package gowild

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
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
	// SilentBody=true 即 speak 工具模式：正文增量不再自动上嘴（漏调
	// speak 的正文打「未出声」提示便于调人设），声音只来自 Speak 工具。
	SilentBody bool
	// PlaybackGate 只覆盖首个 PCM 送播到设备排空，不覆盖合成或预取。
	PlaybackGate Gate
	// EchoMute 是每次放音结束后闸门续持的回声静默窗：backend 的 VAD 在
	// 播放结束后还可能把尾音成轮（final 迟到恰好落在闸门放行之后），
	// 静默窗把这批迟到 final 挡在门外。0 = 默认 900ms；负数 = 关闭。
	EchoMute time.Duration
}

// DefaultEchoMute 是回声静默窗的默认时长。
const DefaultEchoMute = 900 * time.Millisecond

// VoiceRenderInstruction 追加到 system prompt（默认渲染模式：正文自动
// 朗读，声音不经过工具）：模型要知道自己在「说话」而不是「写文章」，
// 以及哪些内容会出声（只有正文）、哪些不会（思考/工具轨迹）。
const VoiceRenderInstruction = `

【语音输出】你的回复正文会被实时朗读给用户听——用户在听你说话，不是在阅读，所以正文要口语化：
- 像平时聊天那样说：短句、大白话，一句一口气，不说长难句；
- 不用 markdown（列表、标题、加粗、代码块）、emoji、括号注释——朗读出来要么奇怪要么没意义；
- 数字、单位、符号换成口语：「大概三分钟」不要「180s」，「九成」不要「90%」；
- 思考过程和工具调用不会被朗读，用户听到的只有你的正文。`

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

	// speak 工具的 FIFO 播报队列（与 Run goroutine 的 current 分开：
	// MessageStart/AgentStart 不掐正文播放的那套规则照旧，队列有自己的
	// 生命周期——插话与新轮作废整条队列，见 stopSpeak）。
	speakMu   sync.Mutex
	speakJobs []*speakJob
}

// speakJob 是 FIFO 队列的一段：文本按序播出。队首在直放位（音频直接上
// 设备），队二在预取位——会话提前开跑、音频压在 holdSink 里，上一段
// 放完的瞬间直灌设备（接缝无空窗）；更后面的段先存文本等管线位腾出。
type speakJob struct {
	text      string
	pb        *ttsPlayback
	hold      *holdSink     // 预取位的缓冲闸（nil = 直放位）
	started   chan struct{} // 会话已开跑（直放/预取都算）：阻塞 speak 等它
	startOnce sync.Once
	err       error // 会话没能开起来的同步错误
	killed    bool  // stopSpeak 作废标记：等待者据此报打断
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
	if cfg.EchoMute == 0 {
		t.cfg.EchoMute = DefaultEchoMute
	}
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
	if cfg.PlaybackGate != nil {
		factory := t.newSink
		t.newSink = func(ctx context.Context, rate, channels int) audioSink {
			return &playbackGateSink{audioSink: factory(ctx, rate, channels), gate: cfg.PlaybackGate}
		}
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
			if t.cfg.SilentBody {
				continue // speak 工具模式：正文不出声，声音只来自 speak 工具
			}
			if t.current == nil {
				t.start()
			}
			if t.current != nil {
				t.current.feed(data.TextDelta)
			}
		case loop.MessageEndData:
			if data.Message.Role != message.RoleAssistant {
				continue
			}
			if t.cfg.SilentBody {
				if data.Message.Text() != "" {
					// 人设要求出声全走 speak；这条正文用户一个字都听不到，
					// 提示出来便于调人设（漏 speak 的正文等于白说）。
					t.log.Warn("tts: 正文未出声（speak 工具模式下正文不朗读）",
						"text", truncStr(data.Message.Text(), 40))
				}
				continue
			}
			t.finishInput() // 输入收口，音频自然排空
		case loop.AgentStartData:
			t.stop()
			t.stopSpeak()    // 新一轮 = 用户又说了话：旧队列作废，不让它压过新回答
			t.gate.Acquire() // 生成语言期间不接受新输入
		case loop.UserMessageInjectedData:
			// 插话拿到话筒：还在排队/在放的播报全部作废（FIFO 让位于用户）。
			t.stopSpeak()
		case loop.AgentEndData:
			t.gate.Release() // 轮次结束（播放各自持有自己的份额）
		}
	}
	t.stop()
	t.stopSpeak() // 会话终结（事件流关闭）：不留挂着的播报
}

func (t *TTS) start() {
	t.stop()
	p, err := startPlayback(t.cfg.Base, t.hc, t.log, t.newSink, t.gate, t.cfg.EchoMute, nil)
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

// truncStr 按 rune 截断并加省略号（日志/提示用）。
func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- speak 工具的 FIFO 播报队列 ----------
//
// 分段播报的空窗不在模型侧，在管线侧：老实现是 radio 单飞（新段掐旧段）
// + 阻塞到放完，每一段都付一次「等下一句首音频」的空窗。FIFO 之后段按
// 序播、队二提前合成（holdSink 压音频），接缝直灌设备；阻塞语义收窄为
// 「等到进入播放/预取位」——管线背压深度 1，模型自然形成流水线。

// EnqueueSpeak 把一段话排进队尾（非阻塞提交）。
func (t *TTS) EnqueueSpeak(text string) {
	t.enqueueSpeak(text)
}

func (t *TTS) enqueueSpeak(text string) *speakJob {
	t.speakMu.Lock()
	defer t.speakMu.Unlock()
	j := &speakJob{text: text, started: make(chan struct{})}
	t.speakJobs = append(t.speakJobs, j)
	t.pumpLocked()
	return j
}

// pumpLocked 保证管线满载：队首在直放位、队二在预取位（狠渲染——永远
// 提前合成一段）。会话开不起来的段（URL 级同步错误）丢弃并解阻塞等待者。
func (t *TTS) pumpLocked() {
	for len(t.speakJobs) > 0 && t.speakJobs[0].pb == nil {
		if t.startJobLocked(t.speakJobs[0], false) {
			break
		}
		t.dropJobLocked(0)
	}
	if len(t.speakJobs) > 1 && t.speakJobs[1].pb == nil {
		if !t.startJobLocked(t.speakJobs[1], true) {
			t.dropJobLocked(1)
		}
	}
}

func (t *TTS) dropJobLocked(i int) {
	j := t.speakJobs[i]
	t.speakJobs = append(t.speakJobs[:i], t.speakJobs[i+1:]...)
	j.startOnce.Do(func() { close(j.started) })
}

func (t *TTS) startJobLocked(j *speakJob, prefetch bool) bool {
	factory := t.newSink
	if prefetch {
		j.hold = newHoldSink()
		base := factory
		factory = func(ctx context.Context, rate, channels int) audioSink {
			j.hold.setFactory(func() audioSink { return base(ctx, rate, channels) })
			return j.hold
		}
	}
	pb, err := startPlayback(t.cfg.Base, t.hc, t.log, factory, t.gate, t.cfg.EchoMute,
		func() bool { return t.advanceSpeak(j) })
	if err != nil {
		j.err = err
		t.log.Error("speak: 会话启动失败（本段静音）", "err", err)
		return false
	}
	j.pb = pb
	pb.feed(j.text)
	pb.finishInput()
	j.startOnce.Do(func() { close(j.started) })
	return true
}

// advanceSpeak 是本段播放 goroutine 的收尾钩子（放完或死亡都调，恰好一
// 次）：出队、放行预取段、补预取下一段。返回本段之后是否还需要回声
// 静默窗——后面还有段就不需要：接缝处声音不断，VAD 无从误成轮。
func (t *TTS) advanceSpeak(j *speakJob) bool {
	t.speakMu.Lock()
	defer t.speakMu.Unlock()
	if len(t.speakJobs) == 0 || t.speakJobs[0] != j {
		return true // 已被 stopSpeak 清队（迟到的收尾）：不推进
	}
	t.speakJobs = t.speakJobs[1:]
	if len(t.speakJobs) == 0 {
		return true // 本段是结尾：静默窗照常
	}
	if next := t.speakJobs[0]; next.hold != nil {
		next.hold.release() // 预取就位：缓冲直灌设备，接缝无空窗
	}
	t.pumpLocked() // 没赶上预取的新队首直接开直放；队二补预取
	return false
}

// Speak 把一段话排进 FIFO 队列，等到它进入播放/预取位（合成与放音重叠，
// 接缝无缝——不等放完，那是队列自己的事）。ctx 取消（打断/转向）作废
// 整条队列：插话夺走话筒，剩下的话不必再说。
func (t *TTS) Speak(ctx context.Context, text string) error {
	j := t.enqueueSpeak(text)
	select {
	case <-j.started:
		switch {
		case j.killed:
			return ErrInterrupted
		case ctx.Err() != nil:
			return ErrInterrupted
		default:
			return j.err
		}
	case <-ctx.Done():
		t.stopSpeak()
		return ErrInterrupted
	}
}

// stopSpeak 作废整条 speak 队列（插话/新轮/会话终结收口，闸门不留悬账）。
func (t *TTS) stopSpeak() {
	t.speakMu.Lock()
	jobs := t.speakJobs
	t.speakJobs = nil
	for _, j := range jobs {
		j.killed = true
	}
	t.speakMu.Unlock()
	for _, j := range jobs {
		if j.pb != nil {
			j.pb.stop() // sink 已建时内含 holdSink.stop
		}
		if j.hold != nil {
			j.hold.stop() // 流头未到（sink 未建）时兜底；幂等
		}
		j.startOnce.Do(func() { close(j.started) })
	}
}
