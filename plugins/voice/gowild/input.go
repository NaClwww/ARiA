package gowild

import (
	"errors"
	"log/slog"
	"sync"
)

// InputSink 是引擎投递口，宿主实现（06 §2 的多插头纪律归宿主：说话人标签与模型参数在宿主侧）。
// 窄接口定义在消费方（本组件），agent.Session 经宿主适配后结构化满足。
type InputSink interface {
	// Inject 把输入注入在途轮次（轮间注入），不阻塞。无在途轮次或本轮已判定收敛时
	// 返回 (false, nil)；会话已关闭等错误返回 err。
	Inject(text, speaker string) (bool, error)
	// Start 以该输入发起新一轮，阻塞到本轮结算。
	Start(text, speaker string) error
}

// InputConfig 是半双工输入纪律的装配参数。
type InputConfig struct {
	Sink         InputSink // 引擎投递口。必填。
	Gate         GateState // 轮次忙碌状态（供状态灯使用）。必填。
	PlaybackGate GateState // 非空时只在实际播放期间拦截输入。
	// Bypass 关闭输入拦截（--no-input-gate）；宿主使用 PlaybackGate 时仅拦播放。
	// 轮次份额照常持有（Gate 同时是状态灯的信号源）。
	Bypass bool
	// OnIgnored 是「正在说话，忽略输入」的提示回调（渲染归宿主）。可空。
	OnIgnored func(text string)
}

// Input 是半双工输入纪律与投递顺序的唯一入口：拦截状态激活期间的输入直接丢弃（自回声防护），
// PlaybackGate 可将输入拦截与忙碌状态分开。放行的输入持轮次份额，直到注入完成或本轮结算，
// 与 TTS 的生成份额、播放份额同池计数。
//
// 投递不阻塞插头：在途轮次可注入时经 Inject 注入；否则由投递 goroutine 依次 Start 起轮。
// 待发队列非空时新输入一律排队，注入与起轮的顺序与到达顺序一致。
type Input struct {
	cfg InputConfig
	log *slog.Logger

	mu      sync.Mutex
	running bool        // 投递 goroutine 存在（正在执行 Start 或处理待发队列）
	pending []utterance // 无法注入、等待起轮的输入，按到达顺序
}

// utterance 是一条已放行、待投递的输入。
type utterance struct{ text, speaker string }

func NewInput(cfg InputConfig, log *slog.Logger) (*Input, error) {
	if cfg.Sink == nil {
		return nil, errors.New("gowild.NewInput: Sink 必填（引擎投递口，宿主实现）")
	}
	if cfg.Gate == nil {
		return nil, errors.New("gowild.NewInput: Gate 必填（半双工输入的忙碌状态）")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Input{cfg: cfg, log: log}, nil
}

// Deliver 是所有输入插头（ASR、stdin、……）的统一入口，立即返回：读取事件流的
// goroutine 不随本轮生成与播放阻塞，生成期间的语音得以作为插话注入。
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
	in.cfg.Gate.Acquire() // 轮次份额：注入完成或本轮结算时归还

	in.mu.Lock()
	// 待发队列为空时先尝试注入；队列非空时直接排队，以保证不越过更早到达的输入。
	// Inject 不阻塞，持 in.mu 调用以与 drive 的出队判定串行。
	if len(in.pending) == 0 {
		ok, err := in.cfg.Sink.Inject(text, speaker)
		if ok || err != nil {
			in.mu.Unlock()
			in.cfg.Gate.Release()
			if err != nil {
				in.log.Error("input inject failed", "err", err)
			}
			return
		}
	}
	u := utterance{text: text, speaker: speaker}
	if in.running {
		in.pending = append(in.pending, u)
		in.mu.Unlock()
		return
	}
	in.running = true
	in.mu.Unlock()
	go in.drive(u)
}

// drive 依次以输入起轮，直到待发队列为空；每条输入的轮次份额在其 Start 返回后归还。
func (in *Input) drive(u utterance) {
	for {
		if err := in.cfg.Sink.Start(u.text, u.speaker); err != nil {
			in.log.Error("input failed", "err", err)
		}
		in.cfg.Gate.Release()

		in.mu.Lock()
		if len(in.pending) == 0 {
			in.running = false
			in.mu.Unlock()
			return
		}
		u = in.pending[0]
		in.pending = in.pending[1:]
		in.mu.Unlock()
	}
}
