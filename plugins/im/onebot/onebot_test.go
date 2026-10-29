package onebot_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aria/pkg/message"
	"aria/plugins/im/onebot"
	"aria/plugins/im/onebot/onebottest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// start 起假服务端与客户端，等到连上且拿到 SelfID。
func start(t *testing.T, token, clientToken string) (*onebottest.Server, *onebot.Client) {
	t.Helper()
	srv := onebottest.New(42, token)
	t.Cleanup(srv.Close)
	c, err := onebot.New(onebot.Config{URL: srv.URL(), Token: clientToken, ActionTimeout: 2 * time.Second,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return srv, c
}

func waitSelf(t *testing.T, c *onebot.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.SelfID() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("SelfID not set")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nextEvent(t *testing.T, c *onebot.Client) onebot.Event {
	t.Helper()
	select {
	case ev := <-c.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return onebot.Event{}
	}
}

// 连上后取 SelfID；事件解码（私聊 / 群聊 / 消息段）；send_* 按 echo 拿到消息 ID。
func TestClientEventsAndSend(t *testing.T) {
	srv, c := start(t, "tok", "tok")
	waitSelf(t, c)
	if c.SelfID() != 42 {
		t.Fatalf("SelfID = %d", c.SelfID())
	}

	mid := srv.PushPrivate(7, "小明", onebot.Text("你好"))
	ev := nextEvent(t, c)
	if ev.MessageType != "private" || ev.UserID != 7 || ev.MessageID != mid || ev.Sender.Name() != "小明" ||
		onebot.PlainText(ev.Segments) != "你好" {
		t.Fatalf("private event = %+v", ev)
	}

	srv.PushGroup(100, 8, "群名片", onebot.Reply(5), onebot.At(42), onebot.Text(" 在吗"))
	ev = nextEvent(t, c)
	if ev.MessageType != "group" || ev.GroupID != 100 || ev.Sender.Name() != "群名片" ||
		!onebot.Mentions(ev.Segments, 42) || onebot.Mentions(ev.Segments, 43) || onebot.ReplyTo(ev.Segments) != 5 {
		t.Fatalf("group event = %+v", ev)
	}

	got, err := c.SendGroup(context.Background(), 100, onebot.TextSegments("好的"))
	if err != nil || got == 0 {
		t.Fatalf("SendGroup = %d, %v", got, err)
	}
	for a := range srv.Actions() {
		if a.Action == "send_group_msg" {
			if a.MessageID != got || onebot.PlainText(a.Message()) != "好的" {
				t.Fatalf("action = %+v, want id %d", a, got)
			}
			break
		}
	}

	_, err = c.Call(context.Background(), "nope", nil)
	var ae *onebot.ActionError
	if !errors.As(err, &ae) || ae.Retcode != 1404 {
		t.Fatalf("unknown action err = %v", err)
	}
}

// token 不对：握手失败、不交付任何东西；Call 返回 ErrNotConnected。
func TestClientBadToken(t *testing.T) {
	_, c := start(t, "tok", "wrong")
	time.Sleep(50 * time.Millisecond)
	if _, err := c.Call(context.Background(), "get_login_info", nil); !errors.Is(err, onebot.ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

// 服务端断开连接后客户端重连，事件照常收。
func TestClientReconnect(t *testing.T) {
	srv, c := start(t, "", "")
	<-srv.Connected()
	srv.Drop()
	select {
	case <-srv.Connected():
	case <-time.After(2 * time.Second):
		t.Fatal("no reconnect")
	}
	// 重连后连接已在服务端登记（Connected 在登记之后发出），推送必达。
	srv.PushPrivate(7, "a", onebot.Text("again"))
	if ev := nextEvent(t, c); onebot.PlainText(ev.Segments) != "again" {
		t.Fatalf("event after reconnect = %+v", ev)
	}
}

// 字符串格式（CQ 码）按纯文本降级；非 message 事件（心跳）不交付。
func TestClientStringFormatAndMeta(t *testing.T) {
	srv, c := start(t, "", "")
	<-srv.Connected()
	srv.Push(map[string]any{"post_type": "meta_event", "meta_event_type": "heartbeat", "self_id": 42})
	srv.Push(map[string]any{"post_type": "message", "message_type": "private", "user_id": "7",
		"message_id": 1, "self_id": 42, "message": "[CQ:face,id=1]hi"})
	ev := nextEvent(t, c)
	if ev.UserID != 7 || onebot.PlainText(ev.Segments) != "[CQ:face,id=1]hi" {
		t.Fatalf("event = %+v", ev)
	}
}

// 等响应期间连接断开：在途调用立即以 ErrDisconnected 结束（不等超时）。
func TestCallFailsOnDisconnect(t *testing.T) {
	// 一个只收不回的服务端。
	up := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgraderFor().Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(up)
		_, _, _ = c.ReadMessage() // get_login_info
		_, _, _ = c.ReadMessage() // 测试的 Call
		_ = c.Close()
	}))
	defer srv.Close()
	c, _ := onebot.New(onebot.Config{URL: "ws" + srv.URL[4:], ActionTimeout: 5 * time.Second, MinBackoff: time.Hour}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-up
	var err error
	for {
		_, err = c.Call(context.Background(), "x", nil)
		if !errors.Is(err, onebot.ErrNotConnected) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !errors.Is(err, onebot.ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
}

func TestToBlocks(t *testing.T) {
	segs := []onebot.Segment{
		onebot.Reply(1), onebot.Text("看"), onebot.At(5), {Type: "face", Data: map[string]any{"id": "1"}},
		{Type: "image", Data: map[string]any{"url": "http://img/1"}}, onebot.Text("这个"),
		{Type: "image", Data: map[string]any{"url": "http://img/bad"}}, {Type: "record"},
	}
	fetch := func(url string) ([]byte, string, error) {
		if url == "http://img/bad" {
			return nil, "", errors.New("404")
		}
		return []byte{1}, "image/png", nil
	}
	got := onebot.ToBlocks(segs, fetch)
	if len(got) != 3 {
		t.Fatalf("blocks = %#v", got)
	}
	if b, ok := got[0].(message.TextBlock); !ok || b.Text != "看@5[表情]" {
		t.Fatalf("block0 = %#v", got[0])
	}
	if b, ok := got[1].(message.ImageBlock); !ok || b.MIME != "image/png" {
		t.Fatalf("block1 = %#v", got[1])
	}
	if b, ok := got[2].(message.TextBlock); !ok || b.Text != "这个[图片][record]" {
		t.Fatalf("block2 = %#v", got[2])
	}
	if got := onebot.ToBlocks(segs[4:5], nil); got[0].(message.TextBlock).Text != "[图片]" {
		t.Fatalf("nil fetch = %#v", got)
	}
}

// Segment.Str / Int 兼容字符串、json.Number 与 float64。
func TestSegmentAccessors(t *testing.T) {
	var s onebot.Segment
	if err := json.Unmarshal([]byte(`{"type":"at","data":{"qq":123456789012}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Int("qq") != 123456789012 {
		t.Fatalf("Int = %d", s.Int("qq"))
	}
	s.Data["qq"] = json.Number("5")
	if s.Str("qq") != "5" || s.Str("missing") != "" {
		t.Fatalf("Str = %q", s.Str("qq"))
	}
}

func TestHTTPImageFetcher(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(png)
		case "/big":
			_, _ = w.Write(make([]byte, 100))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := onebot.HTTPImageFetcher(nil, time.Second, 50)
	if data, mime, err := f(srv.URL + "/ok"); err != nil || mime != "image/png" || len(data) != len(png) {
		t.Fatalf("ok = %d %q %v", len(data), mime, err)
	}
	if _, _, err := f(srv.URL + "/big"); err == nil {
		t.Fatal("want size error")
	}
	if _, _, err := f(srv.URL + "/404"); err == nil {
		t.Fatal("want 404 error")
	}
}

func upgraderFor() *websocket.Upgrader { return &websocket.Upgrader{} }
