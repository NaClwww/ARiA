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
type Engine struct {
	Cfg     config.Config
	Session *agent.Session

	log        *slog.Logger
	jsonlStore *jsonl.Store
	fake       bool
	model      string // 生效模型名（deepseek kind 留空配置时为默认 flash）
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

	var prov provider.Provider
	var model string
	if opts.Fake {
		prov = echoProvider{}
		model = "echo"
		mgr.SetLocal("compress.strategy", "keeplast") // echo 不做真摘要，保持确定性
		cfg = mgr.Effective()
		log.Info("provider: echo（无网络冒烟）")
	} else {
		// 配置 → provider 走共享装配表（kind=deepseek 官方特化 / openai 兼容；
		// 模型默认、环境变量兜底都在 assemble.Provider 里，与 aria-demo 同源）。
		res, perr := assemble.Provider(cfg, opts.APIKey)
		if perr != nil {
			return nil, fmt.Errorf("真实模式 provider 装配失败: %w（或用 --api-key/--fake）", perr)
		}
		prov, model = res.Impl, res.Model
		log.Info("provider", "kind", res.Kind, "model", res.Model,
			"base_url", orDefault(cfg.Provider.BaseURL, "(官方默认)"))
	}

	compressor, err := assemble.Compressor(cfg, prov)
	if err != nil {
		return nil, fmt.Errorf("压缩策略装配失败: %w", err)
	}
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
	systemPrompt, err := ResolvePersona(cfg.Persona)
	if err != nil {
		return nil, fmt.Errorf("人设加载失败: %w", err)
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
	if opts.SpeakTool && cfg.Limits.ToolTimeoutMS < gowildvoice.MinSpeakToolTimeoutMS {
		// 阻塞放音要盖过最长段落的播放时长。
		mgr.SetLocal("limits.tool_timeout_ms", gowildvoice.MinSpeakToolTimeoutMS)
		cfg = mgr.Effective()
		log.Info("speak 工具：tool_timeout_ms 提到播放需要的上限", "ms", cfg.Limits.ToolTimeoutMS)
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

	// 内置工具：按 [tools] builtin 的名字从共享注册表（plugins/tool/basic）
	// 挑选；未知名字报错（fail-loud，与 aria-demo 同纪律——静默少一个工具
	// 比启动失败更难排查）。空名单 = 不挂工具。
	registry := basic.All()
	tools := make([]tool.Tool, 0, len(cfg.Tools.Builtin)+len(opts.Tools))
	for _, name := range cfg.Tools.Builtin {
		t, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("未知内置工具 %q（可选：now, calc, random, weather, bash）", name)
		}
		tools = append(tools, t)
	}
	tools = append(tools, opts.Tools...)
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Def().Name)
		}
		log.Info("tools", "count", len(tools), "names", names)
	}

	ag, err := agent.New(agent.Config{
		Provider:     prov,
		Tools:        tools,
		Compressor:   compressor,
		Assembler:    vision,
		Store:        store,
		SystemPrompt: systemPrompt,
		MaxTurns:     cfg.Limits.MaxTurns,
		ToolTimeout:  time.Duration(cfg.Limits.ToolTimeoutMS) * time.Millisecond,
		Logger:       log,
	})
	if err != nil {
		return nil, fmt.Errorf("agent 装配失败: %w", err)
	}
	sess, err := ag.NewSession(ctxx.Scope{SessionID: cfg.Session.ID, UserID: cfg.Session.DefaultUser})
	if err != nil {
		return nil, fmt.Errorf("会话建立失败: %w", err)
	}
	return &Engine{Cfg: cfg, Session: sess, log: log, jsonlStore: jsonlStore, fake: opts.Fake, model: model}, nil
}

// ModelName 是对外展示的模型名（echo 冒烟时为 "echo"；deepseek kind 留空
// 配置时展示生效默认 deepseek-flash，不展示空串）。
func (e *Engine) ModelName() string {
	if e.fake {
		return "echo"
	}
	return e.model
}

// Close 收尾落盘（会话本身的 Close 由宿主在等待消费者退出前先行调用，
// 顺序是宿主的职责）。重复调用安全。
func (e *Engine) Close() error {
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
