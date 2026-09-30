package gowild

import (
	"errors"
	"log/slog"
)

// InputSink 是引擎投递口，宿主实现（06 §2 的多插头纪律归宿主：Queue 轮间
// 注入、ErrNoActiveRun 则 Input 起轮并阻塞到结算、说话人标签与模型参数）。
// 窄接口定义在消费方（本组件），agent.Session 经宿主适配后结构化满足。
type InputSink interface {
	Deliver(text, speaker string) error
}

// InputConfig 是半双工输入纪律的装配参数。
type InputConfig struct {
	Sink         InputSink // 引擎投递口。必填。
	Gate         GateState // 轮次忙碌状态（供状态灯使用）。必填。
	PlaybackGate GateState // 非空时只在实际播放期间拦截输入。
	// Bypass 关闭输入拦截（--no-input-gate）；宿主使用 PlaybackGate 时仅拦播放。
	// 轮次份额仍照持（闸门同时是状态灯的信号源）。
	Bypass bool
	// OnIgnored 是「正在说话，忽略输入」的提示回调（渲染归宿主）。可空。
	OnIgnored func(text string)
}

// Input 是半双工输入纪律：闸门激活期间的输入直接丢弃（自回声防护），
// PlaybackGate 可将输入拦截与忙碌状态分开。放行的输入持轮次份额到结算，
// 与 TTS 的生成份额、播放
// 份额同池计数，灯的 thinking 态由此覆盖整轮。
type Input struct {
	cfg InputConfig
	log *slog.Logger
}

func NewInput(cfg InputConfig, log *slog.Logger) (*Input, error) {
	if cfg.Sink == nil {
		return nil, errors.New("gowild.NewInput: Sink 必填（引擎投递口，宿主实现）")
	}
	if cfg.Gate == nil {
		return nil, errors.New("gowild.NewInput: Gate 必填（半双工闸门）")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Input{cfg: cfg, log: log}, nil
}

// Deliver 是所有输入插头（ASR、stdin、……）的统一入口。
func (in *Input) Deliver(text, speaker string) {
	block := in.cfg.PlaybackGate
	if block == nil {
		block = in.cfg.Gate
	}
	if !in.cfg.Bypass && block.Active() {
		if in.cfg.OnIgnored != nil {
			in.cfg.OnIgnored(text)
		}
		return
	}
	in.cfg.Gate.Acquire() // 轮次份额：从输入被接收持到本轮结算
	defer in.cfg.Gate.Release()
	if err := in.cfg.Sink.Deliver(text, speaker); err != nil {
		in.log.Error("input failed", "err", err)
	}
}
