// aria-host 是 Gowild-HE 场景的宿主薄壳（docs/06「产品入口 = 事件流订阅者 +
// Scope 制造者」）：main 只是一份挂载清单（runtime/app 收编订阅/goroutine/
// 退订/等待/退出钩子），组件在 internal/aria-host（引擎组装/投递纪律/终端
// 插头/渲染/人设），语音栈在 plugins/voice/gowild（闸门/输入纪律/ASR/TTS/
// 状态灯/闭耳）：
//
//	mic → gowild.ASR ──final──→ gowild.Input ──→ ARiA 会话（ariahost.Engine）
//	  │partial                     │轮次份额        │事件流
//	  ↓                            ↓               ├──→ 终端渲染（ariahost）
//	gowild.Light ←──迁移订阅── gowild.Gate ←─生成/播放份额
//	gowild.MicMute ←─迁移订阅──┘（闭耳：回合中 backend 源头丢 mic 流，防自回声）
//
// 挂载顺序纪律（runtime/app，执行顺序是容器存在的理由）：链底 OnShutdown
// 先注册（会话/引擎关闭最后跑）→ 事件消费者（终端/TTS）→ 闸门订阅者
// （闭耳/灯）→ 输入插头最后（消费者就位才放输入）；收尾严格逆序。
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
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"aria/core/loop"
	ariahost "aria/internal/aria-host"
	gowild "aria/plugins/voice/gowild"
	"aria/runtime/app"
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
		noMicMute    = flag.Bool("no-mic-mute", false, "不在回合中闭耳（默认开：agent 生成/放音期间 backend 源头丢 mic 流，防自回声+省解码）")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	eng, err := ariahost.NewEngine(ariahost.Options{
		ConfigPath: *configPath, OverridePath: *overridePath,
		Fake: *fake, APIKey: *apiKey, Logger: log,
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

	// 半双工闸门（引用计数 + 迁移订阅）：输入纪律、TTS、状态灯、闭耳共用
	// 的信号源——「她正忙着说」这条总线在插件里，宿主只接线。
	gate := gowild.NewGate()

	// ---- 事件消费者（输入插头之前挂：事件不重放，晚订阅者错过即错过）----

	// 消费者 1：终端渲染（stdout 回答增量，stderr 工具轨迹）。
	app.Mount("terminal", 0, func(ch <-chan loop.Event) {
		ariahost.ConsumeTerminal(ch, os.Stdout, os.Stderr)
	})

	// 消费者 2：TTS（流式合成 → 设备扬声器/paplay；闸门的生成/播放份额在此持有）。
	if !*noTTS {
		tts, terr := gowild.NewTTS(gowild.TTSConfig{Base: *backend, Device: *device}, gate, log)
		if terr != nil {
			log.Error("tts 装配失败", "err", terr)
			os.Exit(2)
		}
		app.Mount("tts", 0, tts.Run)
	}

	// ---- 闸门订阅者 ----

	// 闭耳：回合中 backend 源头丢 mic 流（防自回声 + 省流式解码）。收尾先
	// 停桥再显式开耳——耳朵是音箱的资产，别把 backend 留在闭耳态。
	if !*noASR && !*noMicMute {
		micMute := gowild.NewMicMute(*backend, log)
		cancel := gowild.FollowGate(gate, micMute, 300*time.Millisecond)
		app.OnShutdown("micmute", func() { cancel(); micMute.Set(false) })
	}

	// 状态灯（摄像头旁 RGB）：订阅闸门迁移 + partial 心跳；退出收回待机。
	var light *gowild.Light
	if *device != "" && !*noLight {
		ld, lerr := gowild.NewLight(gowild.LightConfig{Base: *device, Colors: *lightColors}, gate, log)
		if lerr != nil {
			log.Error("light 装配失败", "err", lerr)
			os.Exit(2)
		}
		light = ld
		app.OnShutdown("light", light.SettleIdle)
		log.Info("light: 状态灯联动（待机暗白 / 收听绿 / 思考蓝）", "device", *device)
	}

	// 输入纪律（半双工：拦截 + 轮次份额）——策略在插件，引擎投递口在本壳。
	input, ierr := gowild.NewInput(gowild.InputConfig{
		Sink:   ariahost.Sink{Session: sess, Cfg: cfg},
		Gate:   gate,
		Bypass: *noInputGate,
		OnIgnored: func(text string) {
			fmt.Fprintf(os.Stderr, "·（正在说话，忽略输入）%s\n", ariahost.TruncStr(strings.TrimSpace(text), 40))
		},
	}, log)
	if ierr != nil {
		log.Error("input 装配失败", "err", ierr)
		os.Exit(2)
	}

	// Ctrl+C：第一次打断当前回答（steering），第二次强制退出。信号处理
	// 不进 app（要拿 sess.Interrupt），强退兜底也在这里。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\n（打断当前回答；再按一次强制退出）")
		sess.Interrupt()
		<-sig
		os.Exit(130)
	}()

	// ---- 输入插头最后挂：所有消费者就位后才放输入进来 ----
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
		app.Service("asr", asr.Run) // ctx 取消即断流退场（旧版 Background 从不收）
		plugs = append(plugs, "asr")
	}
	if !*noStdin {
		// stdin EOF 是否收工取决于有没有 ASR 插头：语音形态要常驻；quit 由
		// /quit/EOF 触发，Service 收尾等它自然返回即可。
		app.Service("stdin", func(context.Context) {
			ariahost.StdinPlug(os.Stdin, input.Deliver, cfg.Session.DefaultUser, *noASR, quit)
		})
		plugs = append(plugs, "stdin")
	}
	if len(plugs) == 0 {
		fmt.Fprintln(os.Stderr, "没有启用的插头（--no-asr --no-stdin）——无事可做，退出")
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "ARiA host · 插头 %v · backend %s · 模型 %s · 会话 %s · 说话人 %s\n",
		plugs, orDefault(*backend, defaultBackend), eng.ModelName(), cfg.Session.ID, cfg.Session.DefaultUser)
	fmt.Fprintln(os.Stderr, "输入一句话回车发送；[名字] 开头切换说话人；/quit 退出；Ctrl+C 打断。")

	// 阻塞到 /quit/EOF，然后逆序收尾：输入插头先停 → 消费者排干 → 灯/耳
	// 资产回收 → 会话结算 → record 收口。
	app.Run(quit)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
