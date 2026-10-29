// Package onebot 是 OneBot 11 协议的 WebSocket 客户端（设计见 docs/discussions/2026-10-29-im-bot-napcat.md §7）：ARiA 作为客户端连上
// 实现方（NapCat / Lagrange / LLOneBot）的正向 WS 服务端，一条连接同时收事件、发 action。
// 本包不认识 ARiA 的 Session——路由、过滤、回发归宿主侧（见设计文档）。
//
//   - 连接：Run 拨号并在断线后指数退避重连；鉴权按 OneBot 11 走 Authorization: Bearer <token>。
//   - 事件：只交付 post_type = message（Events）；心跳、通知、自己发出的消息等丢弃。
//     要求实现方配置 messagePostFormat = array；收到字符串格式时按纯文本降级并告警一次。
//   - action：Call 带 echo，按 echo 匹配响应（带超时）；断线时在途调用立即失败。
//   - 自己的 QQ 号：每次连上后经 get_login_info 获取（SelfID），宿主据此判断 @。
package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 缺省值。
const (
	DefaultActionTimeout = 10 * time.Second
	DefaultMinBackoff    = time.Second
	DefaultMaxBackoff    = 30 * time.Second
	DefaultEventBuffer   = 256
)

var (
	// ErrNotConnected 表示当前没有连接（尚未连上或正在重连），action 未发出。
	ErrNotConnected = errors.New("onebot: not connected")
	// ErrDisconnected 表示 action 已发出但等到响应前连接断开（是否执行未知）。
	ErrDisconnected = errors.New("onebot: connection lost before response")
)

// ActionError 是实现方返回的失败响应（status = failed 或 retcode ≠ 0）。
type ActionError struct {
	Action  string
	Retcode int
	Message string
}

func (e *ActionError) Error() string {
	return fmt.Sprintf("onebot: %s failed: retcode=%d %s", e.Action, e.Retcode, e.Message)
}

type Config struct {
	URL   string // ws:// 或 wss:// 地址，如 ws://127.0.0.1:3001
	Token string // access_token；空 = 不鉴权

	ActionTimeout time.Duration // 单次 action 等响应的上限；0 → DefaultActionTimeout
	MinBackoff    time.Duration // 重连退避起点；0 → DefaultMinBackoff
	MaxBackoff    time.Duration // 重连退避上限；0 → DefaultMaxBackoff
	EventBuffer   int           // Events 通道缓冲；0 → DefaultEventBuffer。满时丢弃新事件并告警
}

// Client 是 OneBot 11 正向 WS 客户端。并发安全：Call / Send* 可从任意 goroutine 调用。
type Client struct {
	cfg    Config
	log    *slog.Logger
	dialer websocket.Dialer
	events chan Event
	selfID atomic.Int64
	seq    atomic.Uint64

	mu      sync.Mutex
	conn    *conn // 当前连接；nil = 未连接
	warnFmt sync.Once
}

// conn 是一次连接：写串行化（gorilla 只允许一个并发写者）+ 在途调用表。
type conn struct {
	ws      *websocket.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan response
	dead    bool
}

type response struct {
	Status  string          `json:"status"`
	Retcode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	err     error
}

// New 校验配置并建 Client（不拨号，连接由 Run 负责）。
func New(cfg Config, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return nil, fmt.Errorf("onebot: url 须为 ws:// 或 wss:// 地址：%q", cfg.URL)
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = DefaultActionTimeout
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = DefaultEventBuffer
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg: cfg, log: log,
		dialer: websocket.Dialer{HandshakeTimeout: 10 * time.Second},
		events: make(chan Event, cfg.EventBuffer),
	}, nil
}

// Events 是消息事件流；Run 返回后关闭。
func (c *Client) Events() <-chan Event { return c.events }

// SelfID 是机器人自己的 QQ 号（连上后由 get_login_info 或事件的 self_id 得到）；未知为 0。
func (c *Client) SelfID() int64 { return c.selfID.Load() }

// Run 保持连接直到 ctx 取消：拨号 → 收发 → 断线后指数退避重连（连上过一次则退避复位）。
// 只能调用一次；返回时关闭 Events。满足 app.Service 的契约。
func (c *Client) Run(ctx context.Context) {
	defer close(c.events)
	backoff := c.cfg.MinBackoff
	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = c.cfg.MinBackoff
		}
		c.log.Warn("onebot: 连接断开，稍后重连", "url", c.cfg.URL, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.cfg.MaxBackoff)
	}
}

// session 跑一次连接直到断开或 ctx 取消；connected 报告是否握手成功。
func (c *Client) session(ctx context.Context) (connected bool, err error) {
	hdr := http.Header{}
	if c.cfg.Token != "" {
		hdr.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	ws, resp, err := c.dialer.DialContext(ctx, c.cfg.URL, hdr)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return false, fmt.Errorf("鉴权失败（HTTP %d，检查 token）: %w", resp.StatusCode, err)
		}
		return false, err
	}
	cn := &conn{ws: ws, pending: map[string]chan response{}}
	c.mu.Lock()
	c.conn = cn
	c.mu.Unlock()
	c.log.Info("onebot: 已连接", "url", c.cfg.URL)

	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(cn) }()
	go c.fetchSelf(ctx)

	select {
	case err = <-readErr:
	case <-ctx.Done():
		_ = ws.Close()
		err = <-readErr
	}
	c.mu.Lock()
	if c.conn == cn {
		c.conn = nil
	}
	c.mu.Unlock()
	cn.fail()
	return true, err
}

