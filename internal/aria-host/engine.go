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
	"os"
	"time"

	"aria/core/provider"
	"aria/internal/assemble"
	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/plugins/persist/jsonl"
	openai "aria/plugins/provider/openai"
	"aria/runtime/agent"
	"aria/runtime/persist"
)

// Options 是 Engine 的组装参数（flag 的物化）。
type Options struct {
	ConfigPath   string // 配置文件路径（人写基准）
	OverridePath string // 覆盖文件路径（机器写）
	Fake         bool   // 使用内置 echo provider（无网络冒烟）
	APIKey       string // API key；空则按配置 api_key_env 读环境变量
	Logger       *slog.Logger
}

// Engine 是组装完成的宿主引擎面：配置快照 + 就绪会话。
type Engine struct {
	Cfg     config.Config
	Session *agent.Session

	log        *slog.Logger
	jsonlStore *jsonl.Store
	fake       bool
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
	if opts.Fake {
		prov = echoProvider{}
		mgr.SetLocal("compress.strategy", "keeplast") // echo 不做真摘要，保持确定性
		cfg = mgr.Effective()
		log.Info("provider: echo（无网络冒烟）")
	} else {
		key := opts.APIKey
		if key == "" && cfg.Provider.APIKeyEnv != "" {
			key = os.Getenv(cfg.Provider.APIKeyEnv)
		}
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" || cfg.Provider.Model == "" {
			return nil, fmt.Errorf("真实模式需要模型与 API key：配置 provider.model + 环境变量 %s（或用 --api-key/--fake）",
				orDefault(cfg.Provider.APIKeyEnv, "ARIA_API_KEY"))
		}
		prov = openai.New(openai.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, Model: cfg.Provider.Model})
		log.Info("provider: openai-compatible", "model", cfg.Provider.Model,
			"base_url", orDefault(cfg.Provider.BaseURL, "(官方默认)"))
	}

	compressor, err := assemble.Compressor(cfg, prov)
	if err != nil {
		return nil, fmt.Errorf("压缩策略装配失败: %w", err)
	}
	systemPrompt, err := ResolvePersona(cfg.Persona)
	if err != nil {
		return nil, fmt.Errorf("人设加载失败: %w", err)
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
		return nil, fmt.Errorf("agent 装配失败: %w", err)
	}
	sess, err := ag.NewSession(ctxx.Scope{SessionID: cfg.Session.ID, UserID: cfg.Session.DefaultUser})
	if err != nil {
		return nil, fmt.Errorf("会话建立失败: %w", err)
	}
	return &Engine{Cfg: cfg, Session: sess, log: log, jsonlStore: jsonlStore, fake: opts.Fake}, nil
}

// ModelName 是对外展示的模型名（echo 冒烟时为 "echo"）。
func (e *Engine) ModelName() string {
	if e.fake {
		return "echo"
	}
	return e.Cfg.Provider.Model
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
