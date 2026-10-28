// Package ariahost 是 aria-host 宿主的定制组件集（docs/06 §5 宿主侧零件，
// internal/assemble 的同族）：main 只做接线，逻辑归宿主组件——
//
//	Engine      配置 → provider（真实/echo）→ agent → 会话 的组装与收尾
//	Sink        多插头投递纪律（06 §2：Queue 轮间注入 → Input 起轮）
//	StdinPlug   终端输入插头（[名字] 前缀切换说话人）
//	ConsumeTerminal 事件流的终端渲染
//	ResolvePersona  人设解析（文件/内联/内置默认）
//
// 语音栈（ASR/TTS/闸门/灯/输入纪律）在 plugins/voice/gowild，引擎在
// core/runtime——本包是两者的宿主侧粘合件，不引入新的引擎概念。
package ariahost

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/internal/assemble"
	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/plugins/persist/jsonl"
	"aria/plugins/tool/basic"
	gowildvision "aria/plugins/vision/gowild"
	gowildvoice "aria/plugins/voice/gowild"
	"aria/runtime/agent"
	"aria/runtime/memory"
	"aria/runtime/persist"
)

// Options 是 Engine 的组装参数（flag 的物化）。
type Options struct {
	ConfigPath   string // 配置文件路径（人写基准）
	OverridePath string // 覆盖文件路径（机器写）
	Fake         bool   // 使用内置 echo provider（无网络冒烟）
	APIKey       string // API key；空则按配置的 api_key_env 读环境变量
	// VisionBase 是 launcher 根地址；非空 = 启用视觉注入（摄像头当前帧
	// 每轮进组装链底部，plugins/vision/gowild）。空 = 无视觉。
	VisionBase string
	// Tools 是宿主装配的额外工具（如 speak——依赖宿主的 gate/TTS，引擎
	// 无从代建）：与 [tools] builtin 注册表工具合并成会话工具表。
	Tools []tool.Tool
	// ExtraPrompt 追加到人设之后的宿主声明（如 speak 工具的使用规则——
	// 工具存在与否是宿主的装配事实，得让人设知道）。
	ExtraPrompt string
	// SpeakTool=true 表示宿主开了 speak 工具：tool_timeout_ms 自动抬到
	// gowild.MinSpeakToolTimeoutMS（30s 默认会把长段播放拦腰掐断）。
	SpeakTool bool
	Logger    *slog.Logger
}

// Engine 是组装完成的宿主引擎面：配置快照 + 就绪会话。
//
// 配置热更新：Config 的变更（Reload / 文件监听 / Set）经 OnChange 交给 Live 应用到会话；
// Cfg 是启动时的快照，只给构造期参数用，每请求参数现取 Config.Effective()。
type Engine struct {
	Cfg     config.Config
	Config  *config.Manager
	Session *agent.Session
	Live    *assemble.Live

	log        *slog.Logger
	jsonlStore *jsonl.Store
	memClose   func(context.Context) error // 记忆服务的后台提交等待（未接入时为 nil）
	fake       bool
	provs      *assemble.ProviderCache // 生效模型名的来源（热更新换模型后随之变化）
}

