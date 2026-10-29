// Package onebottest 是进程内的假 OneBot 11 WS 服务端（扮演 NapCat）：测试与宿主 --fake
// 冒烟共用。它接受客户端连接、按 action 回应答（get_login_info / send_private_msg /
// send_group_msg），把收到的 action 交给 Actions，并可向全部连接推送事件。
package onebottest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aria/plugins/im/onebot"
)

// Action 是客户端发来的一次 action 调用。
type Action struct {
	Action string
	Params json.RawMessage
	Echo   json.RawMessage
	// MessageID 是 send_* 应答里分配的消息 ID（其余 action 为 0）。
	MessageID int64
}

// Message 解出 send_* 的 message 参数。
func (a Action) Message() []onebot.Segment {
	var p struct {
		Message []onebot.Segment `json:"message"`
	}
	_ = json.Unmarshal(a.Params, &p)
	return p.Message
}

// Server 是假 OneBot 服务端。Token 非空时校验 Authorization: Bearer <Token>。
type Server struct {
	SelfID int64
	Token  string

	srv     *httptest.Server
	actions chan Action
	conns   chan struct{} // 每建立一条连接发一个信号

	mu     sync.Mutex
	live   map[*websocket.Conn]*sync.Mutex // 连接 → 写锁
	nextID int64
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// New 起一个假服务端（selfID 是机器人 QQ 号）。用完调 Close。
func New(selfID int64, token string) *Server {
	s := &Server{SelfID: selfID, Token: token, actions: make(chan Action, 256),
		conns: make(chan struct{}, 16), live: map[*websocket.Conn]*sync.Mutex{}, nextID: 1000}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL 是 ws:// 地址。
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") }

// Actions 是收到的 action 流（缓冲满时丢弃最新的）。
func (s *Server) Actions() <-chan Action { return s.actions }

// Connected 每建立一条连接产生一个信号。
func (s *Server) Connected() <-chan struct{} { return s.conns }

// Close 断开全部连接并停止服务端。
func (s *Server) Close() {
	s.Drop()
	s.srv.Close()
}

// Drop 断开当前全部连接（客户端应重连）。
func (s *Server) Drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.live {
		_ = c.Close()
		delete(s.live, c)
	}
}

// Push 把 v（事件对象）编码后推给全部连接。
func (s *Server) Push(v any) {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, wmu := range s.live {
		wmu.Lock()
		_ = c.WriteMessage(websocket.TextMessage, b)
		wmu.Unlock()
	}
}

// PushPrivate 推一条私聊消息事件，返回其消息 ID。
func (s *Server) PushPrivate(userID int64, nickname string, segs ...onebot.Segment) int64 {
	mid := s.allocID()
	s.Push(map[string]any{
		"time": time.Now().Unix(), "self_id": s.SelfID, "post_type": "message",
		"message_type": "private", "sub_type": "friend", "message_id": mid, "user_id": userID,
		"message": segs, "raw_message": onebot.PlainText(segs),
		"sender": map[string]any{"user_id": userID, "nickname": nickname},
	})
	return mid
}

// PushGroup 推一条群消息事件，返回其消息 ID。
func (s *Server) PushGroup(groupID, userID int64, card string, segs ...onebot.Segment) int64 {
	mid := s.allocID()
	s.Push(map[string]any{
		"time": time.Now().Unix(), "self_id": s.SelfID, "post_type": "message",
		"message_type": "group", "sub_type": "normal", "message_id": mid, "user_id": userID,
		"group_id": groupID, "message": segs, "raw_message": onebot.PlainText(segs),
		"sender": map[string]any{"user_id": userID, "nickname": card, "card": card, "role": "member"},
	})
	return mid
}

func (s *Server) allocID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	wmu := &sync.Mutex{}
	s.mu.Lock()
	s.live[c] = wmu
	s.mu.Unlock()
	select {
	case s.conns <- struct{}{}:
	default:
	}
	defer func() {
		s.mu.Lock()
		delete(s.live, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			return
		}
		var a struct {
			Action string          `json:"action"`
			Params json.RawMessage `json:"params"`
			Echo   json.RawMessage `json:"echo"`
		}
		if json.Unmarshal(b, &a) != nil {
			continue
		}
		act := Action{Action: a.Action, Params: a.Params, Echo: a.Echo}
		resp := map[string]any{"status": "ok", "retcode": 0, "echo": a.Echo}
		switch a.Action {
		case "get_login_info":
			resp["data"] = map[string]any{"user_id": s.SelfID, "nickname": "aria"}
		case "send_private_msg", "send_group_msg":
			act.MessageID = s.allocID()
			resp["data"] = map[string]any{"message_id": act.MessageID}
		default:
			resp = map[string]any{"status": "failed", "retcode": 1404, "wording": "不支持的 action " + strconv.Quote(a.Action), "echo": a.Echo}
		}
		select {
		case s.actions <- act:
		default:
		}
		out, _ := json.Marshal(resp)
		wmu.Lock()
		_ = c.WriteMessage(websocket.TextMessage, out)
		wmu.Unlock()
	}
}
