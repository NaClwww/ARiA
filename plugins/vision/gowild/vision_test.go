package gowild

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aria/core/loop"
	"aria/pkg/message"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func userMsg(text string) message.Message {
	return message.Message{Role: message.RoleUser, Blocks: []message.Block{message.TextBlock{Text: text}}}
}

func lastImage(m message.Message) (message.ImageBlock, bool) {
	for _, b := range m.Blocks {
		if img, ok := b.(message.ImageBlock); ok {
			return img, true
		}
	}
	return message.ImageBlock{}, false
}

func TestInjectsFreshFrameAtTail(t *testing.T) {
	frame := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3} // JPEG 魔数开头即可
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(frame)
	}))
	defer srv.Close()

	v, err := New(Config{Base: srv.URL}, quietLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := loop.State{Messages: []message.Message{userMsg("早"), userMsg("你看这个")}}
	out := v.Assemble(context.Background(), state)

	if len(out) != 3 {
		t.Fatalf("应追加一条，得到 %d 条", len(out))
	}
	if out[0].Blocks[0].(message.TextBlock).Text != "早" || out[1].Blocks[0].(message.TextBlock).Text != "你看这个" {
		t.Fatal("原有消息被动过")
	}
	tail := out[2]
	if tail.Role != message.RoleUser {
		t.Fatalf("注入角色 %v，应为 user", tail.Role)
	}
	img, ok := lastImage(tail)
	if !ok {
		t.Fatal("注入消息没有 ImageBlock")
	}
	if string(img.Data) != string(frame) || img.MIME != "image/jpeg" {
		t.Fatal("注入的不是刚拉的帧")
	}
	if len(state.Messages) != 2 {
		t.Fatal("污染了调用方的 State（只读约定）")
	}
}

func TestFailureInjectsNothing(t *testing.T) {
	// 500：安静跳过
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	v, _ := New(Config{Base: srv.URL}, quietLog())
	state := loop.State{Messages: []message.Message{userMsg("hi")}}
	if out := v.Assemble(context.Background(), state); len(out) != 1 {
		t.Fatalf("失败应原样返回，得到 %d 条", len(out))
	}
	srv.Close()

	// 连不上：同样跳过
	v2, _ := New(Config{Base: srv.URL}, quietLog()) // srv 已关，地址仍在
	if out := v2.Assemble(context.Background(), state); len(out) != 1 {
		t.Fatalf("失联应原样返回，得到 %d 条", len(out))
	}
}

func TestTimeoutSkips(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done(): // 客户端超时取消也放行，别拖住 srv.Close
		}
	}))
	defer srv.Close()  // 后执行
	defer close(block) // 先执行：放行 handler（Close 会等在途请求结束）

	v, _ := New(Config{Base: srv.URL}, quietLog())
	v.timeout = 50 * time.Millisecond // 测试收紧
	state := loop.State{Messages: []message.Message{userMsg("hi")}}
	done := make(chan []message.Message, 1)
	go func() { done <- v.Assemble(context.Background(), state) }()
	select {
	case out := <-done:
		if len(out) != 1 {
			t.Fatalf("超时应原样返回，得到 %d 条", len(out))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Assemble 被挂起的抓取卡住（同步缝红线）")
	}
}

func TestBaseRequired(t *testing.T) {
	if _, err := New(Config{}, quietLog()); err == nil {
		t.Fatal("空 Base 应报错")
	}
}

// Reassembling replaces the camera slot without removing user images or
// modifying earlier requests. A failed refresh must not reuse a stale frame.
func TestVisualSlotRefreshAndFailure(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 3 {
			http.Error(w, "offline", 500)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{byte(count)})
	}))
	defer srv.Close()
	v, _ := New(Config{Base: srv.URL}, quietLog())
	original := userMsg("看看我上传的图片")
	original.Blocks = append(original.Blocks, message.ImageBlock{Data: []byte{9}, MIME: "image/jpeg"})
	first := v.Assemble(context.Background(), loop.State{Messages: []message.Message{original}})
	second := v.Assemble(context.Background(), loop.State{Messages: first})
	if len(second) != 2 || second[1].ID != visualSlotID {
		t.Fatal("camera slot accumulated")
	}
	a, _ := lastImage(first[1])
	b, _ := lastImage(second[1])
	u, _ := lastImage(second[0])
	if a.Data[0] != 1 || b.Data[0] != 2 || u.Data[0] != 9 {
		t.Fatal("refresh mutated history or user image")
	}
	if second[1].Blocks[0].(message.TextBlock).Text != visualSlotOpen+"\n" {
		t.Fatal("missing visual boundary")
	}
	failed := v.Assemble(context.Background(), loop.State{Messages: second})
	if len(failed) != 1 {
		t.Fatal("failed refresh retained stale camera image")
	}
}
