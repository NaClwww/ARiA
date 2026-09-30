package gowild

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aria/core/tool"
)

// newSpeakTTS 装一个最小 TTS：fake backend 返回 WAV 头 + PCM，sink 即写即收。
func newSpeakTTS(t *testing.T, body []byte, delay time.Duration, gate *testGate, sink *gateSink) (*TTS, *httptest.Server) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	tts := &TTS{
		cfg:  TTSConfig{Base: srv.URL, EchoMute: 0},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc:   &http.Client{},
		gate: gate,
		newSink: func(context.Context, int, int) audioSink {
			return sink
		},
	}
	return tts, srv
}

func speakBody(t *testing.T) []byte {
	pcm := make([]byte, 3200)
	return append(wavHeader(t, 16000, 1, len(pcm)), pcm...)
}

func TestSpeakBlockingWaitsForPipelineSlot(t *testing.T) {
	gate, sink := &testGate{}, &gateSink{}
	tts, _ := newSpeakTTS(t, speakBody(t), 200*time.Millisecond, gate, sink)

	// 阻塞 = 等到进入播放管线位：合成还没出音频（200ms 延迟途中）就该返回。
	start := time.Now()
	if err := tts.Speak(context.Background(), "一段话"); err != nil {
		t.Fatalf("Speak 返回错误: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("Speak 等了 %v（应只等管线位，不等合成+播放）", elapsed)
	}
	waitFor(t, "播放完成且闸门放行", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.ended && !gate.held()
	})
	sink.mu.Lock()
	written := sink.written
	sink.mu.Unlock()
	if written == 0 {
		t.Fatal("播放轨迹不符")
	}
}

func TestSpeakNonBlockingReturnsImmediately(t *testing.T) {
	gate, sink := &testGate{}, &gateSink{}
	// 音频迟到 300ms：入队必须立即返回，闸门随即持有到放完。
	tts, _ := newSpeakTTS(t, speakBody(t), 300*time.Millisecond, gate, sink)

	start := time.Now()
	tts.EnqueueSpeak("插报一句")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("EnqueueSpeak 等了 %v（入队语义破坏）", elapsed)
	}
	waitFor(t, "闸门持有", gate.held)
	waitFor(t, "播放完成", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.ended && !gate.held()
	})
}

func TestSpeakInterrupted(t *testing.T) {
	gate, sink := &testGate{}, &gateSink{}
	// 音频永不到达：直放/预取两个会话都挂在等流头，第三段无管线位。
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	tts := &TTS{
		cfg: TTSConfig{Base: srv.URL, EchoMute: 0},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc:  &http.Client{}, gate: gate,
		newSink: func(context.Context, int, int) audioSink { return sink },
	}

	tts.EnqueueSpeak("第一段") // 直放位
	tts.EnqueueSpeak("第二段") // 预取位
	waitFor(t, "两个会话占住闸门", gate.held)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tts.Speak(ctx, "第三段") }() // 无空位：等
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != ErrInterrupted {
		t.Fatalf("打断应返回 ErrInterrupted，得到 %v", err)
	}
	// 打断作废整条队列：所有会话被杀，闸门份额归还，无悬账。
	waitFor(t, "打断后闸门放行", func() bool { return !gate.held() })
}

func TestSpeakEchoMuteWindow(t *testing.T) {
	gate, sink := &testGate{}, &gateSink{}
	tts := &TTS{cfg: TTSConfig{Base: "http://x", EchoMute: 300 * time.Millisecond},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc:  &http.Client{}, gate: gate,
		newSink: func(context.Context, int, int) audioSink { return sink }}

	pcm := make([]byte, 3200)
	body := append(wavHeader(t, 16000, 1, len(pcm)), pcm...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	tts.cfg.Base = srv.URL

	if err := tts.Speak(context.Background(), "一段"); err != nil {
		t.Fatal(err)
	}
	// Speak 只等管线位就返回；音频放完（sink.end）时静默窗才刚开始——
	// 闸门必须仍持有，300ms 后才放行。
	waitFor(t, "音频放完", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.ended
	})
	if !gate.held() {
		t.Fatal("静默窗期闸门未持有")
	}
	waitFor(t, "静默窗结束放行", func() bool { return !gate.held() })
}

// 两条语音声明是行为契约的一部分：模式不同「什么时候出声」答案不同，
// 关键句不许漂移（改文案可以，改语义要过这里的眼睛）。
func TestVoiceInstructionsContract(t *testing.T) {
	if !strings.Contains(SpeakToolInstruction, "听不到") || !strings.Contains(SpeakToolInstruction, "口语化") {
		t.Error("speak 模式声明必须讲清「正文听不到」与「口语化」")
	}
	if !strings.Contains(SpeakToolInstruction, "stop") {
		t.Error("speak 模式声明必须讲清 stop 收尾")
	}
	if !strings.Contains(VoiceRenderInstruction, "朗读") || !strings.Contains(VoiceRenderInstruction, "口语化") {
		t.Error("渲染模式声明必须讲清「正文会被朗读」与「口语化」")
	}
}

// stop 是收尾工具：实现 core/tool.Stopper（loop 见成功执行即收敛），
// Exec 恒成功、无参数要求。
func TestStopToolIsStopper(t *testing.T) {
	st := NewStopTool()
	s, ok := st.(tool.Stopper)
	if !ok || !s.StopsLoop() {
		t.Fatal("NewStopTool 必须实现 tool.Stopper")
	}
	res := st.Exec(context.Background(), tool.Call{ID: "c1", Args: json.RawMessage(`{}`)})
	if res.IsError || res.CallID != "c1" {
		t.Fatalf("stop Exec 应成功回带 CallID：%+v", res)
	}
}

func TestSpeakToolExecBlockModes(t *testing.T) {
	gate, sink := &testGate{}, &gateSink{}
	tts, _ := newSpeakTTS(t, speakBody(t), 0, gate, sink)
	st := NewSpeakTool(tts)

	// 非阻塞：立即返回成功，闸门随即持有、放完放行。
	res := st.Exec(context.Background(), tool.Call{ID: "c1",
		Args: json.RawMessage(`{"text":"hi","block":false}`)})
	if res.IsError {
		t.Fatalf("非阻塞 speak 失败: %+v", res)
	}
	waitFor(t, "入队后闸门持有", gate.held)
	waitFor(t, "播放完成", func() bool { return !gate.held() })

	// 阻塞（缺省）：等到管线位返回，播放随后自然放完。
	res = st.Exec(context.Background(), tool.Call{ID: "c2",
		Args: json.RawMessage(`{"text":"hi"}`)})
	if res.IsError {
		t.Fatalf("阻塞 speak 失败: %+v", res)
	}
	waitFor(t, "播放完成", func() bool { return !gate.held() })

	// 空 text 拒绝。
	res = st.Exec(context.Background(), tool.Call{ID: "c3",
		Args: json.RawMessage(`{"text":"  "}`)})
	if !res.IsError {
		t.Fatal("空 text 应报错")
	}
}
