// Package gowild 是 Gowild-HE 音箱的语音插件（docs/03 §1：VAD/ASR/驱动 =
// 插件层能力；docs/06 §2/§1：ASR 是输入插头、TTS 是事件流订阅者）：
//
//	Gate  —— 半双工闸门（引用计数 + 迁移订阅）：轮次/生成/播放三类份额
//	         同池计数，是「她正忙着说」这条总线的信号源
//	Input —— 半双工输入纪律：闸门激活期间丢弃输入（自回声防护），放行的
//	         输入持轮次份额到结算；引擎投递口（Queue→Input 多插头纪律）
//	         由宿主以 InputSink 窄接口实现
//	ASR  —— backend :8800 /asr/events SSE：partial 报「mic 听到人声」
//	        （宿主转投 Light.Heartbeat），type=final 交付「一句完整的话 +
//	        谁说的」（成轮判定在服务端 VAD，插头只收 final）
//	TTS  —— 事件流 → backend /tts/stream_input 流式合成 → 设备扬声器
//	        （launcher /api/voice/play* 三段式）或本机 paplay
//	Light —— 机身 RGB 指示灯（launcher /api/light 三色通道）：订阅闸门
//	        迁移与 partial 心跳，事件驱动求值 待机/收听/思考
//
// 装配纪律（docs/03 §1）：宿主在 Setup 处把零件接到一起（OnFinal →
// Input.Deliver、OnPartial → Light.Heartbeat、事件流 → TTS.Run），插件之间
// 互不调用；引擎（core/runtime）不认识本包。
package gowild

// Gate 是「正在说话」闸门的份额能力：TTS 从请求发出到设备放完按份额
// 持有，配合轮次份额实现半双工（说与听不并行，挡自回声）。
// 完整能力（状态 + 迁移订阅）见 GateState，宿主与 Light 消费。
type Gate interface {
	Acquire()
	Release()
}
