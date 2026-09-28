// aria-host 是 Gowild-HE 场景的宿主薄壳（docs/06「产品入口 = 事件流订阅者 +
// Scope 制造者」的第一块真实插头）：
//
//	音箱 mic → backend :8800 /asr/events SSE（partial 驱状态灯，type=final
//	成轮，服务端已做端点检测）→ Session.Input → LLM → 事件流 → TTS 流式
//	合成（/tts/stream_input）→ 设备扬声器（launcher /api/voice/play* 三段式）
//	或本机 paplay；会话状态映射到机身 RGB 指示灯（launcher /api/light：
//	待机暗白 / 收听绿 / 思考生成蓝）；终端 stdin 为并存开发插头
//
// 尚未接入：口型同步、真·抢话（现在是半双工闸门：说话/生成期间不收新
// 输入）、人设标签剥离与表情映射。接入缝都在本壳内，引擎不动。
//
// 用法：
//
//	aria-host --fake --no-asr --no-tts   # 纯 stdin 冒烟（无网络、不花钱）
//	aria-host --fake                     # stdin + ASR + TTS（echo 冒烟）
//	aria-host                            # 按 aria.toml 连真实模型
//
// 交互：[名字] 开头切换说话人；/quit 优雅退出；Ctrl+C 打断当前回答，
// 连按两次强制退出。stdin EOF 在 ASR 模式下不退出（常驻语音形态）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/internal/assemble"
	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/plugins/persist/jsonl"
	openai "aria/plugins/provider/openai"
	"aria/runtime/agent"
	"aria/runtime/persist"
	"aria/runtime/window"
)

const defaultBackend = "http://127.0.0.1:8800"

