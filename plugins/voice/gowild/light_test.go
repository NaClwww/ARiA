package gowild

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type lightRec struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func (r *lightRec) posts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func newLightServer(t *testing.T, status int) (*httptest.Server, *lightRec) {
	t.Helper()
	rec := &lightRec{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/light" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(req.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, string(b))
		st := rec.status
		rec.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newTestLight(t *testing.T, base string) (*Light, GateState) {
	t.Helper()
	g := NewGate()
	l, err := NewLight(LightConfig{Base: base, Colors: "202020,00a000,2050ff"},
		g, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.SettleIdle)
	return l, g
}

func assertLightPost(t *testing.T, rec *lightRec, wantRGB [3]int) {
	t.Helper()
	posts := rec.posts()
	if len(posts) == 0 {
		t.Fatalf("期待一次 POST，实际 0 次")
	}
	var body struct {
		Mode string `json:"mode"`
		RGB  []int  `json:"rgb"`
	}
	if err := json.Unmarshal([]byte(posts[len(posts)-1]), &body); err != nil {
		t.Fatalf("解析 POST 体失败：%v（%q）", err, posts[len(posts)-1])
	}
	if body.Mode != "color" || len(body.RGB) != 3 ||
		body.RGB[0] != wantRGB[0] || body.RGB[1] != wantRGB[1] || body.RGB[2] != wantRGB[2] {
		t.Fatalf("POST 体不符：mode=%s rgb=%v，想要 color %v", body.Mode, body.RGB, wantRGB)
	}
}

func waitLightState(t *testing.T, rec *lightRec, wantRGB [3]int) {
	t.Helper()
	waitFor(t, "灯态 "+(string)(rune('0'+wantRGB[0])), func() bool {
		posts := rec.posts()
		if len(posts) == 0 {
			return false
		}
		var body struct {
			RGB []int `json:"rgb"`
		}
		if json.Unmarshal([]byte(posts[len(posts)-1]), &body) != nil || len(body.RGB) != 3 {
			return false
		}
		return body.RGB[0] == wantRGB[0] && body.RGB[1] == wantRGB[1] && body.RGB[2] == wantRGB[2]
	})
}

func TestParseLightColors(t *testing.T) {
	c, err := ParseLightColors("#202020, 00a000,2050FF")
	if err != nil {
		t.Fatal(err)
	}
	want := [3][3]int{{0x20, 0x20, 0x20}, {0x00, 0xa0, 0x00}, {0x20, 0x50, 0xff}}
	if c != want {
		t.Fatalf("got %v want %v", c, want)
	}
	for _, bad := range []string{
		"", "202020", "202020,00a000", "202020,00a000,2050ff,ffffff",
		"202020,zz0000,2050ff", "2020200,00a000,2050ff",
	} {
		if _, err := ParseLightColors(bad); err == nil {
			t.Errorf("ParseLightColors(%q) 应报错", bad)
		}
	}
}

// 闸门迁移只在跨越 0↔n 边界时通知；退订后不再收。
func TestGateTransitionsNotify(t *testing.T) {
	g := NewGate()
	var mu sync.Mutex
	var events []bool // 每次回调时读取的 Active()
	cancel := g.Observe(func() {
		mu.Lock()
		events = append(events, g.Active())
		mu.Unlock()
	})

	g.Acquire() // 0→1：通知
	g.Acquire() // 1→2：不通知
	g.Release() // 2→1：不通知
	g.Release() // 1→0：通知
	g.Release() // 计数已为 0：不通知
	cancel()
	g.Acquire() // 已退订：即便 0→1 也不通知
	g.Release()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || !events[0] || events[1] {
		t.Fatalf("迁移通知不符：%v（想要 [true false]）", events)
	}
}

func TestLightStateTransitions(t *testing.T) {
	srv, rec := newLightServer(t, http.StatusOK)
	light, gate := newTestLight(t, srv.URL)

	waitLightState(t, rec, [3]int{0x20, 0x20, 0x20}) // 初始：待机暗白

	light.Heartbeat() // mic 听到人声 → 收听绿（迁移即求值，不等轮询周期）
	waitLightState(t, rec, [3]int{0x00, 0xa0, 0x00})

	gate.Acquire() // 她开始忙 → 思考蓝（优先于心跳）
	waitLightState(t, rec, [3]int{0x20, 0x50, 0xff})

	gate.Release() // 闸门放行，但心跳仍在窗口内 → 收听绿
	waitLightState(t, rec, [3]int{0x00, 0xa0, 0x00})

	// 心跳过期 → 待机（白盒回拨心跳 + 补一枚求值信号；生产路径由衰减
	// 定时器触发同一入口）
	light.mu.Lock()
	light.heartbeat = time.Now().Add(-2 * partialWindow)
	light.mu.Unlock()
	light.signal()
	waitLightState(t, rec, [3]int{0x20, 0x20, 0x20})
}

func TestLightFailureBackoff(t *testing.T) {
	srv, rec := newLightServer(t, http.StatusInternalServerError)
	light, gate := newTestLight(t, srv.URL)

	waitFor(t, "首拍尝试（将失败）", func() bool { return len(rec.posts()) > 0 })

	light.signal() // 退避期内同目标重发被抑制
	time.Sleep(400 * time.Millisecond)
	if n := len(rec.posts()); n != 1 {
		t.Fatalf("退避期内不应重试，共 %d 次", n)
	}

	gate.Acquire() // 目标变了：立即尝试新目标（思考蓝）
	waitFor(t, "新目标应尝试", func() bool { return len(rec.posts()) >= 2 })
	if n := len(rec.posts()); n != 2 {
		t.Fatalf("新目标只应多一次尝试，共 %d 次", n)
	}
}
