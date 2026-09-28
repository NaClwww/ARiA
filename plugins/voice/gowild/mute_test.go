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

// muteStub 充 backend：记录 /asr/mute 收到的 muted 序列，code 可切 404。
type muteStub struct {
	mu    sync.Mutex
	calls []bool
	code  int
}

func (s *muteStub) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Muted bool `json:"muted"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls = append(s.calls, body.Muted)
	code := s.code
	s.mu.Unlock()
	if code == http.StatusNotFound {
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"muted": body.Muted})
}

func (s *muteStub) snapshot() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.calls...)
}

func (s *muteStub) setCode(code int) {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
}

// waitCalls 轮询等到凑满 n 条下发（桥是异步的，等比断好）。
func (s *muteStub) waitCalls(t *testing.T, n int) []bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := s.snapshot(); len(calls) >= n {
			return calls
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等 %d 条 mute 下发超时，现有 %v", n, s.snapshot())
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMicMuteSetIdempotent(t *testing.T) {
	st := &muteStub{}
	srv := httptest.NewServer(http.HandlerFunc(st.handle))
	defer srv.Close()

	m := NewMicMute(srv.URL, testLogger())
	if err := m.Set(true); err != nil {
		t.Fatalf("Set(true): %v", err)
	}
	if err := m.Set(true); err != nil { // 与最近成功值相同：不再发
		t.Fatalf("Set(true) 重复: %v", err)
	}
	if err := m.Set(false); err != nil {
		t.Fatalf("Set(false): %v", err)
	}
	if calls := st.waitCalls(t, 2); len(calls) != 2 {
		t.Fatalf("幂等失效，下发 %v", calls)
	}
}

func TestMicMute404Disables(t *testing.T) {
	st := &muteStub{code: http.StatusNotFound}
	srv := httptest.NewServer(http.HandlerFunc(st.handle))
	defer srv.Close()

	m := NewMicMute(srv.URL, testLogger())
	if err := m.Set(true); err != nil {
		t.Fatalf("Set(true): %v", err)
	}
	st.setCode(http.StatusOK)
	if err := m.Set(false); err != nil { // 已判旧版：静默跳过
		t.Fatalf("Set(false) after 404: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if calls := st.snapshot(); len(calls) != 1 {
		t.Fatalf("404 后应永久停用，实际下发 %v", calls)
	}
}

func TestFollowGateBasicTransitions(t *testing.T) {
	st := &muteStub{}
	srv := httptest.NewServer(http.HandlerFunc(st.handle))
	defer srv.Close()

	gate := NewGate()
	cancel := FollowGate(gate, NewMicMute(srv.URL, testLogger()), 20*time.Millisecond)
	defer cancel()

	// 启动时闸门闲：补一发开耳（崩溃残留修复）
	if calls := st.waitCalls(t, 1); calls[0] {
		t.Fatalf("启动应为开耳修复，得到 %v", calls)
	}
	gate.Acquire()
	if calls := st.waitCalls(t, 2); !calls[1] {
		t.Fatalf("忙应闭耳，序列 %v", calls)
	}
	gate.Release()
	if calls := st.waitCalls(t, 3); calls[2] {
		t.Fatalf("闲（过 hold）应开耳，序列 %v", calls)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := st.snapshot(); len(calls) != 3 {
		t.Fatalf("稳态不应有额外下发，序列 %v", calls)
	}
}

func TestFollowGateHoldSwallowsChurn(t *testing.T) {
	st := &muteStub{}
	srv := httptest.NewServer(http.HandlerFunc(st.handle))
	defer srv.Close()

	gate := NewGate()
	hold := 250 * time.Millisecond
	cancel := FollowGate(gate, NewMicMute(srv.URL, testLogger()), hold)
	defer cancel()

	st.waitCalls(t, 1) // 启动开耳修复
	gate.Acquire()
	st.waitCalls(t, 2) // 闭耳

	// 闲→忙在 hold 内折返：开耳被作废，不该出现下发
	gate.Release()
	time.Sleep(hold / 3)
	gate.Acquire()
	time.Sleep(hold) // 足够触发一次错误的开耳（若实现有此缺陷）
	if calls := st.snapshot(); len(calls) != 2 {
		t.Fatalf("hold 内折返不应开耳，序列 %v", calls)
	}
	// 真正闲下来：过 hold 后开耳
	gate.Release()
	if calls := st.waitCalls(t, 3); calls[2] {
		t.Fatalf("稳闲后应开耳，序列 %v", calls)
	}
}
