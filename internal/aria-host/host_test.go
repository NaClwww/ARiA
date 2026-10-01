package ariahost

import (
	"strings"
	"testing"

	"aria/internal/config"
)

func TestTruncStr(t *testing.T) {
	if got := TruncStr("短句", 10); got != "短句" {
		t.Errorf("短句不应截断：%q", got)
	}
	// 按 rune 截断：5 个汉字截到 3 个 + 省略号
	long := strings.Repeat("汉", 5)
	if got := TruncStr(long, 3); got != "汉汉汉…" {
		t.Errorf("rune 截断不符：%q", got)
	}
}

func TestSpeakerInstruction(t *testing.T) {
	inst := SpeakerInstruction("user")
	if !strings.Contains(inst, "[user]") {
		t.Fatalf("声明应含默认说话人：%q", inst)
	}
}

// llmOptions：[llm] 缺省（0）= 不下发该字段——temperature 绝不能以 0
// 值显式下发（那等于贪心解码）；显式配置才进 Options。
func TestLLMOptionsZeroMeansAbsent(t *testing.T) {
	var cfg config.Config
	cfg.Provider.Model = "m"

	off := llmOptions(cfg)
	if off.Model != "m" || off.Temperature != nil || off.MaxTokens != 0 {
		t.Fatalf("缺省应不下发温度/上限：%+v", off)
	}

	cfg.LLM.Temperature, cfg.LLM.MaxTokens = 0.7, 2048
	on := llmOptions(cfg)
	if on.Temperature == nil || *on.Temperature != 0.7 || on.MaxTokens != 2048 {
		t.Fatalf("显式配置必须生效：%+v", on)
	}
}
