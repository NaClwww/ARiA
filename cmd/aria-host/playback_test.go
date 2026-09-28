package main

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 回归：播放闸门份额从「TTS 请求发出」即持有——正文生成完到音频开始
// 之间的合成排队期（RVC 忙时以十秒计），灯与输入闸门不允许掉下去。

func wavHeader(t *testing.T, rate, channels, dataLen int) []byte {
	t.Helper()
	h := make([]byte, wavHeaderLen)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+dataLen))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(h[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(h[34:36], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataLen))
	return h
}

// gateSink 只为闸门测试服务：排空即放行（drained 预先关闭）。
type gateSink struct {
	mu      sync.Mutex
	written int
	ended   bool
}

func (f *gateSink) begin() error { return nil }
func (f *gateSink) write(p []byte) error {
	f.mu.Lock()
	f.written += len(p)
	f.mu.Unlock()
	return nil
}
func (f *gateSink) end() error {
	f.mu.Lock()
	f.ended = true
	f.mu.Unlock()
	return nil
}
func (f *gateSink) stop()      {}
func (f *gateSink) waitDrain() {}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestGateHeldThroughSynthesisQueue(t *testing.T) {
	pcm := make([]byte, 3200)
	body := append(wavHeader(t, 16000, 1, len(pcm)), pcm...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(400 * time.Millisecond) // 模拟合成排队：音频迟到
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	sink := &gateSink{}
	gate := &speakingGate{}
	d := &ttsDriver{
		base: srv.URL, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		hc: &http.Client{}, gate: gate,
		newSink: func(context.Context, int, int) audioSink { return sink },
	}

	p, err := startPlayback(d.base, d.hc, d.log, d.newSink, gate)
	if err != nil {
		t.Fatal(err)
	}
	p.feed("一句话")
	p.finishInput()

	// 关键断言：请求已发出、音频还没来（400ms 排队中），闸门必须已持有。
	time.Sleep(100 * time.Millisecond)
	if !gate.active() {
		t.Fatal("合成排队期闸门未持有（灯会提前掉回待机）")
	}

	// 音频送达并排空后正常放行。
	waitFor(t, "播放完成", func() bool { return !gate.active() })
	sink.mu.Lock()
	written, ended := sink.written, sink.ended
	sink.mu.Unlock()
	if written == 0 || !ended {
		t.Fatalf("播放轨迹不符：written=%d ended=%v", written, ended)
	}
}
