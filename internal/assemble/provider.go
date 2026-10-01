package assemble

import (
	"errors"
	"fmt"
	"os"

	"aria/core/provider"
	"aria/internal/config"
	deepseek "aria/plugins/provider/deepseek"
	openai "aria/plugins/provider/openai"
)

// ProviderNames 返回可用 provider kind（网页面板下拉框与错误提示用，顺序即推荐顺序）。
func ProviderNames() []string { return []string{"deepseek", "openai"} }

// ProviderResult 是 Provider 装配结果。Model 是生效模型名（kind=deepseek 且
// 未配置时为 deepseek.DefaultModel）——日志与 Engine.ModelName 都用它，
// 避免「配置留空、实际跑默认值」的展示错位。
type ProviderResult struct {
	Impl  provider.Provider
	Kind  string
	Model string
}

// Provider 按配置装配 LLM provider（配置 → provider 的唯一入口，两个薄壳
// demo/host 共用，与 Compressor 同纪律）：
//
//   - "deepseek"：DeepSeek 官方端点特化适配器（思考等级 / 流式 toolcall
//     分片细节 / 官方 finish_reason，见 plugins/provider/deepseek 包注释）；
//   - "openai"（默认，空 = openai）：OpenAI 兼容端点通用适配器
//     （DeepSeek/GLM/Kimi/Ollama 等同构端点也走它，只是没有上述特化）；
//   - 未知名字报错而不是静默退化——配置里写了不存在的 kind，应该显式失败。
//
// apiKey 非空优先（flag）；否则读 cfg.Provider.APIKeyEnv 指的环境变量，再兜底
// kind 对应的默认变量（openai=OPENAI_API_KEY / deepseek=DEEPSEEK_API_KEY）。
// key 本体永远不落配置文件。
func Provider(cfg config.Config, apiKey string) (ProviderResult, error) {
	kind := cfg.Provider.Kind
	if kind == "" {
		kind = "openai"
	}
	key := apiKey
	if key == "" && cfg.Provider.APIKeyEnv != "" {
		key = os.Getenv(cfg.Provider.APIKeyEnv)
	}
	envHint := orDefaultStr(cfg.Provider.APIKeyEnv, "")

	switch kind {
	case "deepseek":
		if key == "" {
			key = os.Getenv("DEEPSEEK_API_KEY")
		}
		if key == "" {
			return ProviderResult{}, fmt.Errorf("provider %q 缺少 API key：设环境变量 %s 或由宿主显式传入",
				kind, orDefaultStr(envHint, "DEEPSEEK_API_KEY"))
		}
		model := cfg.Provider.Model
		if model == "" {
			model = deepseek.DefaultModel // 稳定默认：V4.1-Flash（选型依据见插件包注释）
		}
		return ProviderResult{
			Impl: deepseek.New(deepseek.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, Model: cfg.Provider.Model,
				ContextWindow: cfg.Provider.ContextWindow, MaxOutput: cfg.Provider.MaxOutput}),
			Kind:  kind,
			Model: model,
		}, nil
	case "openai":
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return ProviderResult{}, fmt.Errorf("provider %q 缺少 API key：设环境变量 %s 或由宿主显式传入",
				kind, orDefaultStr(envHint, "OPENAI_API_KEY"))
		}
		if cfg.Provider.Model == "" {
			return ProviderResult{}, errors.New("provider \"openai\" 需要 provider.model（或换 kind = \"deepseek\" 吃默认模型）")
		}
		return ProviderResult{
			Impl: openai.New(openai.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, Model: cfg.Provider.Model,
				ContextWindow: cfg.Provider.ContextWindow, MaxOutput: cfg.Provider.MaxOutput}),
			Kind:  kind,
			Model: cfg.Provider.Model,
		}, nil
	default:
		return ProviderResult{}, fmt.Errorf("未知 provider kind %q（可选：%v）", kind, ProviderNames())
	}
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
