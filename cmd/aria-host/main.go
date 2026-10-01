// aria-host 是 Gowild-HE 场景的宿主薄壳（docs/06「产品入口 = 事件流订阅者 +
// Scope 制造者」）：main 只是一份挂载清单（runtime/app 收编订阅/goroutine/
// 退订/等待/退出钩子），组件在 internal/aria-host（引擎组装/投递纪律/终端
// 插头/渲染/人设），语音栈在 plugins/voice/gowild（闸门/输入纪律/ASR/TTS/
// 状态灯/闭耳），视觉在 plugins/vision/gowild（摄像头当前帧每轮进组装链
// 底部，--device 模式）：
//
//	mic → gowild.ASR ──final──→ gowild.Input ──→ ARiA 会话（ariahost.Engine）
//	  │partial                     │轮次份额        │事件流
//	  ↓                            ↓               ├──→ 终端渲染（ariahost）
//	gowild.Light ←──迁移订阅── gowild.Gate ←─生成/播放份额
//	gowild.MicMute ←──迁移订阅──┘（闭耳：回合中 backend 源头丢 mic 流，防自回声）
//
// asr-gateway 模式（EPYC VM 网关，见 Gowild-HE/asr_backend）：mic 由
// gowild.Gateway 自己拉音箱流转发网关 WS（FSMN-VAD+整句 ASR+声纹），
// speech_start 驱灯、final 交付同上；闭耳 = 网关 reset + 本地停转
// （MicMute/backend SSE 不参与，TTS 仍走 backend）。
//
// 挂载顺序纪律（runtime/app，执行顺序是容器存在的理由）：链底 OnShutdown
// 先注册（会话/引擎关闭最后跑）→ 事件消费者（终端/TTS）→ 闸门订阅者
// （闭耳/灯）→ 输入插头最后（消费者就位才放输入）；收尾严格逆序。
//
// 尚未接入：口型同步、真·抢话（现在是半双工闸门：说话/生成期间不收新
// 输入）、人设标签剥离与表情映射。引擎不动。
//
// 启动项：装配项（backend/asr_gateway/device/light_colors/各 no_* 开关）
// 的基准在 aria.toml [host] 段，机器差异写 aria.override.toml；同名 flag
// 显式给出时仅本次运行覆盖。留成 flag 的只有引导三件（--config/--override
// 路径本身）、--fake（冒烟开关）和 --api-key（秘密，本体不落盘）。
//
// 用法：
//
//	aria-host --fake --no-asr --no-tts   # 纯 stdin 冒烟（无网络、不花钱）
//	aria-host --fake                     # stdin + ASR + TTS（echo 冒烟）
//	aria-host                            # 按 aria.toml 连真实模型
//
// 交互：[名字] 开头切换说话人；/quit 按收尾链退出；Ctrl+C 打断当前回答，2 秒内
// 再按一次按收尾链退出，收尾期间再按一次强制退出；SIGTERM 按收尾链退出。
// stdin EOF 在 ASR 模式下不退出（常驻语音形态）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"aria/core/loop"
	"aria/core/tool"
	ariahost "aria/internal/aria-host"
	"aria/internal/config"
	gowild "aria/plugins/voice/gowild"
	"aria/runtime/app"
)