// fetchSelf 连上后取自己的 QQ 号（失败只告警：事件里的 self_id 也会填上）。
func (c *Client) fetchSelf(ctx context.Context) {
	data, err := c.Call(ctx, "get_login_info", struct{}{})
	if err != nil {
		c.log.Warn("onebot: get_login_info 失败", "err", err)
		return
	}
	var info struct {
		UserID   id     `json:"user_id"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &info); err != nil || info.UserID == 0 {
		c.log.Warn("onebot: get_login_info 响应无法解析", "err", err, "data", string(data))
		return
	}
	c.selfID.Store(int64(info.UserID))
	c.log.Info("onebot: 登录账号", "self_id", int64(info.UserID), "nickname", info.Nickname)
}

// readLoop 读帧：带 echo 的是 action 响应，post_type = message 的交付 Events，其余丢弃。
func (c *Client) readLoop(cn *conn) error {
	for {
		mt, b, err := cn.ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var head struct {
			PostType string          `json:"post_type"`
			Echo     json.RawMessage `json:"echo"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			c.log.Warn("onebot: 无法解析的帧", "err", err)
			continue
		}
		switch {
		case head.PostType == "" && len(head.Echo) > 0:
			var r response
			if err := json.Unmarshal(b, &r); err != nil {
				c.log.Warn("onebot: 无法解析的响应", "err", err)
				continue
			}
			cn.deliver(echoKey(head.Echo), r)
		case head.PostType == "message":
			ev, err := decodeEvent(b)
			if errors.Is(err, errStringFormat) {
				c.warnFmt.Do(func() { c.log.Warn("onebot: " + err.Error() + "（已按纯文本降级，@ 与图片不可用）") })
			} else if err != nil {
				c.log.Warn("onebot: 消息事件解码失败", "err", err)
				continue
			}
			if ev.SelfID != 0 && c.selfID.Load() == 0 {
				c.selfID.Store(ev.SelfID)
			}
			select {
			case c.events <- ev:
			default:
				c.log.Warn("onebot: 事件通道已满，丢弃消息", "message_id", ev.MessageID)
			}
		}
	}
}

// echoKey 把 echo 规整为匹配键：字符串取其值，其余取原文。
func echoKey(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (cn *conn) deliver(echo string, r response) {
	cn.mu.Lock()
	ch, ok := cn.pending[echo]
	delete(cn.pending, echo)
	cn.mu.Unlock()
	if ok {
		ch <- r
	}
}

// fail 让全部在途调用以 ErrDisconnected 结束，之后的注册直接失败。
func (cn *conn) fail() {
	cn.mu.Lock()
	cn.dead = true
	pending := cn.pending
	cn.pending = nil
	cn.mu.Unlock()
	for _, ch := range pending {
		ch <- response{err: ErrDisconnected}
	}
	_ = cn.ws.Close()
}

// Call 调用一个 action 并等待响应，返回 data 原文。ctx 与 ActionTimeout 取先到者。
func (c *Client) Call(ctx context.Context, action string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn == nil {
		return nil, ErrNotConnected
	}
	echo := strconv.FormatUint(c.seq.Add(1), 10)
	b, err := json.Marshal(struct {
		Action string `json:"action"`
		Params any    `json:"params"`
		Echo   string `json:"echo"`
	}{action, params, echo})
	if err != nil {
		return nil, fmt.Errorf("onebot: encode %s: %w", action, err)
	}
	ch := make(chan response, 1)
	cn.mu.Lock()
	if cn.dead {
		cn.mu.Unlock()
		return nil, ErrNotConnected
	}
	cn.pending[echo] = ch
	cn.mu.Unlock()
	forget := func() {
		cn.mu.Lock()
		delete(cn.pending, echo)
		cn.mu.Unlock()
	}

	cn.wmu.Lock()
	_ = cn.ws.SetWriteDeadline(time.Now().Add(c.cfg.ActionTimeout))
	err = cn.ws.WriteMessage(websocket.TextMessage, b)
	cn.wmu.Unlock()
	if err != nil {
		forget()
		return nil, fmt.Errorf("onebot: send %s: %w", action, err)
	}

	timer := time.NewTimer(c.cfg.ActionTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.Status == "failed" || r.Retcode != 0 {
			msg := r.Wording
			if msg == "" {
				msg = r.Message
			}
			return nil, &ActionError{Action: action, Retcode: r.Retcode, Message: msg}
		}
		return r.Data, nil
	case <-timer.C:
		forget()
		return nil, fmt.Errorf("onebot: %s: no response within %v", action, c.cfg.ActionTimeout)
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

// SendPrivate 发私聊消息，返回消息 ID。
func (c *Client) SendPrivate(ctx context.Context, userID int64, segs []Segment) (int64, error) {
	return c.send(ctx, "send_private_msg", map[string]any{"user_id": userID, "message": segs})
}

// SendGroup 发群消息，返回消息 ID。
func (c *Client) SendGroup(ctx context.Context, groupID int64, segs []Segment) (int64, error) {
	return c.send(ctx, "send_group_msg", map[string]any{"group_id": groupID, "message": segs})
}

func (c *Client) send(ctx context.Context, action string, params map[string]any) (int64, error) {
	data, err := c.Call(ctx, action, params)
	if err != nil {
		return 0, err
	}
	var r struct {
		MessageID id `json:"message_id"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &r); err != nil {
			return 0, fmt.Errorf("onebot: %s response: %w", action, err)
		}
	}
	return int64(r.MessageID), nil
}