func main() {
	var (
		fake         = flag.Bool("fake", false, "使用内置 echo provider（无网络，验证链路）")
		configPath   = flag.String("config", "aria.toml", "配置文件路径（人写基准，启动时读一次）")
		overridePath = flag.String("override", "aria.override.toml", "覆盖文件路径")
		backend      = flag.String("backend", defaultBackend, "ASR/TTS 后端地址（Gowild-HE-Backend :8800）")
		noASR        = flag.Bool("no-asr", false, "停用 ASR 插头（纯终端开发）")
		noStdin      = flag.Bool("no-stdin", false, "停用 stdin 插头（纯语音）")
		noTTS        = flag.Bool("no-tts", false, "停用 TTS 播放（只看文字）")
		noInputGate  = flag.Bool("no-input-gate", false, "关闭「说话/生成期间不接受新输入」闸门（半双工）")
		device       = flag.String("device", "", "launcher 控制面地址，TTS 从设备出声（如 http://127.0.0.1:18900）；空 = 本机 paplay")
		apiKey       = flag.String("api-key", "", "API key；空则按配置的 api_key_env 读环境变量")
		noLight      = flag.Bool("no-light", false, "停用状态灯（会话状态 → 设备 RGB 指示灯，仅 --device 模式）")
		lightColors  = flag.String("light-colors", "202020,00a000,2050ff", "状态灯颜色 idle,listening,thinking（hex，# 可选；暗白/绿/蓝）")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	mgr, err := config.Load(*configPath, *overridePath)
	if err != nil {
		log.Error("config load failed", "err", err, "hint", "仓库根有 aria.toml 样例，--config 可指定路径")
		os.Exit(2)
	}
	cfg := mgr.Effective()

	var prov provider.Provider
	if *fake {
		prov = echoProvider{}
		mgr.SetLocal("compress.strategy", "keeplast") // echo 不做真摘要，保持确定性
		cfg = mgr.Effective()
		log.Info("provider: echo（无网络冒烟）")
	} else {
		key := *apiKey
		if key == "" && cfg.Provider.APIKeyEnv != "" {
			key = os.Getenv(cfg.Provider.APIKeyEnv)
		}
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" || cfg.Provider.Model == "" {
			fmt.Fprintf(os.Stderr, "真实模式需要模型与 API key：配置 provider.model + 环境变量 %s（或用 --api-key/--fake）\n",
				orDefault(cfg.Provider.APIKeyEnv, "ARIA_API_KEY"))
			os.Exit(2)
		}
		prov = openai.New(openai.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, Model: cfg.Provider.Model})
		log.Info("provider: openai-compatible", "model", cfg.Provider.Model, "base_url", orDefault(cfg.Provider.BaseURL, "(官方默认)"))
	}

	compressor, err := assemble.Compressor(cfg, prov)
	if err != nil {
		log.Error("compress assemble failed", "err", err)
		os.Exit(2)
	}
	systemPrompt, err := resolvePersona(cfg.Persona)
	if err != nil {
		log.Error("persona load failed", "err", err)
		os.Exit(2)
	}

	var store persist.Store
	var jsonlStore *jsonl.Store
	if cfg.Record.Path != "" {
		s, err := jsonl.New(cfg.Record.Path)
		if err != nil {
			log.Error("record open failed", "err", err)
			os.Exit(1)
		}
		store, jsonlStore = s, s
		log.Info("record", "path", cfg.Record.Path)
	}

	ag, err := agent.New(agent.Config{
		Provider:     prov,
		Compressor:   compressor,
		Store:        store,
		SystemPrompt: systemPrompt,
		MaxTurns:     cfg.Limits.MaxTurns,
		ToolTimeout:  time.Duration(cfg.Limits.ToolTimeoutMS) * time.Millisecond,
		Logger:       log,
	})
	if err != nil {
		log.Error("agent new failed", "err", err)
		os.Exit(1)
	}
	sess, err := ag.NewSession(ctxx.Scope{SessionID: cfg.Session.ID, UserID: cfg.Session.DefaultUser})
	if err != nil {
		log.Error("session new failed", "err", err)
		os.Exit(1)
	}

	// 事件流订阅：独立 goroutine 持续排空（Input 阻塞期间也不能停——
	// 订阅者停滞会被总线断开，durable/渲染承诺随之失效）。
	ch, unsub := sess.Subscribe(0)
	printerDone := make(chan struct{})
	go func() {
		defer close(printerDone)
		printEvents(ch, os.Stdout, os.Stderr)
	}()

	// 半双工闸门：生成语言 + 设备放音期间不接受新输入（含自回声防护）。
	gate := &speakingGate{}

	// TTS 驱动：事件流的第二个消费者（一切服务都是事件订阅者）。
	var ttsDone <-chan struct{}
	var ttsUnsub func()
	if !*noTTS {
		ttsCh, u := sess.Subscribe(0)
		ttsUnsub = u
		d := newTTSDriver(*backend, *device, gate, log)
		done := make(chan struct{})
		ttsDone = done
		go func() {
			defer close(done)
			d.run(ttsCh)
		}()
	}

	// 状态灯：会话状态 → 设备 RGB 指示灯（摄像头旁那颗；/api/light 的
	// color 模式只驱三色通道，不碰暖白舞台环）。仅设备模式存在。
	var light *lightDriver
	if *device != "" && !*noLight {
		ld, lerr := newLightDriver(*device, *lightColors, log)
		if lerr != nil {
			log.Error("light-colors 无效", "err", lerr)
			os.Exit(2)
		}
		light = ld
		go light.run(gate)
		log.Info("light: 状态灯联动（待机暗白 / 收听绿 / 思考蓝）", "device", orDefault(*device, "(无)"))
	}

	// Ctrl+C：第一次打断当前回答（steering），第二次强制退出（与 aria-demo 一致）。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\n（打断当前回答；再按一次强制退出）")
		sess.Interrupt()
		<-sig
		os.Exit(130)
	}()

	// 输入插头（06 §2：插头交付「一句完整的话 + 谁说的」，多插头谁先来谁先进）。
	deliver := func(text, speaker string) {
		if !*noInputGate && gate.active() {
			fmt.Fprintf(os.Stderr, "·（正在说话，忽略输入）%s\n", truncStr(strings.TrimSpace(text), 40))
			return
		}
		// 轮次份额：从输入被接收持到本轮结算。半双工闸门与状态灯的
		// thinking 态共用这一路信号——它与 ttsDriver 的 AgentStart 份额、
		// 播放份额同池计数，各管各的生命周期。
		gate.acquire()
		defer gate.release()
		if err := deliverUtterance(sess, text, speaker, cfg); err != nil {
			log.Error("input failed", "err", err)
		}
	}
	var plugs []string
	quit := make(chan struct{})
	if !*noASR {
		var onPartial func()
		if light != nil {
			onPartial = light.partial
		}
		go newASRPlug(*backend, cfg.Session.DefaultUser, deliver, onPartial, log).run(context.Background())
		plugs = append(plugs, "asr")
	}
	if !*noStdin {
		// stdin EOF 是否收工取决于有没有 ASR 插头：语音形态要常驻。
		go stdinPlug(deliver, cfg.Session.DefaultUser, *noASR, quit)
		plugs = append(plugs, "stdin")
	}
	if len(plugs) == 0 {
		fmt.Fprintln(os.Stderr, "没有启用的插头（--no-asr --no-stdin）——无事可做，退出")
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "ARiA host · 插头 %v · backend %s · 模型 %s · 会话 %s · 说话人 %s\n",
		plugs, orDefault(*backend, defaultBackend), modelName(*fake, cfg.Provider.Model), cfg.Session.ID, cfg.Session.DefaultUser)
	fmt.Fprintln(os.Stderr, "输入一句话回车发送；[名字] 开头切换说话人；/quit 退出；Ctrl+C 打断。")

	<-quit
	if light != nil {
		light.settleIdle() // 灯是音箱的资产：退出前收回待机，别留在一半的状态上
	}
	unsub()
	if ttsUnsub != nil {
		ttsUnsub()
	}
	if err := sess.Close(); err != nil {
		log.Error("session close", "err", err)
	}
	<-printerDone
	if ttsDone != nil {
		<-ttsDone
	}
	if jsonlStore != nil {
		_ = jsonlStore.Close()
	}
}

