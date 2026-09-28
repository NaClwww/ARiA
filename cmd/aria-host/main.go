// aria-host 是 Gowild-HE 场景的宿主薄壳（docs/06「产品入口 = 事件流订阅者 +
// Scope 制造者」）：语音的 IO 细节归插件（plugins/voice/gowild），壳只做接线——
//
//	音箱 mic → gowild.ASR（backend :8800 SSE，final 成轮、partial 驱灯）→
//	Session.Input → LLM → 事件流 → gowild.TTS（流式合成 → 设备扬声器/
//	paplay）；会话状态映射到机身 RGB 指示灯（launcher /api/light：待机暗白/
//	收听绿/思考蓝）；终端 stdin 为并存开发插头
//
// 尚未接入：口型同步、真·抢话（现在是半双工闸门：说话/生成期间不收新
// 输入）、人设标签剥离与表情映射。引擎不动。
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
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	gowild "aria/plugins/voice/gowild"
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

	// 半双工闸门（引用计数 + 迁移订阅）：输入纪律、TTS、状态灯共用的
	// 信号源——「她正忙着说」这条总线在插件里，宿主只接线。
	gate := gowild.NewGate()

	// TTS 驱动：事件流的第二个消费者（一切服务都是事件订阅者）。
	var ttsDone <-chan struct{}
	var ttsUnsub func()
	if !*noTTS {
		tts, terr := gowild.NewTTS(gowild.TTSConfig{Base: *backend, Device: *device}, gate, log)
		if terr != nil {
			log.Error("tts 装配失败", "err", terr)
			os.Exit(2)
		}
		ttsCh, u := sess.Subscribe(0)
		ttsUnsub = u
		done := make(chan struct{})
		ttsDone = done
		go func() {
			defer close(done)
			tts.Run(ttsCh)
		}()
	}

	// 状态灯：订阅闸门迁移 + ASR partial 心跳（摄像头旁那颗 RGB 指示灯；
	// /api/light color 模式只驱三色通道，不碰暖白舞台环）。仅设备模式存在。
	var light *gowild.Light
	if *device != "" && !*noLight {
		ld, lerr := gowild.NewLight(gowild.LightConfig{Base: *device, Colors: *lightColors}, gate, log)
		if lerr != nil {
			log.Error("light 装配失败", "err", lerr)
			os.Exit(2)
		}
		light = ld
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

	// 输入纪律（半双工：拦截 + 轮次份额）——策略在插件，引擎投递口在本壳。
	input, ierr := gowild.NewInput(gowild.InputConfig{
		Sink:   engineSink{sess: sess, cfg: cfg},
		Gate:   gate,
		Bypass: *noInputGate,
		OnIgnored: func(text string) {
			fmt.Fprintf(os.Stderr, "·（正在说话，忽略输入）%s\n", truncStr(strings.TrimSpace(text), 40))
		},
	}, log)
	if ierr != nil {
		log.Error("input 装配失败", "err", ierr)
		os.Exit(2)
	}
	var plugs []string
	quit := make(chan struct{})
	if !*noASR {
		var onPartial func()
		if light != nil {
			onPartial = light.Heartbeat
		}
		asr, aerr := gowild.NewASR(gowild.ASRConfig{
			Base:    *backend,
			Speaker: cfg.Session.DefaultUser,
			OnFinal: func(text, speaker string) {
				fmt.Fprintf(os.Stderr, "%s(语音)> %s\n", speaker, text) // 渲染归宿主，插件只交付
				input.Deliver(text, speaker)
			},
			OnPartial: onPartial,
		}, log)
		if aerr != nil {
			log.Error("asr 装配失败", "err", aerr)
			os.Exit(2)
		}
		go asr.Run(context.Background())
		plugs = append(plugs, "asr")
	}
	if !*noStdin {
		// stdin EOF 是否收工取决于有没有 ASR 插头：语音形态要常驻。
		go stdinPlug(input.Deliver, cfg.Session.DefaultUser, *noASR, quit)
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
		light.SettleIdle() // 灯是音箱的资产：退出前收回待机，别留在一半的状态上
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

// engineSink 是 gowild.Input 的引擎投递口：多插头纪律（06 §2）归宿主——
// 在途轮次先试 Queue（轮间注入，非阻塞）；空闲（ErrNoActiveRun）则用
// Input 起一轮、阻塞到本轮结算。先后由会话锁与到达顺序决定，与「谁先
// 来谁先进」一致。说话人标签与当次模型参数都在这里落。
type engineSink struct {
	sess *agent.Session
	cfg  config.Config
}

func (s engineSink) Deliver(text, speaker string) error {
	sess, cfg := s.sess, s.cfg
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
