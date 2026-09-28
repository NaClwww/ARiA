// aria-host 是 Gowild-HE 场景的宿主薄壳（docs/06「产品入口 = 事件流订阅者 +
// Scope 制造者」的第一块真实插头）：
//
//	音箱 mic → backend :8800 /asr/events SSE（type=final，服务端已做端点
//	检测）→ Session.Input → LLM → 事件流 → TTS 流式合成（/tts/stream_input）
//	→ 本机 paplay 播放；终端 stdin 为并存开发插头
//
// 尚未接入：设备端播放（launcher /api/voice/play* 建成后从 paplay 切换）、
// 设备控制工具（:8900 /api/control/*，决策型 Tool）、完整抢话策略（当前仅
// 新一轮开始时掐掉上一条没放完的音频尾巴）。接入缝都在本壳内，引擎不动。
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
	"strings"
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
		apiKey       = flag.String("api-key", "", "API key；空则按配置的 api_key_env 读环境变量")
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

	// TTS 驱动：事件流的第二个消费者（一切服务都是事件订阅者）。
	var ttsDone <-chan struct{}
	var ttsUnsub func()
	if !*noTTS {
		ttsCh, u := sess.Subscribe(0)
		ttsUnsub = u
		d := newTTSDriver(*backend, log)
		done := make(chan struct{})
		ttsDone = done
		go func() {
			defer close(done)
			d.run(ttsCh)
		}()
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
		if err := deliverUtterance(sess, text, speaker, cfg); err != nil {
			log.Error("input failed", "err", err)
		}
	}
	var plugs []string
	quit := make(chan struct{})
	if !*noASR {
		go newASRPlug(*backend, cfg.Session.DefaultUser, deliver, log).run(context.Background())
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

// ---------- ASR 插头：backend /asr/events SSE，只收 type=final ----------

// asrPlug 订阅 backend 的 ASR 事件流。06 §2 插头契约：成轮判定由插头自理
// ——backend 的流式 ASR 已做端点检测（VAD→final），final 即「一句说完整的
// 话」，partial 一律丢弃。断线指数退避重连（服务重启/网络抖动属常态）。
type asrPlug struct {
	base   string
	speak  string
	onText func(text, speaker string)
	log    *slog.Logger
	hc     *http.Client
}

func newASRPlug(base, defUser string, onText func(text, speaker string), log *slog.Logger) *asrPlug {
	return &asrPlug{
		base:   strings.TrimSuffix(base, "/"),
		speak:  defUser,
		onText: onText,
		log:    log,
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
		if ev.Type != "final" || strings.TrimSpace(ev.Text) == "" {
			continue // partial 不驱动轮次：只有 final 是「说完了」
		}
		fmt.Fprintf(os.Stderr, "%s(语音)> %s\n", p.speak, ev.Text)
		p.onText(ev.Text, p.speak)
	}
	if serr := sc.Err(); serr != nil {
		return true, serr
	}
	return true, errors.New("sse stream closed by server")
}

// ---------- TTS 驱动：事件流 → /tts/stream_input → paplay ----------

// wavHeaderLen 是 backend 流式响应开头的 WAV 头长度（PCM16 单声道，RIFF
// 占位长度）；剥掉后喂 paplay --raw。
const wavHeaderLen = 44

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
	current *ttsPlayback
}

func newTTSDriver(base string, log *slog.Logger) *ttsDriver {
	return &ttsDriver{
		base: strings.TrimSuffix(base, "/"),
		log:  log,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
	}
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
		}
	}
	d.stop()
}

func (d *ttsDriver) start() {
	d.stop()
	if _, err := exec.LookPath("paplay"); err != nil {
		d.log.Error("tts: 找不到 paplay，本轮起静音（--no-tts 可关掉本告警）", "err", err)
		return
	}
	p, err := startPlayback(d.base, d.hc, d.log)
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

// ttsPlayback 是一条 assistant 消息的 TTS 会话：文本增量写入请求体管道，
// 音频从响应体剥掉 WAV 头后喂 paplay。
type ttsPlayback struct {
	ctx    context.Context
	cancel context.CancelFunc
	pw     *io.PipeWriter // 请求体：文本增量
	fin    bool           // 输入已收口/会话已死：后续增量丢弃
}

func startPlayback(base string, hc *http.Client, log *slog.Logger) (*ttsPlayback, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/tts/stream_input", pr)
	if err != nil {
		cancel()
		pw.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	// PCM16 单声道 16k（backend tts_rvc 的输出规格）。
	cmd := exec.CommandContext(ctx, "paplay", "--raw",
		"--format=s16le", "--rate=16000", "--channels=1")
	audioPr, audioPw := io.Pipe()
	cmd.Stdin = audioPr
	if err := cmd.Start(); err != nil {
		cancel()
		pw.Close()
		audioPw.Close()
		return nil, err
	}

	go func() {
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("tts: 请求失败", "err", err)
			}
			_ = pw.CloseWithError(err) // feed 侧随即 fin
			audioPw.Close()
			return
		}
		go func() { defer resp.Body.Close() }()
		// 响应体 = WAV 头 + PCM16 流；剥头直喂 paplay，排空即自然收尾。
		if _, err := io.CopyN(io.Discard, resp.Body, wavHeaderLen); err == nil {
			_, _ = io.Copy(audioPw, resp.Body)
		}
		audioPw.Close()
		_ = cmd.Wait()
	}()

	return &ttsPlayback{ctx: ctx, cancel: cancel, pw: pw}, nil
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
	p.fin = true
	p.cancel() // 掐 HTTP 请求与 paplay（exec.CommandContext）
	_ = p.pw.Close()
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