// deliverUtterance 实现多插头纪律（06 §2）：在途轮次先试 Queue（轮间注入，
// 非阻塞）；空闲（ErrNoActiveRun）则用 Input 起一轮、阻塞到本轮结算。
// 先后由会话锁与到达顺序决定，与「谁先来谁先进」一致。
func deliverUtterance(sess *agent.Session, text, speaker string, cfg config.Config) error {
	msg := message.NewUser(text)
	if speaker != cfg.Session.DefaultUser {
		msg = message.NewUser("[" + speaker + "] " + text)
	}
	if err := sess.Queue(msg); errors.Is(err, agent.ErrNoActiveRun) {
		ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: speaker})
		temp := cfg.LLM.Temperature
		ctx = ctxx.WithOptions(ctx, ctxx.Options{
			Model: cfg.Provider.Model, Temperature: &temp, MaxTokens: cfg.LLM.MaxTokens,
		})
		_, err := sess.Input(ctx, msg)
		return err
	} else if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  · 轮间注入：%s\n", strings.TrimSpace(text))
	return nil
}

// ---------- stdin 插头：一行 = 一句已说完的话（开发/调试用） ----------

func stdinPlug(deliver func(text, speaker string), defUser string, eofQuits bool, quit chan<- struct{}) {
	sc := bufio.NewScanner(os.Stdin)
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
		speaker, utterance := splitSpeaker(text, defUser)
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

// splitSpeaker 解析「[名字] 内容」前缀；无前缀用默认说话人（与 aria-demo 一致）。
func splitSpeaker(line, def string) (string, string) {
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

// ---------- ASR 插头：backend /asr/events SSE，partial 驱状态灯，final 成轮 ----------

// asrPlug 订阅 backend 的 ASR 事件流。06 §2 插头契约：成轮判定由插头自理
// ——backend 的流式 ASR 已做端点检测（VAD→final），final 即「一句说完整的
// 话」，partial 不驱动轮次、只喂状态灯（「用户开口中」）。断线指数退避
// 重连（服务重启/网络抖动属常态）。
type asrPlug struct {
	base      string
	speak     string
	onText    func(text, speaker string)
	onPartial func() // 每条非空 partial 一调（灯的「收听中」信号）；可为 nil
	log       *slog.Logger
	hc        *http.Client
}

func newASRPlug(base, defUser string, onText func(text, speaker string), onPartial func(), log *slog.Logger) *asrPlug {
	return &asrPlug{
		base:      strings.TrimSuffix(base, "/"),
		speak:     defUser,
		onText:    onText,
		onPartial: onPartial,
		log:       log,
		hc: &http.Client{Transport: &http.Transport{
			// SSE 长连接不能设整体超时，只约束建连速度：连不上要快点进退避。
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
	}
}

func (p *asrPlug) run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for ctx.Err() == nil {
		connected, err := p.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second // 连上过就重置退避
		}
		p.log.Warn("asr: 事件流断开，重连中", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// consume 阻塞消费一条 SSE 连接直到断开；connected 报告是否成功建立过
// 连接（用于退避重置）。
func (p *asrPlug) consume(ctx context.Context) (connected bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/asr/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /asr/events: status %d", resp.StatusCode)
	}
	p.log.Info("asr: 已连接", "url", p.base+"/asr/events")

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// SSE 帧：`data: {json}`；`: keepalive` / 空行跳过。backend 每事件
		// 单行 JSON，不存在多行 data 聚合。
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue // 解析失败的行按噪音丢弃，不断流
		}
		if ev.Type == "partial" {
			if strings.TrimSpace(ev.Text) != "" && p.onPartial != nil {
				p.onPartial() // 成轮与否由 final 决定；partial 只说明 mic 听到了人声
			}
			continue
		}
		if ev.Type != "final" || strings.TrimSpace(ev.Text) == "" {
			continue // 只有 final 是「说完了」
		}
		fmt.Fprintf(os.Stderr, "%s(语音)> %s\n", p.speak, ev.Text)
		p.onText(ev.Text, p.speak)
	}
	if serr := sc.Err(); serr != nil {
		return true, serr
	}
	return true, errors.New("sse stream closed by server")
}

// ---------- TTS 驱动：事件流 → /tts/stream_input → 播放 sink ----------

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

// speakingGate 是「正在说话」闸门（半双工策略）：从输入被接收（deliver 的
// 轮次份额）或生成开始（ttsDriver 的 AgentStart 份额）到设备把声音放完
// （播放份额）期间不接受新输入。引用计数——各份额独立持有，全释放才放行。
// 顺带挡住自回声：播放期间 mic 收到的「设备自己的声音」不再成轮，自问自
// 答循环消失。状态灯的 thinking 态直接读 active()。
type speakingGate struct {
	mu sync.Mutex
	n  int
}

func (g *speakingGate) acquire() {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
}

func (g *speakingGate) release() {
	g.mu.Lock()
	if g.n > 0 {
		g.n--
	}
	g.mu.Unlock()
}

func (g *speakingGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n > 0
}

// ---------- 状态灯：会话状态 → 设备 /api/light（摄像头旁 RGB 指示灯） ----------

// lightState 是灯的三态词汇：待机 / 收听（用户开口中）/ 思考生成（整轮忙）。
type lightState int

const (
	lightIdle lightState = iota
	lightListening
	lightThinking
)

func (s lightState) String() string {
	switch s {
	case lightListening:
		return "listening"
	case lightThinking:
		return "thinking"
	default:
		return "idle"
	}
}

// partialWindow 判定「正在收听」的 partial 新鲜度窗口：流式 ASR 说话期间
// 数百毫秒一发 partial，1.5s 没有下一发即认为用户已停口（final 还在路上，
// 或 VAD 判成噪音根本不会有 final）。窗口必须显著小于 ASR 断线退避间隔，
// 否则断流会让灯挂在绿色上。
const partialWindow = 1500 * time.Millisecond

// lightDriver 把会话状态映射到机身 RGB 指示灯：launcher /api/light 的
// color 模式只驱 12/15/18 三色通道，不碰 15 颗暖白舞台环（那是舞台效果，
// 归 console 的灯面板管）。
//
// 求值是拉模式：每拍用「闸门激活（thinking）> partial 新鲜（listening）
// > 待机」合成目标态，与已下发的态不同才 POST。拉模式的意义在自愈——
// 状态迁移的边（AgentEnd、播放排空、Input 失败没有 AgentStart……）无需
// 逐一处理，任何一路信号丢失，最多一拍灯就回到真实状态。
type lightDriver struct {
	base   string
	colors [3][3]int // 按 lightState 索引的 rgb
	hc     *http.Client
	log    *slog.Logger

	// lastPartial 由 asrPlug 并发写、求值循环读；applied/failWant/
	// retryNotBefore confinement 在 run goroutine。
	mu          sync.Mutex
	lastPartial time.Time

	applied        lightState // 已成功下发的态；构造为 -1：首拍强制下发（零值 idle 会被当成「已下发」跳过）
	failWant       lightState // 上次失败的目标态（退避期内同目标不重试）
	retryNotBefore time.Time
}

// newLightDriver 的 colors 形如 "202020,00a000,2050ff"（idle,listening,thinking）。
func newLightDriver(base, colors string, log *slog.Logger) (*lightDriver, error) {
	c, err := parseLightColors(colors)
	if err != nil {
		return nil, err
	}
	return &lightDriver{
		base:    strings.TrimSuffix(base, "/"),
		colors:  c,
		applied: lightState(-1),
		hc: &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
		log: log,
	}, nil
}

func parseLightColors(s string) ([3][3]int, error) {
	var out [3][3]int
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return out, errors.New("需要三个逗号分隔的 hex 颜色（idle,listening,thinking），如 \"202020,00a000,2050ff\"")
	}
	for i, p := range parts {
		p = strings.TrimPrefix(strings.TrimSpace(p), "#")
		if len(p) != 6 {
			return out, fmt.Errorf("颜色 %d（%q）须为 rrggbb 六位 hex", i, p)
		}
		for j := 0; j < 3; j++ {
			v, err := strconv.ParseUint(p[j*2:j*2+2], 16, 8)
			if err != nil {
				return out, fmt.Errorf("颜色 %d（%q）解析失败：%w", i, p, err)
			}
			out[i][j] = int(v)
		}
	}
	return out, nil
}

// partial 记录一次「mic 听到了人声」。播放期间设备自己的回声也会进来——
// 无妨，闸门激活时 thinking 优先级更高；回声留下的新鲜 partial 最多让灯
// 在回答结束后多绿 1.5s，随后自然回落。
func (l *lightDriver) partial() {
	l.mu.Lock()
	l.lastPartial = time.Now()
	l.mu.Unlock()
}

// run 是求值主循环：250ms 一拍。灯是人看的指示器，不需要更快。
func (l *lightDriver) run(gate *speakingGate) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		l.evaluate(gate)
	}
}

