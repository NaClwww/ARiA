package main

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

// lightRec 记录测试服务器收到的每次 POST（体 + 次数）。
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

func mustLight(t *testing.T, base string) *lightDriver {
	t.Helper()
	l, err := newLightDriver(base, "202020,00a000,2050ff", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func assertLastPost(t *testing.T, rec *lightRec, wantRGB [3]int) {
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

func TestParseLightColors(t *testing.T) {
	c, err := parseLightColors("#202020, 00a000,2050FF")
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
		if _, err := parseLightColors(bad); err == nil {
			t.Errorf("parseLightColors(%q) 应报错", bad)
		}
	}
}

func TestLightStateTransitions(t *testing.T) {
	srv, rec := newLightServer(t, http.StatusOK)
	l := mustLight(t, srv.URL)
	gate := &speakingGate{}

	// 首拍：无任何信号 → 待机（暗白）。applied 构造为 -1，不能被跳过。
	l.evaluate(gate)
	assertLastPost(t, rec, [3]int{0x20, 0x20, 0x20})

	// 状态未变不重发
	l.evaluate(gate)
	if n := len(rec.posts()); n != 1 {
		t.Fatalf("状态未变不应重发，共 %d 次", n)
	}

	// partial → 收听（绿）
	l.partial()
	l.evaluate(gate)
	assertLastPost(t, rec, [3]int{0x00, 0xa0, 0x00})

	// 闸门激活 → 思考（蓝）优先于 partial
	gate.acquire()
	l.evaluate(gate)
	assertLastPost(t, rec, [3]int{0x20, 0x50, 0xff})

	// 闸门释放但 partial 仍在窗口内 → 收听
	gate.release()
	l.evaluate(gate)
	assertLastPost(t, rec, [3]int{0x00, 0xa0, 0x00})

	// partial 过期 → 待机
	l.mu.Lock()
	l.lastPartial = time.Now().Add(-2 * partialWindow)
	l.mu.Unlock()
	l.evaluate(gate)
	assertLastPost(t, rec, [3]int{0x20, 0x20, 0x20})
}

func TestLightFailureBackoff(t *testing.T) {
	srv, rec := newLightServer(t, http.StatusInternalServerError)
	l := mustLight(t, srv.URL)
	gate := &speakingGate{}

	l.evaluate(gate) // 失败：不得标记 applied
	if n := len(rec.posts()); n != 1 {
		t.Fatalf("首拍应尝试一次，共 %d 次", n)
	}
	if l.applied == lightIdle {
		t.Fatal("失败不应标记 applied")
	}
	l.evaluate(gate) // 退避期内同目标不重试
	if n := len(rec.posts()); n != 1 {
		t.Fatalf("退避期内不应重试，共 %d 次", n)
	}
	gate.acquire() // 目标变了：立即尝试新目标
	l.evaluate(gate)
	if n := len(rec.posts()); n != 2 {
		t.Fatalf("新目标应尝试，共 %d 次", n)
	}
	if l.applied == lightThinking {
		t.Fatal("失败不应标记 applied")
	}
}
