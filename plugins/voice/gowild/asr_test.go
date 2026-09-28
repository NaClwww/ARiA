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