// evaluate 求值一拍：thinking > listening > idle。只在目标态与已下发态
// 不同才下发；失败退避 5s（同目标态），设备不可达时不刷错误日志。
func (l *lightDriver) evaluate(gate *speakingGate) {
	l.mu.Lock()
	last := l.lastPartial
	l.mu.Unlock()

	want := lightIdle
	if gate.active() {
		want = lightThinking
	} else if time.Since(last) < partialWindow {
		want = lightListening
	}
	if want == l.applied {
		return
	}
	if want == l.failWant && time.Now().Before(l.retryNotBefore) {
		return
	}
	if err := l.apply(want); err != nil {
		l.log.Warn("light: 下发失败", "state", want, "err", err)
		l.failWant, l.retryNotBefore = want, time.Now().Add(5*time.Second)
		return
	}
	l.applied = want
	l.failWant, l.retryNotBefore = lightIdle, time.Time{}
	l.log.Info("light: " + want.String(), "rgb", l.colors[want])
}

// apply 下发一档颜色（同步、2s 超时；只被求值循环单线程调用）。color 模式
// 的 brightness 缺省 255 = rgb 原值直发，明暗直接编进颜色里。
func (l *lightDriver) apply(s lightState) error {
	body := fmt.Sprintf(`{"mode":"color","rgb":[%d,%d,%d]}`,
		l.colors[s][0], l.colors[s][1], l.colors[s][2])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/api/light", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /api/light: status %d", resp.StatusCode)
	}
	return nil
}

