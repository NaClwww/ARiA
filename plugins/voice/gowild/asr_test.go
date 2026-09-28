package gowild

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// SSE 帧序列：keepalive、空 text 的 partial、有效 partial、final、噪音行。
func TestASRDeliversFinalsAndPartials(t *testing.T) {
	var mu sync.Mutex
	var finals []string
	partials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": keepalive\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"partial\",\"text\":\"你\"}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"partial\",\"text\":\"\"}\n\n") // 空 partial：不报
		fl.Flush()
		fmt.Fprint(w, "not-json\n\n") // 噪音行：丢弃不断流
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\" 你好呀 \"}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\"\"}\n\n") // 空 final：不成轮
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

	asr, err := NewASR(ASRConfig{
		Base:    srv.URL,
		Speaker: "user",
		OnFinal: func(text, speaker string) {
			mu.Lock()
			finals = append(finals, speaker+":"+text)
			mu.Unlock()
		},
		OnPartial: func() {
			mu.Lock()
			partials++
			mu.Unlock()
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		asr.Run(ctx) // 服务器写完保持连接挂着；靠 cancel 收
	}()
	defer cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(finals)
		p := partials
		mu.Unlock()
		if got == 1 && p == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(finals) != 1 || finals[0] != "user:你好呀" {
		t.Fatalf("final 交付不符：%v", finals)
	}
	if partials != 1 {
		t.Fatalf("partial 信号数不符：%d（空 partial 不应报）", partials)
	}
}

// 认主解析：matched 用 backend 的 id；未匹配/空 id/旧版缺字段回落默认。
func TestASRSpeakerResolution(t *testing.T) {
	var mu sync.Mutex
	var finals []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\"你好\",\"speaker_id\":\"nacl\",\"speaker_status\":\"matched\",\"speaker_score\":0.78}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\"那算了\",\"speaker_id\":\"someone\",\"speaker_status\":\"unmatched\"}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\" matched 空 id 也回落 \",\"speaker_id\":\" \",\"speaker_status\":\"matched\"}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"type\":\"final\",\"text\":\"旧版没有认主字段\"}\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

	asr, err := NewASR(ASRConfig{
		Base:    srv.URL,
		Speaker: "user",
		OnFinal: func(text, speaker string) {
			mu.Lock()
			finals = append(finals, speaker+":"+text)
			mu.Unlock()
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		asr.Run(ctx)
	}()
	defer cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(finals)
		mu.Unlock()
		if got == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []string{"nacl:你好", "user:那算了", "user:matched 空 id 也回落", "user:旧版没有认主字段"}
	if len(finals) != 4 || finals[0] != want[0] || finals[1] != want[1] || finals[2] != want[2] || finals[3] != want[3] {
		t.Fatalf("认主解析不符：\n got %v\nwant %v", finals, want)
	}
}

func TestASRConfigValidation(t *testing.T) {
	for _, cfg := range []ASRConfig{
		{},
		{Base: "http://x", Speaker: "user"}, // 缺 OnFinal
		{Base: "http://x", OnFinal: func(string, string) {}}, // 缺 Speaker
		{Speaker: "user", OnFinal: func(string, string) {}},  // 缺 Base
	} {
		if _, err := NewASR(cfg, nil); err == nil {
			t.Errorf("cfg=%+v 应报装配错误", cfg)
		}
	}
}
