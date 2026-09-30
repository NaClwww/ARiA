// speak.go 是出声的决策型工具（docs/03：工具是 agent 的行动边界）：模型
// 显式调 speak(text) 把一段话送进 TTS 的 FIFO 播报队列——把「说」变成
// 行动序列里的一步，说与做在飞轮里串行，模型也拿到说话的主动权（先安抚
// 再执行 / 分段讲长内容 / 可沉默干活）。
//
// 队列语义下段与段无缝接续：队二在队首放音期间就提前合成。block 参数
// 只决定等多深：block=true（默认）等到本段进入播放/预取位（背压深度 1，
// 模型自然流水线化）；block=false 纯入队立即返回。插话（用户消息/新一轮）
// 作废整条队列。
package gowild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aria/core/tool"
	"aria/pkg/message"
)

// ErrInterrupted 表示 speak 被打断（工具 ctx 取消：用户插话/转向）。
// 与链路失败相区分：打断是正常对话事件，失败才需要模型补救。
var ErrInterrupted = errors.New("speak: interrupted")

// MinSpeakToolTimeoutMS 是 speak 工具模式下 [limits] tool_timeout_ms 的
// 下限：一段话的合成+播放可长达几十秒，30s 默认值会把长段拦腰掐断
// （表现为音频停、工具报错、模型困惑地另起一段）。宿主装配时负责抬到
// 本下限。
const MinSpeakToolTimeoutMS = 120000

// SpeakToolInstruction 追加到 system prompt（speak 工具模式）：把「什么
// 时候会有语音输出」讲清楚——正文栏用户听不到，出声的唯一途径是 speak
// 工具；以及口语化纪律：念给人听的话要像说话，不像文章。
const SpeakToolInstruction = `

【语音输出】用户在听，不在读。这台设备上你只有调 speak 工具说出的话用户才听得到——直接写在回复正文里的内容，用户一个字都听不到。什么时候出声：
- 用户需要听到的都走 speak：回答、追问、安抚、汇报结果、征得确认；
- 安静干活是允许的：查资料、跑命令、翻文件的中间过程不必解说，做完用 speak 汇报结果就好；
- 别把要紧的话只写在正文里——那等于没对用户说。

speak 的内容要口语化（是念给人听的，不是文章）：
- 像平时聊天那样说：短句、大白话，一句一口气，不说长难句；
- 不用列表、标题、加粗、代码块、emoji、括号注释——朗读出来要么奇怪要么没意义；
- 数字、单位、符号都换成口语：「大概三分钟」不要「180s」，「九成」不要「90%」；
- 每次只说一小段（一两句），长内容拆成多次调用、按顺序说完。

speak 不是进度播报：
- 搜索、查文件、跑命令的中间过程一律不 speak——用户不需要听你干活的声音；
- 对同一画面/同一事物的观察只说一次，细节别翻来覆去修正；
- 查不到就直说「没查到」然后 stop，不要换个说法无限重试；没有新结论就没有可说的。

节奏：speak 默认阻塞到放完才返回——说完一段要继续说或做事，等它返回再动；只有一句快速插报、说完立刻继续干活时才用 block=false（不要连发多段非阻塞）。

收尾：该说的都说完、没有别的要做时，调用 stop 结束本轮、把话筒交还用户——不要再写一段用户听不到的正文当结尾。任务做不下去（连续失败、查无结果、没有进展）也算说完了：直说结果然后 stop。stop 必须是你调用序列的最后一步（它之后排的任何调用都不会被执行）；同一问题只作答一次，说完就收，不要换个说法再答一遍。用户插话会自动优先，转去回答插话后再 stop 即可。`

// NewSpeakTool 把出声工具接到 TTS 管线上（宿主装配：gate/TTS 先于 agent
// 就位，工具表在 agent.New 时就要交出去）。
func NewSpeakTool(t *TTS) tool.Tool { return &speakTool{t: t} }

// NewStopTool 是收尾工具（speak 模式标配）：模型显式结束本轮 run，把话筒
// 交还用户——说完最后一段 speak 后直接收尾，不再写一段用户听不到的正文，
// 也省一轮 provider 调用。实现 core/tool.Stopper：loop 见成功执行即收敛。
func NewStopTool() tool.Tool { return stopTool{} }

type stopTool struct{}

func (stopTool) Def() tool.Def {
	return tool.Def{
		Name: "stop",
		Description: "结束本轮、把话筒交还用户。该说的都已通过 speak 说完、也不需要再调用其他工具时立即调用本工具收尾，且必须作为最后一步——之后的调用不会被执行。" +
			"不调用而直接输出正文结束也可以，但那段正文用户听不到、等于白写。用户插话优先：插话到来时本轮会继续，转去回答后再调本工具。",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (stopTool) Exec(_ context.Context, call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID,
		Blocks: []message.Block{message.TextBlock{Text: "本轮已收尾"}}}
}

// StopsLoop 实现 core/tool.Stopper。
func (stopTool) StopsLoop() bool { return true }

type speakTool struct{ t *TTS }

func (s *speakTool) Def() tool.Def {
	return tool.Def{
		Name: "speak",
		Description: "把 text 朗读给用户听，这是用户听到你声音的唯一途径。内容要口语化：短句大白话、像聊天，" +
			"不要 markdown/列表/emoji。每次只说一小段（一两句），长内容分多次调用按顺序说，队列会无缝接上；不调 speak 的正文用户听不到。" +
			"block=true（默认）等到本段进入播放管线（紧跟前一段无缝播出）；block=false 入队立即返回。",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"text":{"type":"string","description":"要说的话：一两句口语化纯文本，念出来自然，不带任何标记"},` +
			`"block":{"type":"boolean","description":"是否等到进入播放管线，默认 true"}},"required":["text"]}`),
	}
}

func (s *speakTool) Exec(ctx context.Context, call tool.Call) tool.Result {
	var args struct {
		Text  string `json:"text"`
		Block *bool  `json:"block"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil || strings.TrimSpace(args.Text) == "" {
		return tool.Result{CallID: call.ID, IsError: true,
			Blocks: []message.Block{message.TextBlock{Text: "参数无效：需要非空 text"}}}
	}
	block := args.Block == nil || *args.Block // 缺省 = 等管线位（分支本意：说与做串行）

	if !block {
		// 纯入队即回。闸门份额照常由播放 goroutine 持有到放完（含结尾
		// 段的回声静默窗），半双工纪律不因提前返回而松动。
		s.t.EnqueueSpeak(args.Text)
		return tool.Result{CallID: call.ID,
			Blocks: []message.Block{message.TextBlock{Text: "已入队：排在前面的话后面按序无缝播出"}}}
	}

	err := s.t.Speak(ctx, args.Text)
	switch {
	case err == nil:
		return tool.Result{CallID: call.ID,
			Blocks: []message.Block{message.TextBlock{Text: "已进入播放管线（前面的话说完就无缝接上）"}}}
	case errors.Is(err, ErrInterrupted):
		// A3 约定：取消即错误结果喂回模型——模型据此知道刚才那段没说完
		return tool.Result{CallID: call.ID, IsError: true,
			Blocks: []message.Block{message.TextBlock{Text: "已打断：放音被中断，这段没说完"}}}
	default:
		return tool.Result{CallID: call.ID, IsError: true,
			Blocks: []message.Block{message.TextBlock{Text: fmt.Sprintf("TTS 失败：%v", err)}}}
	}
}