// settleIdle 退出前把灯收回待机（尽力而为；灯是音箱的资产，不该留在
// thinking 上过夜）。
func (l *lightDriver) settleIdle() {
	if err := l.apply(lightIdle); err != nil {
		l.log.Warn("light: 退出置待机失败", "err", err)
	}
}

// audioSink 是 TTS 音频的去处：每条 assistant 消息一个 sink 实例，按流首
// 头解析出的采样率/声道配置。write 的阻塞即播放背压（paplay 管道满 /
// 设备端队列满），反压整条链。waitDrain 等实际放完（闸门据此放行输入）。
type audioSink interface {
	begin() error
	write(pcm []byte) error
	end() error     // 输入收口：排空自然收尾
	stop()          // 立即掐断（幂等，可与 write 并发调用）
	waitDrain()     // 阻塞到声音真正放完/会话已死
}

// ttsDriver 是事件流的第二个消费者（06：一切服务都是事件订阅者）：assistant
// 的文本增量直接透传给 backend 的流式 TTS——其 StreamingSession.feed 内部
// 自带切句，Go 侧不缓冲，首响延迟最低。每条 assistant 消息一次 HTTP 分块
// 请求；新一轮开始或新消息开始时掐掉上一条没放完的音频尾巴（粗粒度抢话
// 的第一步，同时缓解「音箱听到自己的 TTS」的自回声）。
//
// 尚未做：人设标签（[happy] 之类）剥离——人设还没训标签约定，训好后在
// feed 前剥除并映射到 launcher 表情。
type ttsDriver struct {
	base    string
	log     *slog.Logger
	hc      *http.Client
	gate    *speakingGate
	dev     *deviceClient // 设备播放隧道（单例；本机播放时为 nil）
	newSink func(ctx context.Context, rate, channels int) audioSink
	current *ttsPlayback
}

