// Package assemble 是宿主侧的「名字 → 零件」装配表（03 §5、06 §3）。
//
// 引擎只认接口（Compressor / Tool / Provider…），不认识任何配置里的名字；
// 把名字翻成实现是宿主的活——所以这张表放在薄壳共用的 internal 下，
// 由 aria-demo / aria-web 各自装配时调用。新增一个压缩策略 = 这里加一个分支，
// 引擎一行不动。
package assemble

import (
	"fmt"

	"aria/core/provider"
	"aria/internal/config"
	"aria/runtime/agent"
	"aria/runtime/window"
)

// CompressorNames 返回可用压缩策略名（网页面板下拉框与错误提示用，顺序即推荐顺序）。
func CompressorNames() []string { return []string{"provider", "keeplast"} }

// Compressor 按配置装配压缩策略。
//
//   - "provider"（默认）：每次压缩调一次 LLM，把「旧记忆 + 本轮」压成摘要；
//   - "keeplast"：不调 LLM，只保留最近 N 条（N = compress.keep_last_n，0 用引擎默认）；
//   - 未知名字报错而不是静默退化——配置里写了不存在的策略，应该是显式失败。
func Compressor(cfg config.Config, prov provider.Provider) (window.Compressor, error) {
	switch name := cfg.Compress.Strategy; name {
	case "", "provider":
		if prov == nil {
			return nil, fmt.Errorf("压缩策略 %q 需要 Provider", "provider")
		}
		return &window.ProviderCompressor{
			Provider:    prov,
			Model:       cfg.Compress.Model,
			Instruction: cfg.Compress.Instruction,
		}, nil
	case "keeplast":
		n := cfg.Compress.KeepLastN
		if n <= 0 {
			n = window.DefaultKeepLast
		}
		return window.KeepLast(n), nil
	default:
		return nil, fmt.Errorf("未知压缩策略 %q（可选：%v）", name, CompressorNames())
	}
}

// CompactBudget 把配置的压缩预留策略翻成 agent.CompactBudget（两个宿主的 agent.Config
// 与配置重载共用）。
func CompactBudget(c config.Compress) agent.CompactBudget {
	return agent.CompactBudget{
		ReserveRatio:      c.ReserveRatio,
		TurnReserveTokens: c.TurnReserveTokens,
		ConcurrentTurns:   c.ConcurrentTurns,
	}
}
