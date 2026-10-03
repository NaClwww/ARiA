package assemble

import (
	"context"
	"fmt"
	"log/slog"

	"aria/core/tool"
	"aria/internal/config"
	"aria/plugins/memory/hindsight"
	"aria/runtime/memory"
)

// MemoryNames 返回可用记忆服务名（错误提示用）。
func MemoryNames() []string { return []string{"hindsight"} }

// MemoryResult 是记忆服务的装配结果（两个宿主共用）：Service 为 nil 表示未接入；Tool 是 memory_recall 工具；
// Close 等待实现的后台提交结束，实现不提供时为 nil。
type MemoryResult struct {
	Service memory.Service
	Tool    tool.Tool
	Close   func(context.Context) error
}

// Memory 按配置装配记忆服务（docs/memory/options.md「测试期后端」）：
//
//   - ""：不接入，Service 为 nil（agent 不调用记忆服务、不提取要点）；
//   - "hindsight"：plugins/memory/hindsight（Hindsight 为检索引擎，暂存与会话级提交在进程内）；
//   - 未知名字报错。
func Memory(cfg config.Memory, log *slog.Logger) (MemoryResult, error) {
	switch cfg.Engine {
	case "":
		return MemoryResult{}, nil
	case "hindsight":
		svc, err := hindsight.New(hindsight.Config{BaseURL: cfg.BaseURL, Dir: cfg.Dir, Members: cfg.Members}, log)
		if err != nil {
			return MemoryResult{}, err
		}
		return MemoryResult{Service: svc, Tool: memory.RecallTool(svc, log), Close: svc.Close}, nil
	default:
		return MemoryResult{}, fmt.Errorf("未知记忆服务 %q（可选：%v）", cfg.Engine, MemoryNames())
	}
}
