package assemble

import (
	"fmt"
	"log/slog"

	"aria/internal/config"
	"aria/plugins/memory/hindsight"
	"aria/runtime/memory"
)

// MemoryNames 返回可用记忆服务名（错误提示用）。
func MemoryNames() []string { return []string{"hindsight"} }

// Memory 按配置装配记忆服务（docs/memory/options.md「测试期后端」）：
//
//   - ""：不接入，返回 nil（agent 不调用记忆服务、不提取要点）；
//   - "hindsight"：plugins/memory/hindsight（Hindsight 为检索引擎，暂存与会话级提交在进程内）；
//   - 未知名字报错。
func Memory(cfg config.Memory, log *slog.Logger) (memory.Service, error) {
	switch cfg.Engine {
	case "":
		return nil, nil
	case "hindsight":
		svc, err := hindsight.New(hindsight.Config{BaseURL: cfg.BaseURL, Dir: cfg.Dir, Members: cfg.Members}, log)
		if err != nil {
			return nil, err
		}
		return svc, nil
	default:
		return nil, fmt.Errorf("未知记忆服务 %q（可选：%v）", cfg.Engine, MemoryNames())
	}
}
