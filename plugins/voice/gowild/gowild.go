// Package gowild 是 Gowild-HE 音箱的语音插件（docs/03 §1：VAD/ASR/驱动 =
// 插件层能力；docs/06 §2/§1：ASR 是输入插头、TTS 是事件流订阅者）：
//
//	ASR —— backend :8800 /asr/events SSE：partial 报「mic 听到人声」（宿主
//	       拿去驱灯），type=final 交付「一句完整的话 + 谁说的」（成轮判定
//	       在服务端 VAD，插头只收 final）
//	TTS —— 事件流 → backend /tts/stream_input 流式合成 → 设备扬声器
//	       （launcher /api/voice/play* 三段式）或本机 paplay
//
// 依赖注入遵循「能力接口定义在消费方」：本包消费宿主的半双工闸门能力，
// Gate 由宿主实现（结构化满足，零 import）；宿主消费本包的 ASR/TTS，
// 构造注入。引擎（core/runtime）不认识本包。
package gowild

// Gate 是宿主的「正在说话」闸门能力：TTS 从请求发出到设备放完按份额
// 持有，配合宿主的轮次份额实现半双工（说与听不并行，挡自回声）。
// 宿主的具体实现（如引用计数 speakingGate）结构化满足即可。
type Gate interface {
	Acquire()
	Release()
}