func newTTSDriver(backend, device string, gate *speakingGate, log *slog.Logger) *ttsDriver {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		// 播放闸门份额从请求发出即持有（见 startPlayback），一个挂死的
		// TTS 请求会把闸门占死、堵住输入。响应头由 backend 生成器首个
		// yield（WAV 头）即刻送出，只约束到头的时间不影响流式体。
		ResponseHeaderTimeout: 15 * time.Second,
	}}
	d := &ttsDriver{base: strings.TrimSuffix(backend, "/"), log: log, hc: hc, gate: gate}
	if device != "" {
		d.dev = newDeviceClient(device, log)
		dev := d.dev
		d.newSink = func(ctx context.Context, rate, channels int) audioSink {
			return &deviceSink{cl: dev, ctx: ctx, rate: rate, channels: channels}
		}
		log.Info("tts: 设备端播放", "device", dev.base)
	} else {
		d.newSink = func(_ context.Context, rate, channels int) audioSink {
			return &paplaySink{rate: rate, channels: channels}
		}
		log.Info("tts: 本机播放（paplay）")
	}
	return d
}

// run 是单消费者事件循环：全部状态 confinement 在本 goroutine，无锁。
func (d *ttsDriver) run(ch <-chan loop.Event) {
	for ev := range ch {
		switch data := ev.Data.(type) {
		case loop.MessageStartData:
			if data.Role == message.RoleAssistant {
				d.stop() // 新消息开新会话：掐上一条尾巴
			}
		case loop.MessageUpdateData:
			if data.TextDelta == "" {
				continue // ThoughtDelta 不上嘴：只念正文
			}
			if d.current == nil {
				d.start()
			}
			if d.current != nil {
				d.current.feed(data.TextDelta)
			}
		case loop.MessageEndData:
			if data.Message.Role == message.RoleAssistant {
				d.finishInput() // 输入收口，音频自然排空
			}
		case loop.AgentStartData:
			d.stop() // 新一轮开始：上一轮没放完的不放
			d.gate.acquire() // 生成语言期间不接受新输入
		case loop.AgentEndData:
			d.gate.release() // 轮次结束（播放各自持有自己的份额）
		}
	}
	d.stop()
}

func (d *ttsDriver) start() {
	d.stop()
	p, err := startPlayback(d.base, d.hc, d.log, d.newSink, d.gate)
	if err != nil {
		d.log.Error("tts: 启动失败（本条静音，文字照常）", "err", err)
		return
	}
	d.current = p
}

func (d *ttsDriver) finishInput() {
	if d.current != nil {
		d.current.finishInput()
	}
}