// NewEngine 走完「配置 → provider → 压缩 → 人设 → 落盘 → agent → 会话」
// 的组装链；任何一步不合法即返回错误（fail-loud，宿主决定退出码）。
func NewEngine(opts Options) (*Engine, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	mgr, err := config.Load(opts.ConfigPath, opts.OverridePath)
	if err != nil {
		return nil, fmt.Errorf("配置加载失败: %w（仓库根有 aria.toml 样例，路径可用 --config 指定）", err)
	}
	cfg := mgr.Effective()

	var fakeProv provider.Provider
	if opts.Fake {
		fakeProv = echoProvider{}
		mgr.SetLocal("compress.strategy", "keeplast") // echo 不做真摘要，保持确定性
		cfg = mgr.Effective()
		log.Info("provider: echo（无网络冒烟）")
	}

	// 启动时建一次、热更新不换的部件：视觉（[host] 装配项）、落盘、记忆服务。
	//
	// 视觉注入（可空）：槽 1 的额外变换位，作用在窗口组装结果之上——
	// 摄像头当前帧追加在组装结果最底部，不进历史不落盘。
	var vision loop.Assembler
	if opts.VisionBase != "" {
		v, verr := gowildvision.New(gowildvision.Config{Base: opts.VisionBase}, log)
		if verr != nil {
			return nil, fmt.Errorf("视觉装配失败: %w", verr)
		}
		vision = v
		log.Info("vision: 固定视觉参考区启用（每次请求更新一张，不作为用户发言）", "device", opts.VisionBase)
	}
	var store persist.Store
	var jsonlStore *jsonl.Store
	if cfg.Record.Path != "" {
		s, err := jsonl.New(cfg.Record.Path)
		if err != nil {
			return nil, fmt.Errorf("落盘打开失败 %s: %w", cfg.Record.Path, err)
		}
		store, jsonlStore = s, s
		log.Info("record", "path", cfg.Record.Path)
	}
	mem, err := assemble.Memory(cfg.Memory, log)
	if err != nil {
		return nil, fmt.Errorf("记忆服务装配失败: %w", err)
	}
	if mem.Service != nil {
		log.Info("memory", "engine", cfg.Memory.Engine, "base_url", cfg.Memory.BaseURL,
			"dir", cfg.Memory.Dir, "members", len(cfg.Memory.Members))
	}

	// build 是「配置 → agent.Config」的装配：启动与热更新（assemble.Live）共用这一份。
	provs := &assemble.ProviderCache{APIKey: opts.APIKey}
	build := func(cfg config.Config) (agent.Config, string, error) {
		prov, note := fakeProv, "provider=echo"
		if prov == nil {
			// 配置 → provider 走共享装配表（与 aria-demo 同源）；[provider] 不变时复用实例。
			res, err := provs.Get(cfg)
			if err != nil {
				return agent.Config{}, "", fmt.Errorf("provider: %w（或用 --api-key/--fake）", err)
			}
			prov, note = res.Impl, res.Note()
		}
		compressor, err := assemble.Compressor(cfg, prov)
		if err != nil {
			return agent.Config{}, "", fmt.Errorf("压缩策略: %w", err)
		}
		systemPrompt, err := ResolvePersona(cfg.Persona)
		if err != nil {
			return agent.Config{}, "", fmt.Errorf("人设: %w", err)
		}
		// 说话人标注声明无条件追加（自定义人设也不例外）：[名字] 前缀是
		// Sink 的投递格式，不是人设的风格选择——不声明模型就不认得这个标记。
		systemPrompt += "\n" + SpeakerInstruction(cfg.Session.DefaultUser)
		if vision != nil {
			systemPrompt += "\n" + gowildvision.VisualContextInstruction
		}
		if opts.ExtraPrompt != "" {
			systemPrompt += "\n" + opts.ExtraPrompt
		}
		// 内置工具：按 [tools] builtin 的名字从共享注册表（plugins/tool/basic）
		// 挑选；未知名字报错（fail-loud，与 aria-demo 同纪律——静默少一个工具
		// 比启动失败更难排查）。空名单 = 不挂工具。
		registry := basic.All()
		tools := make([]tool.Tool, 0, len(cfg.Tools.Builtin)+len(opts.Tools)+1)
		for _, name := range cfg.Tools.Builtin {
			t, ok := registry[name]
			if !ok {
				return agent.Config{}, "", fmt.Errorf("未知内置工具 %q（可选：now, calc, random, weather, bash）", name)
			}
			tools = append(tools, t)
		}
		tools = append(tools, opts.Tools...)
		// 记忆服务接入时装入 memory_recall 工具并在人设后追加使用规则（工具存在与否是
		// 宿主的装配事实，与 speak 同纪律）。
		if mem.Service != nil {
			tools = append(tools, mem.Tool)
			systemPrompt += "\n" + memory.RecallToolInstruction
		}
		toolTimeoutMS := cfg.Limits.ToolTimeoutMS
		if opts.SpeakTool && toolTimeoutMS < gowildvoice.MinSpeakToolTimeoutMS {
			// 阻塞放音要盖过最长段落的播放时长（配置更大则按配置）。
			toolTimeoutMS = gowildvoice.MinSpeakToolTimeoutMS
		}
		return agent.Config{
			Provider:        prov,
			Tools:           tools,
			Compressor:      compressor,
			KeepRecentTurns: cfg.Compress.KeepRecentTurns,
			Compact:         assemble.CompactBudget(cfg.Compress),
			IdleTimeout:     assemble.IdleTimeout(cfg.Session),
			RecallWait:      assemble.RecallWait(cfg.Session),
			Assembler:       vision,
			SystemPrompt:    systemPrompt,
			MaxTurns:        cfg.Limits.MaxTurns,
			ToolTimeout:     time.Duration(toolTimeoutMS) * time.Millisecond,
			Logger:          log,
		}, note, nil
	}

	acfg, note, err := build(cfg)
	if err != nil {
		return nil, fmt.Errorf("装配失败: %w", err)
	}
	names := make([]string, 0, len(acfg.Tools))
	for _, t := range acfg.Tools {
		names = append(names, t.Def().Name)
	}
	log.Info("assembled", "note", note, "base_url", orDefault(cfg.Provider.BaseURL, "(官方默认)"),
		"compress", cfg.Compress.Strategy, "tools", names, "tool_timeout", acfg.ToolTimeout)

	sessionID, newSessionID := assemble.SessionIDs(cfg.Session, time.Now())
	acfg.Memory, acfg.Store, acfg.NewSessionID = mem.Service, store, newSessionID
	ag, err := agent.New(acfg)
	if err != nil {
		return nil, fmt.Errorf("agent 装配失败: %w", err)
	}
	sess, err := ag.NewSession(ctxx.Scope{SessionID: sessionID, UserID: cfg.Session.DefaultUser, Namespace: cfg.Session.Namespace})
	if err != nil {
		return nil, fmt.Errorf("会话建立失败: %w", err)
	}
	live := assemble.NewLive(sess, build, cfg, log)
	mgr.OnChange(live.Apply)
	return &Engine{Cfg: cfg, Config: mgr, Session: sess, Live: live, log: log, jsonlStore: jsonlStore,
		memClose: mem.Close, fake: opts.Fake, provs: provs}, nil
}

// ModelName 是对外展示的模型名（echo 冒烟时为 "echo"；deepseek kind 留空
// 配置时展示生效默认 deepseek-flash，不展示空串）。
func (e *Engine) ModelName() string {
	if e.fake {
		return "echo"
	}
	return e.provs.Model()
}

// Close 收尾落盘（会话本身的 Close 由宿主在等待消费者退出前先行调用，
// 顺序是宿主的职责）。重复调用安全。
func (e *Engine) Close() error {
	if e.memClose != nil {
		// 记忆服务的后台提交（遗留会话）以 agent.DefaultCloseGrace 为上限等待；超时则放行，
		// 未完成的提交由下一次启动的 Start 重做（document_id 幂等）。
		ctx, cancel := context.WithTimeout(context.Background(), agent.DefaultCloseGrace)
		if err := e.memClose(ctx); err != nil {
			e.log.Warn("memory: background commit still running at close", "err", err)
		}
		cancel()
	}
	if e.jsonlStore != nil {
		return e.jsonlStore.Close()
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
