package gowild

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeMic 持续吐假 PCM 的音箱 mic 流；写完挂着等连接关闭。
func fakeMic(t *testing.T, chunk []byte, interval time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeGateway 假 asr-gateway：升级 WS 后收 binary 计数、text 记入 cmds；
// script 在独立 goroutine 推事件，push 供测试随时向当前连接补事件。
type fakeGateway struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	bytes  int
	cmds   []string
	script func(conn *websocket.Conn)
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	if f.script != nil {
		go f.script(conn)
	}
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		if mt == websocket.TextMessage {
			f.cmds = append(f.cmds, string(data))
		} else {
			f.bytes += len(data)
		}
		f.mu.Unlock()
	}
}

func (f *fakeGateway) push(v any) error {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		return nil // 连接未建：测试时序外，忽略
	}
	return conn.WriteJSON(v)
}

func (f *fakeGateway) audioBytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytes
}

func (f *fakeGateway) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

// 转发 + 交付：mic 块原样进 binary 帧，speech_start 报信号，final 按
// 认主字段交付（matched 用 id，未匹配回落默认，空 text 不成轮）。
func TestGatewayForwardsAndDelivers(t *testing.T) {
	mic := fakeMic(t, make([]byte, 2048), 20*time.Millisecond)
	fg := &fakeGateway{
		script: func(conn *websocket.Conn) {
			conn.WriteJSON(map[string]any{"type": "speech_start", "utt": 1})
			conn.WriteJSON(map[string]any{"type": "final", "text": " 你好呀 ", "utt": 1,
				"speaker_id": "nacl", "speaker_status": "matched"})
			conn.WriteJSON(map[string]any{"type": "final", "text": "那算了", "utt": 2,
				"speaker_id": "x", "speaker_status": "unknown"})
			conn.WriteJSON(map[string]any{"type": "final", "text": "", "utt": 3}) // 空 final：不成轮
			conn.WriteJSON(map[string]any{"type": "error", "message": "noise"})   // 未知事件：忽略
		},
	}
	gwSrv := httptest.NewServer(fg)
	t.Cleanup(gwSrv.Close)

	var mu sync.Mutex
	var finals []string
	speech := 0
	gw, err := NewGateway(GatewayConfig{
		Base:          gwSrv.URL, // http:// 前缀：测规范化
		MicURL:        mic.URL + "/api/voice/mic/stream",
		Speaker:       "user",
		OnFinal:       func(text, speaker string) { mu.Lock(); finals = append(finals, speaker+":"+text); mu.Unlock() },
		OnSpeechStart: func() { mu.Lock(); speech++; mu.Unlock() },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); gw.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(finals)
		mu.Unlock()
		if n == 2 && fg.audioBytes() > 4096 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(finals) != 2 || finals[0] != "nacl:你好呀" || finals[1] != "user:那算了" {
		t.Fatalf("final 交付不符：%v", finals)
	}
	if speech != 1 {
		t.Fatalf("speech_start 信号数不符：%d", speech)
	}
	if fg.audioBytes() == 0 {
		t.Fatal("mic 音频没有转发到网关")
	}
}

// 闭耳：SetMuted(true) 发 reset + 停转（音频不再增长），mute 后网关推的
// final 客户端丢弃；SetMuted(false) 恢复转发。幂等：重复 Set 不再发。
func TestGatewayMute(t *testing.T) {
	mic := fakeMic(t, make([]byte, 2048), 10*time.Millisecond)
	fg := &fakeGateway{}
	gwSrv := httptest.NewServer(fg)
	t.Cleanup(gwSrv.Close)

	var mu sync.Mutex
	finals := 0
	gw, err := NewGateway(GatewayConfig{
		Base:    "ws://" + strings.TrimPrefix(gwSrv.URL, "http://"),
		MicURL:  mic.URL + "/api/voice/mic/stream",
		Speaker: "user",
		OnFinal: func(string, string) { mu.Lock(); finals++; mu.Unlock() },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); gw.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fg.audioBytes() < 8192 {
		time.Sleep(10 * time.Millisecond)
	}
	if fg.audioBytes() == 0 {
		t.Fatal("转发未就绪")
	}

	if err := gw.Set(true); err != nil {
		t.Fatal(err)
	}
	gw.Set(true) // 幂等：第二次不再发 reset
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(fg.commands()) < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	cmds := fg.commands()
	if len(cmds) != 1 || cmds[0] != `{"cmd":"reset"}` {
		t.Fatalf("应恰发一条 reset，got %v", cmds)
	}
	frozen := fg.audioBytes()
	time.Sleep(200 * time.Millisecond) // mic 还在吐，但闭耳期不应再转发
	if got := fg.audioBytes(); got != frozen {
		t.Fatalf("闭耳期仍在转发：%d → %d", frozen, got)
	}

	// 闭耳状态下网关残留 final：客户端丢弃
	if err := fg.push(map[string]any{"type": "final", "text": "自回声残留", "utt": 9}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	delivered := finals
	mu.Unlock()
	if delivered != 0 {
		t.Fatalf("闭耳期不应交付 final，got %d", delivered)
	}

	// 开耳恢复转发
	if err := gw.Set(false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fg.audioBytes() <= frozen {
		time.Sleep(10 * time.Millisecond)
	}
	if fg.audioBytes() <= frozen {
		t.Fatal("开耳后未恢复转发")
	}
}

func TestGatewayConfigValidation(t *testing.T) {
	for _, cfg := range []GatewayConfig{
		{},
		{Base: "ws://x", MicURL: "http://m", Speaker: "u"},                     // 缺 OnFinal
		{Base: "ws://x", MicURL: "http://m", OnFinal: func(string, string) {}}, // 缺 Speaker
		{Base: "ws://x", Speaker: "u", OnFinal: func(string, string) {}},       // 缺 MicURL
		{MicURL: "http://m", Speaker: "u", OnFinal: func(string, string) {}},   // 缺 Base
	} {
		if _, err := NewGateway(cfg, nil); err == nil {
			t.Errorf("cfg=%+v 应报装配错误", cfg)
		}
	}
	// 合法配置 + http:// 前缀规范化 + 尾斜杠
	g, err := NewGateway(GatewayConfig{Base: "http://h:1/", MicURL: "http://m", Speaker: "u",
		OnFinal: func(string, string) {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.cfg.Base != "ws://h:1" {
		t.Fatalf("http→ws 规范化不符：%q", g.cfg.Base)
	}
}