func (d *ttsDriver) stop() {
	if d.current != nil {
		d.current.stop()
		d.current = nil
	}
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
	gate *speakingGate) (*ttsPlayback, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/tts/stream_input", pr)
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
		gate.acquire()
		defer gate.release()
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

// ---------- 播放 sink：设备端（launcher /api/voice/play*）与本机 paplay ----------

// deviceClient 把对设备播放端点的所有请求串成单连接 FIFO：一条隧道连接
// （MaxConnsPerHost=1）+ 单 worker 逐个执行，设备侧到达序 = 发出序。这是
// 「对话太快 TTS 被打烂」的根治——此前新旧会话的 POST 各自并发，旧轮次
// 没死透的 chunk 会混进新会话、旧 end 会掐断新会话的输入。
type deviceClient struct {
	base string
	log  *slog.Logger
	ops  chan deviceOp
	hc   *http.Client
}

// deviceOp 是隧道里的一次请求；ctx 取消即放弃执行（本条已死）。
// done 为 nil 表示 fire-and-forget（stop 用，不阻塞事件循环）。
type deviceOp struct {
	ctx    context.Context
	method string
	path   string
	body   []byte
	ct     string
	done   chan opResult
}

type opResult struct {
	body []byte
	err  error
}

func newDeviceClient(base string, log *slog.Logger) *deviceClient {
	c := &deviceClient{
		base: strings.TrimSuffix(base, "/"),
		log:  log,
		ops:  make(chan deviceOp, 64),
		hc: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxConnsPerHost:       1, // 单连接：FIFO 发出序才能等于到达序
			MaxIdleConnsPerHost:   1,
			ResponseHeaderTimeout: 10 * time.Second,
		}},
	}
	go c.worker()
	return c
}

func (c *deviceClient) worker() {
	for op := range c.ops {
		if op.ctx != nil && op.ctx.Err() != nil {
			if op.done != nil {
				op.done <- opResult{err: op.ctx.Err()}
			}
			continue
		}
		body, err := c.do(op)
		if op.done != nil {
			op.done <- opResult{body: body, err: err}
		}
	}
}

func (c *deviceClient) do(op deviceOp) ([]byte, error) {
	ctx := op.ctx
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}
	method := op.method
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if len(op.body) > 0 {
		reader = bytes.NewReader(op.body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+op.path, reader)
	if err != nil {
		return nil, err
	}
	if op.ct != "" {
		req.Header.Set("Content-Type", op.ct)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device %s: status %d", op.path, resp.StatusCode)
	}
	return body, nil
}

