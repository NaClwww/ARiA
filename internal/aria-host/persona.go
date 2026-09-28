package ariahost

import (
	"fmt"
	"os"

	"aria/internal/config"
	"aria/runtime/window"
)

// ResolvePersona 取人设：文件优先，其次内联，空则用内置默认（Gowild-HE
// 陪伴助手 + 标签策略说明）。
func ResolvePersona(p config.Persona) (string, error) {
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
