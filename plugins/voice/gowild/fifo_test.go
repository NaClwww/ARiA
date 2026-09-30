package gowild

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"aria/core/loop"
	"aria/pkg/message"
)

// ---------- FIFO 队列回归（2026-09-29） ----------
//
// 分段播报的空窗曾有三个来源：每段的「等首句合成」、段间的回声静默窗、
// radio 单飞对新段的敌意。队列 + 预取之后：段间只剩一次设备会话切换。

// seqSink 记录带时间戳的调用序列，用于断言接缝时序。
type seqSink struct {
	mu     sync.Mutex
	events []struct {
		at time.Time
		ev string
	}
}

func (s *seqSink) log(ev string) {
	s.mu.Lock()
	s.events = append(s.events, struct {
		at time.Time
		ev string
	}{time.Now(), ev})
	s.mu.Unlock()
}

func (s *seqSink) begin() error       { s.log("begin"); return nil }
func (s *seqSink) write([]byte) error { s.log("write"); return nil }
func (s *seqSink) end() error         { s.log("end"); return nil }
func (s *seqSink) stop()              { s.log("stop") }
func (s *seqSink) waitDrain()         { s.log("drain") }

func (s *seqSink) count(ev string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.ev == ev {
			n++
		}
	}
	return n
}

func (s *seqSink) first(ev string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.ev == ev {
			return e.at, true
		}
	}
	return time.Time{}, false
}

// 核心断言：两段话无缝接续——第二段的会话在第一段放完之前就已到达
// backend（预取），第一段放完的瞬间第二段直接上设备（不重新等合成、
// 不插回声静默窗）。
func TestSpeakFIFOPrefetchGapless(t *testing.T) {
	const synth = 150 * time.Millisecond // 模拟首句合成延迟
	const mute = 200 * time.Millisecond  // 段尾静默窗（若被错误执行，接缝 ≥350ms）

	gate := &testGate{}
	pcm := make([]byte, 3200)
	body := append(wavHeader(t, 16000, 1, len(pcm)), pcm...)

	var mu sync.Mutex
	var arrivals []time.Time
	var sinks []*seqSink
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(synth)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tts := &TTS{
		cfg:  TTSConfig{Base: srv.URL, EchoMute: mute},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc:   &http.Client{},
		gate: gate,
		newSink: func(context.Context, int, int) audioSink {
			s := &seqSink{}
			mu.Lock()
			sinks = append(sinks, s)
			mu.Unlock()
			return s
		},
	}

	if err := tts.Speak(context.Background(), "第一段"); err != nil {
		t.Fatal(err)
	}
	if err := tts.Speak(context.Background(), "第二段"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "两段放完", func() bool { return !gate.held() })

	mu.Lock()
	defer mu.Unlock()
	if len(sinks) != 2 {
		t.Fatalf("应有两条播放会话，得到 %d", len(sinks))
	}
	a, b := sinks[0], sinks[1]
	if a.count("write") == 0 || b.count("write") == 0 {
		t.Fatal("两段都必须真的出声")
	}
	aEnd, ok := a.first("end")
	if !ok {
		t.Fatal("第一段没有 end")
	}
	bBegin, ok := b.first("begin")
	if !ok {
		t.Fatal("第二段没有 begin")
	}
	// 顺序：第二段在第一段排空之后才上设备。
	if gap := bBegin.Sub(aEnd); gap < 0 || gap > 100*time.Millisecond {
		t.Fatalf("接缝 %v 过宽（预取未生效或静默窗被错误执行）", gap)
	}
	// 预取：第二段的请求在第一段放完之前就已到达 backend。
	if len(arrivals) < 2 {
		t.Fatalf("backend 应收到两个请求，得到 %d", len(arrivals))
	}
	if arrivals[1].After(aEnd) {
		t.Fatal("第二段请求晚于第一段放完（没有预取）")
	}
}

// 插话（用户消息注入）作废整条队列：闸门归还、挂着的会话被杀。
func TestSpeakQueueKilledByUserMessage(t *testing.T) {
	gate := &testGate{}
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	tts := &TTS{
		cfg:  TTSConfig{Base: srv.URL, EchoMute: 0},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc:   &http.Client{},
		gate: gate,
		newSink: func(context.Context, int, int) audioSink {
			return &seqSink{}
		},
	}
	ch := make(chan loop.Event, 4)
	go tts.Run(ch)

	tts.EnqueueSpeak("第一段") // 直放位：挂在等流头
	tts.EnqueueSpeak("第二段") // 预取位：同上
	waitFor(t, "队列占住闸门", gate.held)

	ch <- loop.Event{Kind: loop.KindUserMessageInjected,
		Data: loop.UserMessageInjectedData{Message: message.NewUser("插话")}}
	waitFor(t, "插话后闸门放行", func() bool { return !gate.held() })
}

func TestHoldSinkBuffersUntilRelease(t *testing.T) {
	inner := &seqSink{}
	h := newHoldSink()
	h.setFactory(func() audioSink { return inner })

	if err := h.begin(); err != nil {
		t.Fatal(err)
	}
	if err := h.write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if inner.count("begin") != 0 || inner.count("write") != 0 {
		t.Fatal("缓冲期真 sink 不应被触碰")
	}

	h.release()
	if inner.count("begin") != 1 || inner.count("write") == 0 {
		t.Fatal("release 后应建真 sink 并灌入缓冲")
	}
	if err := h.write([]byte("cd")); err != nil {
		t.Fatal(err)
	}
	if inner.count("write") < 2 {
		t.Fatal("release 后应直通")
	}
	if err := h.end(); err != nil {
		t.Fatal(err)
	}
	if inner.count("end") != 1 {
		t.Fatal("end 应直通")
	}
}

func TestHoldSinkStopDropsBuffer(t *testing.T) {
	inner := &seqSink{}
	h := newHoldSink()
	h.setFactory(func() audioSink { return inner })

	_ = h.write([]byte("ab"))
	h.stop()
	h.release() // stop 之后 release 是 no-op
	if inner.count("begin") != 0 {
		t.Fatal("stop 后不得再建真 sink")
	}
}