// call 同步提交（等结果与响应体）；返回的 error 供背压与失败判定。
func (c *deviceClient) call(ctx context.Context, method, path string, body []byte, ct string) ([]byte, error) {
	op := deviceOp{ctx: ctx, method: method, path: path, body: body, ct: ct, done: make(chan opResult, 1)}
	select {
	case c.ops <- op:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-op.done:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fireAndForget 异步提交（stop 用）：绝不阻塞驱动事件循环。
func (c *deviceClient) fireAndForget(path string) {
	select {
	case c.ops <- deviceOp{path: path}:
	default:
		c.log.Warn("tts: 设备请求隧道已满，丢弃", "path", path)
	}
}

// deviceSink 把 PCM 送到 launcher 的播放端点（对齐记录 #3 落地，口型暂缓）。
// 三段式（begin/chunk×n/end）+ stop 立即掐断；所有请求走 deviceClient 的
// 单连接 FIFO，顺序有保证。dead 是会话栅栏：stop() 先置死再投递 stop 操作，
// 之后本会话的任何请求（包括正卡在背压上的 chunk）一律本地丢弃——旧会话
// 永远碰不到新会话。采样率/声道来自流首 WAV 头（自适应）。
type deviceSink struct {
	cl       *deviceClient
	ctx      context.Context // 本条消息的播放 ctx（p.stop 先 cancel 再 stop）
	mu       sync.Mutex
	dead     bool
	rate     int
	channels int
}

func (d *deviceSink) begin() error {
	body := fmt.Sprintf(`{"rate":%d,"channels":%d}`, d.rate, d.channels)
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/begin", []byte(body), "application/json")
	return err
}

func (d *deviceSink) write(pcm []byte) error {
	d.mu.Lock()
	dead := d.dead
	d.mu.Unlock()
	if dead {
		return errors.New("sink stopped")
	}
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/chunk", pcm, "application/octet-stream")
	return err
}

// end 结束输入（设备端排空收尾）。会话已死则静默成功——旧的 end 决不能
// 落到新会话上把人家的输入掐断。
func (d *deviceSink) end() error {
	d.mu.Lock()
	dead := d.dead
	d.mu.Unlock()
	if dead {
		return nil
	}
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/end", nil, "")
	return err
}

// waitDrain 轮询设备直到会话排空关闭（open:false）——设备侧 EOF 后还要
// 把已入队音频放完才收口，这里等的就是那段尾巴。闸门（灯）跟着这个函数
// 走：提前放行=灯提前灭+输入提前开。查询失败容忍连续 3 次（偶发抖动不该
// 提前收口），超时/会话已死仍一律放行：闸门宁可早开也不能卡死输入。
func (d *deviceSink) waitDrain() {
	deadline := time.Now().Add(20 * time.Second)
	fails := 0
	for time.Now().Before(deadline) {
		d.mu.Lock()
		dead := d.dead
		d.mu.Unlock()
		if dead {
			return
		}
		body, err := d.cl.call(d.ctx, http.MethodGet, "/api/voice/play", nil, "")
		if err != nil {
			fails++
			if fails >= 3 {
				return // 连续失败：设备/隧道真不可用，别死等
			}
		} else {
			fails = 0
			var st struct {
				Open bool `json:"open"`
			}
			if json.Unmarshal(body, &st) != nil || !st.Open {
				return
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// stop 立即置死 + 异步投递 /play/stop。置死先于投递：置死前入队的旧 chunk
// 在 FIFO 里位于 stop 之前，落进旧会话后即被 stop 清掉；置死后的请求本地
// 丢弃，永远到不了设备。
func (d *deviceSink) stop() {
	d.mu.Lock()
	d.dead = true
	d.mu.Unlock()
	d.cl.fireAndForget("/api/voice/play/stop")
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

// ---------- 事件渲染（stdout = 回答增量，stderr = 工具轨迹） ----------

func printEvents(ch <-chan loop.Event, out, errw io.Writer) {
	for ev := range ch {
		switch d := ev.Data.(type) {
		case loop.MessageUpdateData:
			fmt.Fprint(out, d.TextDelta)
		case loop.MessageEndData:
			if d.Message.Role == message.RoleAssistant && d.Message.Text() != "" {
				fmt.Fprintln(out)
			}
		case loop.ToolExecStartData:
			fmt.Fprintf(errw, "  · 工具 %s(%s)\n", d.Call.Name, truncStr(string(d.Call.Args), 60))
		case loop.ToolExecEndData:
			if d.Denied {
				fmt.Fprintf(errw, "  · 已拒绝 %s\n", d.Call.Name)
			} else if d.Result.IsError {
				fmt.Fprintf(errw, "  · 失败 %s：%s\n", d.Call.Name, truncStr(firstLine(d.Result), 80))
			}
		}
	}
}

func firstLine(r message.ToolResult) string {
	text := strings.TrimSpace(r.ToMessage().Text())
	if i := strings.IndexByte(text, '\n'); i > 0 {
		text = text[:i]
	}
	return truncStr(text, 80)
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- 杂项 ----------

// resolvePersona 取人设：文件优先，其次内联，空则用内置默认。
func resolvePersona(p config.Persona) (string, error) {
	if p.SystemPromptFile != "" {
		b, err := os.ReadFile(p.SystemPromptFile)
		if err != nil {
			return "", fmt.Errorf("读 persona 文件 %s: %w", p.SystemPromptFile, err)
		}
		return string(b), nil
	}
	if p.SystemPrompt != "" {
		return p.SystemPrompt, nil
	}
	return "你是 Gowild-HE 音箱上的实时陪伴助手。回应简短自然，像身边的伙伴；" +
		"可以调用工具完成任务，不确定时先问一句。\n" + window.TagPolicyInstruction, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func modelName(fake bool, m string) string {
	if fake {
		return "echo"
	}
	return m
}

// ---------- --fake：内置 echo provider（无网络冒烟） ----------

// echoProvider 把最后一句用户输入复读回去（流式分片），用于不花钱地验证
// 「插头 → Input → 事件流 → 渲染」整条链路。
type echoProvider struct{}

func (echoProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	input := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == message.RoleUser {
			input = req.Messages[i].Text()
			break
		}
	}
	reply := "（echo）" + input
	runes := []rune(reply)
	step := len(runes)/4 + 1
	ch := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(ch)
		for i := 0; i < len(runes); i += step {
			end := min(i+step, len(runes)) // 切片上界必须钳制：静默越界会读出零值 rune
			ch <- provider.PartDelta{Text: string(runes[i:end])}
		}
		ch <- provider.MessageComplete{Message: message.Message{
			Role:   message.RoleAssistant,
			Blocks: []message.Block{message.TextBlock{Text: reply}},
		}}
	}()
	return ch, nil
}
