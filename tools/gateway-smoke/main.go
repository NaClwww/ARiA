// E2E 冒烟：假音箱 mic 流(测试 wav 的 s16 PCM 循环)→ Gateway → 真实
// asr-gateway(EPYC VM)。验证 gorilla ↔ uvicorn WS 互通与事件交付时序。
// 用法: go run ./tools/gateway-smoke -gateway ws://172.16.53.179:8210 -wav a.wav
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	gowild "aria/plugins/voice/gowild"
)

func main() {
	gateway := flag.String("gateway", "ws://172.16.53.179:8210", "asr-gateway 根地址")
	wav := flag.String("wav", "", "16k mono wav(循环当 mic 输入)")
	flag.Parse()
	if *wav == "" {
		fmt.Fprintln(os.Stderr, "需要 -wav")
		os.Exit(2)
	}
	pcm, err := wavToPCM16(*wav)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	pcm = append(pcm, make([]byte, 16000*2*2)...) // 尾静音触发判停

	// 假音箱:mic 裸流按 200ms 块循环吐
	mux := http.NewServeMux()
	mux.HandleFunc("/api/voice/mic/stream", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		chunk := 16000 * 2 / 5 // 200ms s16
		for i := 0; i < len(pcm); i += chunk {
			end := min(i+chunk, len(pcm))
			if _, err := w.Write(pcm[i:end]); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond): // 真实节奏
			}
		}
		<-r.Context().Done() // 放完挂着
	})
	go func() { _ = http.ListenAndServe("127.0.0.1:18990", mux) }()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	gw, err := gowild.NewGateway(gowild.GatewayConfig{
		Base:    *gateway,
		MicURL:  "http://127.0.0.1:18990/api/voice/mic/stream",
		Speaker: "user",
		OnFinal: func(text, speaker string) {
			fmt.Printf("FINAL speaker=%s text=%s\n", speaker, text)
		},
		OnSpeechStart: func() { fmt.Println("SPEECH_START") },
	}, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		time.Sleep(2 * time.Second)
		fmt.Println("→ mute 1s(测 reset)")
		_ = gw.Set(true)
		time.Sleep(1 * time.Second)
		_ = gw.Set(false)
	}()
	gw.Run(ctx)
}

// wavToPCM16 解 44 字节标准 WAV 头取 data(冒烟用途,非通用解码)。
func wavToPCM16(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s: 不是标准 WAV", path)
	}
	// 找 data 块
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(data[off+4]) | int(data[off+5])<<8 | int(data[off+6])<<16 | int(data[off+7])<<24
		if id == "data" {
			end := min(off+8+size, len(data))
			return data[off+8 : end], nil
		}
		off += 8 + size + (size & 1) // 奇数块补齐
	}
	return nil, fmt.Errorf("%s: 无 data 块", path)
}
