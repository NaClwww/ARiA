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

// SpeakerInstruction 是随人设下发的说话人标注声明：宿主把每条用户消息
// 统一标成「[名字] 内容」（语音来自 backend 声纹认主，未认出回落默认名）。
// 没有这句，标记对模型只是无含义的括号——问「你知道我是谁吗」就答不上来。
func SpeakerInstruction(defaultUser string) string {
	return "用户消息开头的「[名字] 」标注是说话人：语音输入经声纹认主，认出即标真实名字；" +
		fmt.Sprintf("未认出标为默认说话人 [%s]。可据此称呼对方、分辨多个人说话。", defaultUser)
}
