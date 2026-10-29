package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Event 是解码后的消息事件（post_type = message）。其余 post_type（meta_event 心跳、
// notice、request、自己发出的 message_sent）不交付。
type Event struct {
	Time        int64
	SelfID      int64
	MessageType string // private | group
	SubType     string // friend / group（群临时会话）/ normal …
	MessageID   int64
	UserID      int64
	GroupID     int64 // 私聊为 0
	Sender      Sender
	Segments    []Segment
	RawMessage  string
}

// Sender 是发送者信息（OneBot 11 sender 字段，各实现给出的字段不全，缺省为空）。
type Sender struct {
	UserID   int64
	Nickname string
	Card     string // 群名片；私聊为空
	Role     string // owner | admin | member（仅群聊）
}

// Name 是展示名：群名片优先，其次昵称，都为空时用 QQ 号。
func (s Sender) Name() string {
	switch {
	case s.Card != "":
		return s.Card
	case s.Nickname != "":
		return s.Nickname
	default:
		return strconv.FormatInt(s.UserID, 10)
	}
}

// Segment 是消息段（OneBot 11 数组格式）：text / at / image / reply / face …。
// Data 的值按协议是字符串，部分实现给数字；读取用 Str / Int。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Str 读 data 字段为字符串（数字按十进制格式化）；缺失为空串。
func (s Segment) Str(key string) string {
	switch v := s.Data[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// Int 读 data 字段为整数（如 at 的 qq、reply 的 id）；缺失或非数字为 0。
func (s Segment) Int(key string) int64 {
	n, _ := strconv.ParseInt(s.Str(key), 10, 64)
	return n
}

// id 是兼容数字与数字字符串两种写法的 ID 字段。
type id int64

func (i *id) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*i = 0
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("onebot: id %q: %w", b, err)
	}
	*i = id(n)
	return nil
}

type rawEvent struct {
	Time        int64           `json:"time"`
	SelfID      id              `json:"self_id"`
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	SubType     string          `json:"sub_type"`
	MessageID   id              `json:"message_id"`
	UserID      id              `json:"user_id"`
	GroupID     id              `json:"group_id"`
	Message     json.RawMessage `json:"message"`
	RawMessage  string          `json:"raw_message"`
	Sender      struct {
		UserID   id     `json:"user_id"`
		Nickname string `json:"nickname"`
		Card     string `json:"card"`
		Role     string `json:"role"`
	} `json:"sender"`
}

// errStringFormat 表示 message 字段是 CQ 码字符串（实现配了 messagePostFormat = string）。
var errStringFormat = fmt.Errorf("onebot: message 是字符串格式，请把 messagePostFormat 配为 array")

// decodeEvent 解码一条 post_type = message 的事件。message 为字符串格式时按纯文本降级（不解析 CQ 码），
// 并返回 errStringFormat 供调用方告警；ev 仍可用。
func decodeEvent(b []byte) (Event, error) {
	var r rawEvent
	if err := json.Unmarshal(b, &r); err != nil {
		return Event{}, fmt.Errorf("onebot: decode event: %w", err)
	}
	ev := Event{
		Time: r.Time, SelfID: int64(r.SelfID), MessageType: r.MessageType, SubType: r.SubType,
		MessageID: int64(r.MessageID), UserID: int64(r.UserID), GroupID: int64(r.GroupID),
		RawMessage: r.RawMessage,
		Sender: Sender{UserID: int64(r.Sender.UserID), Nickname: r.Sender.Nickname,
			Card: r.Sender.Card, Role: r.Sender.Role},
	}
	if ev.Sender.UserID == 0 {
		ev.Sender.UserID = ev.UserID
	}
	msg := bytes.TrimSpace(r.Message)
	if len(msg) > 0 && msg[0] == '"' {
		var s string
		if err := json.Unmarshal(msg, &s); err != nil {
			return Event{}, fmt.Errorf("onebot: decode message: %w", err)
		}
		ev.Segments = TextSegments(s)
		return ev, errStringFormat
	}
	if len(msg) > 0 {
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber() // QQ 号等数字保持原样，不经 float64
		if err := dec.Decode(&ev.Segments); err != nil {
			return Event{}, fmt.Errorf("onebot: decode message: %w", err)
		}
	}
	return ev, nil
}