func main() {
	// 引导项：只能走 flag（config 路径本身 / 冒烟开关 / 秘密）。
	fake := flag.Bool("fake", false, "使用内置 echo provider（无网络，验证链路）")
	configPath := flag.String("config", "aria.toml", "配置文件路径（人写基准，启动时读一次）")
	overridePath := flag.String("override", "aria.override.toml", "覆盖文件路径")
	apiKey := flag.String("api-key", "", "API key；空则按配置的 api_key_env 读环境变量")

	// 装配项：基准在 aria.toml [host] 段，flag 只做「本次运行」的显式覆盖
	// ——默认零值 = 不覆盖，生效值一律以解析后的 h 为准。声明、帮助与写回
	// 收进同一条表项（hostKnob）：新增 [host] 旋钮 = 表里加一行，不再散在
	// flag 定义 / set 判断 / 赋值三处。
	strKnob := func(name, help string, apply func(*config.Host, string)) hostKnob[string] {
		return hostKnob[string]{name: name, apply: apply, val: flag.String(name, "", help)}
	}
	boolKnob := func(name, help string, apply func(*config.Host, bool)) hostKnob[bool] {
		return hostKnob[bool]{name: name, apply: apply, val: flag.Bool(name, false, help)}
	}
	strKnobs := []hostKnob[string]{
		strKnob("backend", "覆盖 [host] backend：TTS 后端根地址（非 gateway 模式下也是 ASR 地址）",
			func(h *config.Host, v string) { h.Backend = v }),
		strKnob("asr-gateway", "覆盖 [host] asr_gateway：asr-gateway WS 根地址（EPYC VM）；非空 = ASR 走网关（需 device；TTS 仍走 backend）",
			func(h *config.Host, v string) { h.ASRGateway = v }),
		strKnob("device", "覆盖 [host] device：launcher 控制面根地址；空 = 本机 paplay",
			func(h *config.Host, v string) { h.Device = v }),
		strKnob("light-colors", "覆盖 [host] light_colors：灯色 idle,listening,thinking（hex，逗号分隔）",
			func(h *config.Host, v string) { h.LightColors = v }),
	}
	boolKnobs := []hostKnob[bool]{
		boolKnob("no-asr", "覆盖 [host] no_asr：停用 ASR 插头（纯终端开发）",
			func(h *config.Host, v bool) { h.NoASR = v }),
		boolKnob("no-stdin", "覆盖 [host] no_stdin：停用 stdin 插头（纯语音）",
			func(h *config.Host, v bool) { h.NoStdin = v }),
		boolKnob("no-tts", "覆盖 [host] no_tts：停用 TTS 播放（只看文字）",
			func(h *config.Host, v bool) { h.NoTTS = v }),
		boolKnob("no-input-gate", "覆盖 [host] no_input_gate：关闭「播放期间不接受新输入」闸门（半双工）",
			func(h *config.Host, v bool) { h.NoInputGate = v }),
		boolKnob("no-light", "覆盖 [host] no_light：停用状态灯（会话状态 → 设备 RGB 指示灯，仅 device 模式）",
			func(h *config.Host, v bool) { h.NoLight = v }),
		boolKnob("no-mic-mute", "覆盖 [host] no_mic_mute：停用播放期间闭耳（默认开：实际送播至排空期间丢 mic，之后保留 300ms 尾音缓冲）",
			func(h *config.Host, v bool) { h.NoMicMute = v }),
		boolKnob("no-vision", "覆盖 [host] no_vision：停用视觉注入（默认开：摄像头当前帧每轮进上下文底部，仅 device 模式）",
			func(h *config.Host, v bool) { h.NoVision = v }),
		boolKnob("speak-tool", "覆盖 [host] speak_tool：TTS 改为 speak 工具（模型显式调用出声、正文不自动朗读）",
			func(h *config.Host, v bool) { h.SpeakTool = v }),
	}
	flag.Parse()

	// 显式给出的 flag 才参与覆盖：Visit 只报命令行真正出现的名字，零值
	// 默认不算。装配项全部收口到 h，此后只读 h。
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// [host] 要在引擎装配前生效（device/no_vision 是 NewEngine 的入参），
	// 先读一份；NewEngine 内部会再 Load 一次（两个小文件，双读无碍）。
	mgr, err := config.Load(*configPath, *overridePath)
	if err != nil {
		log.Error("配置加载失败（仓库根有 aria.toml 样例，路径可用 --config 指定）", "err", err)
		os.Exit(2)
	}
	h := mgr.Effective().Host
	for _, k := range strKnobs {
		k.applyExplicit(set, &h)
	}
	for _, k := range boolKnobs {
		k.applyExplicit(set, &h)
	}
	if h.SpeakTool && h.NoTTS {
		log.Error("speak_tool 需要 TTS 管线（不能同时给 no_tts）")
		os.Exit(2)
	}

	visionBase := h.Device
	if h.NoVision {
		visionBase = ""
	}

	// 半双工闸门与 TTS 管线先于 agent 就位：speak 工具复用 TTS 管线与闸门
	// （工具表在引擎装配 agent.New 时就要交出去），闸门又是 TTS/灯/闭耳/
	// 输入纪律共用的信号源。
	gate := gowild.NewGate()         // 轮次忙碌，供状态灯使用
	playbackGate := gowild.NewGate() // 仅实际放音，供半双工输入使用
	var tts *gowild.TTS
	var speakTools []tool.Tool
	extraPrompt := ""
	if !h.NoTTS {
		t, terr := gowild.NewTTS(gowild.TTSConfig{
			Base: h.Backend, Device: h.Device, SilentBody: h.SpeakTool, PlaybackGate: playbackGate,
		}, gate, log)
		if terr != nil {
			log.Error("tts 装配失败", "err", terr)
			os.Exit(2)
		}
		tts = t
		// 语音声明跟装配走：模型得知道这台设备上「什么话会出声、怎么
		// 说才像人话」——两种模式答案不同，见各 Instruction 注释。
		if h.SpeakTool {
			speakTools = append(speakTools, gowild.NewSpeakTool(tts), gowild.NewStopTool())
			extraPrompt = gowild.SpeakToolInstruction
			log.Info("tts: speak 工具模式（模型显式调用出声、正文不再自动朗读；block 可选阻塞/非阻塞；stop 显式收尾）")
		} else {
			extraPrompt = gowild.VoiceRenderInstruction
		}
	}

	eng, err := ariahost.NewEngine(ariahost.Options{
		ConfigPath: *configPath, OverridePath: *overridePath,
		Fake: *fake, APIKey: *apiKey, VisionBase: visionBase,
		Tools: speakTools, ExtraPrompt: extraPrompt, SpeakTool: h.SpeakTool, Logger: log,
	})
	if err != nil {
		log.Error("engine 装配失败", "err", err)
		os.Exit(2)
	}
	sess, cfg := eng.Session, eng.Cfg

	// 挂载与生命周期容器：从这里到 app.Run 之前就是全部装配——一份清单。
	app := app.New(sess, log)

	// 链底（最先注册 → 最后执行）：先关会话（结算落盘）再关引擎（record
	// 收口）。注意注册序 = 收尾执行序的逆序——engine 先注册才能最后关。
	app.OnShutdown("engine", func() {
		if err := eng.Close(); err != nil {
			log.Error("record close", "err", err)
		}
	})
	app.OnShutdown("session", func() {
		if err := sess.Close(); err != nil {
			log.Error("session close", "err", err)
		}
	})

	// 半双工闸门（引用计数 + 迁移订阅）：已在引擎装配前创建（speak 工具
	// 复用）——输入纪律、TTS、状态灯、闭耳共用的信号源，「她正忙着说」
	// 这条总线在插件里，宿主只接线。

	// ---- 事件消费者（输入插头之前挂：事件不重放，晚订阅者错过即错过）----

	// 消费者 1：终端渲染（stdout 回答增量，stderr 工具轨迹）。
	app.Mount("terminal", 0, func(ch <-chan loop.Event) {
		ariahost.ConsumeTerminal(ch, os.Stdout, os.Stderr)
	})

	// 消费者 2：TTS（流式合成 → 设备扬声器/paplay；闸门的生成/播放份额在此
	// 持有）。speak 工具模式下正文不上嘴，消费者只留轮次份额与未出声提示。
	if tts != nil {
		app.Mount("tts", 0, tts.Run)
	}

	// ---- 闸门订阅者 ----

	// 闭耳：回合中在源头丢 mic 流（防自回声 + 省流式解码）。收尾先停桥再
	// 显式开耳——耳朵是音箱的资产，别把水槽留在闭耳态。asr-gateway 模式
	// 的闭耳挂到网关插头自身（reset + 停转），在插头段装配。
	useGateway := h.ASRGateway != ""
	if !h.NoASR && !h.NoMicMute && !useGateway {
		micMute := gowild.NewMicMute(h.Backend, log)
		cancel := gowild.FollowGate(playbackGate, micMute, 300*time.Millisecond)
		app.OnShutdown("micmute", func() { cancel(); micMute.Set(false) })
	}

	// 状态灯（摄像头旁 RGB）：订阅闸门迁移 + partial 心跳；退出收回待机。
	var light *gowild.Light
	if h.Device != "" && !h.NoLight {
		ld, lerr := gowild.NewLight(gowild.LightConfig{Base: h.Device, Colors: h.LightColors}, gate, log)
		if lerr != nil {
			log.Error("light 装配失败", "err", lerr)
			os.Exit(2)
		}
		light = ld
		app.OnShutdown("light", light.SettleIdle)
		log.Info("light: 状态灯联动（待机暗白 / 收听绿 / 思考蓝）", "device", h.Device)
	}

	// 输入纪律（半双工：拦截 + 轮次份额）——策略在插件，引擎投递口在本壳。
	input, ierr := gowild.NewInput(gowild.InputConfig{
		Sink:         ariahost.Sink{Session: sess, Cfg: cfg},
		Gate:         gate,
		PlaybackGate: playbackGate,
		Bypass:       h.NoInputGate,
		OnIgnored: func(text string) {
			fmt.Fprintf(os.Stderr, "·（正在说话，忽略输入）%s\n", ariahost.TruncStr(strings.TrimSpace(text), 40))
		},
	}, log)
	if ierr != nil {
		log.Error("input 装配失败", "err", ierr)
		os.Exit(2)
	}

	// 退出请求：/quit、stdin EOF 与信号共用，幂等。
	quitCtx, requestQuit := context.WithCancel(context.Background())

	// 信号处理不进 app（要调用 sess.Interrupt）：
	//   - Ctrl+C 首次：打断当前回答（steering）；
	//   - 距上一次打断 forceQuitWindow 内再按 Ctrl+C，或收到 SIGTERM：requestQuit，按收尾链退出；
	//   - 收尾期间再收到信号：os.Exit(130) 强制退出。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		var lastInterrupt time.Time
		quitting := false
		for s := range sig {
			switch {
			case quitting:
				os.Exit(130)
			case s != os.Interrupt || time.Since(lastInterrupt) < forceQuitWindow:
				quitting = true
				fmt.Fprintln(os.Stderr, "\n（正在退出；再按一次 Ctrl+C 强制退出）")
				requestQuit()
			default:
				lastInterrupt = time.Now()
				fmt.Fprintf(os.Stderr, "\n（打断当前回答；%v 内再按一次退出）\n", forceQuitWindow)
				sess.Interrupt()
			}
		}
	}()

	// ---- 输入插头最后挂：所有消费者就位后才放输入进来 ----
	var plugs []string
	if !h.NoASR {
		var onPartial func()
		if light != nil {
			onPartial = light.Heartbeat
		}
		if useGateway {
			if h.Device == "" {
				log.Error("asr-gateway 模式需要 device（拉音箱 mic 流）")
				os.Exit(2)
			}
			gw, gerr := gowild.NewGateway(gowild.GatewayConfig{
				Base:    h.ASRGateway,
				MicURL:  strings.TrimSuffix(h.Device, "/") + "/api/voice/mic/stream",
				Speaker: cfg.Session.DefaultUser,
				OnFinal: func(text, speaker string) {
					fmt.Fprintf(os.Stderr, "%s(语音)> %s\n", speaker, text) // 渲染归宿主，插件只交付
					input.Deliver(text, speaker)
				},
				OnSpeechStart: onPartial, // 整句 ASR 无 partial，收听信号 = VAD 开口
			}, log)
			if gerr != nil {
				log.Error("asr-gateway 装配失败", "err", gerr)
				os.Exit(2)
			}
			app.Service("asr-gateway", gw.Run)
			plugs = append(plugs, "asr-gateway")
			if !h.NoMicMute {
				cancel := gowild.FollowGate(playbackGate, gw, 300*time.Millisecond)
				app.OnShutdown("micmute", func() { cancel(); gw.Set(false) })
			}
		} else {
			asr, aerr := gowild.NewASR(gowild.ASRConfig{
				Base:    h.Backend,
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
			app.Service("asr", asr.Run) // ctx 取消即断流退场（旧版 Background 从不收）
			plugs = append(plugs, "asr")
		}
	}
	if !h.NoStdin {
		// stdin EOF 是否退出取决于有没有 ASR 插头：语音形态要常驻。
		app.Service("stdin", func(ctx context.Context) {
			ariahost.StdinPlug(ctx, os.Stdin, input.Deliver, cfg.Session.DefaultUser, h.NoASR, requestQuit)
		})
		plugs = append(plugs, "stdin")
	}
	if len(plugs) == 0 {
		fmt.Fprintln(os.Stderr, "没有启用的插头（no_asr && no_stdin）——无事可做，退出")
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "ARiA host · 插头 %v · backend %s · 模型 %s · 会话 %s · 说话人 %s\n",
		plugs, h.Backend, eng.ModelName(), sess.SessionID(), cfg.Session.DefaultUser)
	if useGateway {
		fmt.Fprintf(os.Stderr, "asr-gateway %s（backend 仅承担 TTS）\n", h.ASRGateway)
	}
	fmt.Fprintln(os.Stderr, "输入一句话回车发送；[名字] 开头切换说话人；/quit 退出；Ctrl+C 打断。")

	// 阻塞到 /quit/EOF，然后逆序收尾：输入插头先停 → 消费者排干 → 灯/耳
	// 资产回收 → 会话结算 → record 收口。
	app.Run(quitCtx.Done())
}

// forceQuitWindow 是两次 Ctrl+C 判定为退出请求的最大间隔；超出则按新的一次打断处理。
const forceQuitWindow = 2 * time.Second

// hostKnob 收编一个 [host] 装配项的「flag 声明 + 显式才写回」：构造时已把
// flag 注册进默认 FlagSet，Parse 之后 applyExplicit 只对命令行真正出现的
// 名字执行写回（零值默认不算覆盖——机器差异的基准永远在 aria.toml）。
type hostKnob[T any] struct {
	name  string
	val   *T
	apply func(h *config.Host, v T)
}

func (k hostKnob[T]) applyExplicit(set map[string]bool, h *config.Host) {
	if set[k.name] {
		k.apply(h, *k.val)
	}
}
