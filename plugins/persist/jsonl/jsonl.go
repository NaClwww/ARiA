// Package jsonl 是 persist.Store 的 JSON Lines 文件实现（demo 级，docs/03 §1）：
// 每行一个事件 envelope，data 按 Kind 定形。写路完整；读路/重放随恢复需求再做
// （v1 只写不恢复）——格式带版本号字段，届时写迁移而非一次性格式。
//
// 格式版本 2：agent_start 的 initial_input 按内容去重。每个元素是以下两种之一：
//   - 完整消息，带 "hash" 字段（消息 JSON 的 SHA-256 前 16 字节，hex）；
//   - {"ref":"<hash>"}，指向本文件中位于其前方、hash 相同的最近一条完整消息。
//
// 比较基准是同一会话上一条 agent_start 的 initial_input（同一行内的重复同样写 ref）。
// 组装上下文是滑动窗口（system + 记忆 + 近轮 + 新输入），重复内容只出现在相邻两次组装之间；
// Store 实例新建（进程重启、重新打开文件）后各会话的首条 agent_start 写全文。
package jsonl

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aria/core/loop"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// FormatVersion 是行格式的版本号（F2：第一天带上，免得日后写一次性迁移）。
// 版本 2 起 agent_start 的 initial_input 元素可为 {"ref":hash}，见包注释。
const FormatVersion = 2

// Store 把事件按行追加进单个文件；多会话共用一个文件，session 字段区分。
// 并发安全：Append 可能来自不同会话的 Recorder。
type Store struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// prev 按会话记录上一条 agent_start 的 initial_input hash 集合，写入成功后整体替换；
	// 占用为每会话一个上下文的消息数。
	prev map[string]map[[16]byte]struct{}
}

// New 打开（或创建）path 的追加句柄；父目录自动创建。
func New(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("jsonl: create dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}
	return &Store{f: f, path: path, prev: map[string]map[[16]byte]struct{}{}}, nil
}

// Path 返回落盘文件路径。
func (s *Store) Path() string { return s.path }

// Append 写一个事件。一次 Write 一整行——行完整性靠单次写调用保证
// （同文件多写者由锁串行化；OS 级 append 原子性对 4KB 内行足够）。
// 编码在锁外完成；agent_start 的去重判定需读取会话基准，见 appendAgentStart。
func (s *Store) Append(_ context.Context, sessionID string, ev loop.Event) error {
	if d, ok := agentStartOf(ev); ok {
		return s.appendAgentStart(sessionID, ev, d)
	}
	data, err := marshalData(ev)
	if err != nil {
		return fmt.Errorf("jsonl: encode %s: %w", ev.Kind, err)
	}
	line, err := envelopeLine(sessionID, ev, data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(line)
}

// appendAgentStart 写 agent_start：各条 initial_input 消息在锁外定形并计算 hash；锁内与该会话
// 上一条 agent_start 的 hash 集合及本行靠前元素比较，已出现的写 ref，其余写全文；
// 写入成功后以本行的 hash 集合替换该会话的比较基准。
func (s *Store) appendAgentStart(sessionID string, ev loop.Event, d loop.AgentStartData) error {
	msgs, err := hashMessages(d.InitialInput)
	if err != nil {
		return fmt.Errorf("jsonl: encode %s: %w", ev.Kind, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.prev[sessionID]
	cur := make(map[[16]byte]struct{}, len(msgs))
	var inputs []json.RawMessage
	for _, m := range msgs {
		_, inPrev := prev[m.hash]
		_, inLine := cur[m.hash]
		if inPrev || inLine {
			inputs = append(inputs, m.ref())
		} else {
			inputs = append(inputs, m.full())
		}
		cur[m.hash] = struct{}{}
	}
	data, err := marshal(struct {
		Scope        wireScope         `json:"scope"`
		InitialInput []json.RawMessage `json:"initial_input,omitempty"`
	}{wireScopeOf(d.Scope), inputs})
	if err != nil {
		return fmt.Errorf("jsonl: encode %s: %w", ev.Kind, err)
	}
	line, err := envelopeLine(sessionID, ev, data)
	if err != nil {
		return err
	}
	if err := s.writeLocked(line); err != nil {
		return err
	}
	s.prev[sessionID] = cur
	return nil
}

// writeLocked 以单次 Write 写入一整行（调用方持有 s.mu）。
func (s *Store) writeLocked(line []byte) error {
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("jsonl: write: %w", err)
	}
	return nil
}

// envelopeLine 编码一行 envelope（含行尾换行符）。
func envelopeLine(sessionID string, ev loop.Event, data json.RawMessage) ([]byte, error) {
	line, err := json.Marshal(envelope{
		V:       FormatVersion,
		Session: sessionID,
		Kind:    string(ev.Kind),
		RunID:   ev.RunID,
		Turn:    ev.Turn,
		At:      ev.At,
		Data:    data,
	})
	if err != nil {
		return nil, fmt.Errorf("jsonl: encode envelope: %w", err)
	}
	return append(line, '\n'), nil
}

// agentStartOf 取出 agent_start 载荷（值与指针形态，nil 指针按非 agent_start 处理）。
func agentStartOf(ev loop.Event) (loop.AgentStartData, bool) {
	switch d := ev.Data.(type) {
	case loop.AgentStartData:
		return d, true
	case *loop.AgentStartData:
		if d != nil {
			return *d, true
		}
	}
	return loop.AgentStartData{}, false
}

// hashedMessage 是定形后的 initial_input 消息：body 为消息 JSON，hash 为其 SHA-256 前 16 字节。
type hashedMessage struct {
	body []byte
	hash [16]byte
}

// hashMessages 逐条定形 initial_input 并计算 hash。
func hashMessages(ms []message.Message) ([]hashedMessage, error) {
	out := make([]hashedMessage, 0, len(ms))
	for _, m := range ms {
		w, err := messageOf(m)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, hashedMessage{body: body, hash: [16]byte(sum[:16])})
	}
	return out, nil
}

// full 返回带 hash 字段的全文：在 body 的左花括号之后插入 "hash" 字段（body 恒含 role 字段，非空对象）。
func (m hashedMessage) full() json.RawMessage {
	return json.RawMessage(`{"hash":"` + hex.EncodeToString(m.hash[:]) + `",` + string(m.body[1:]))
}

// ref 返回指向同 hash 完整消息的引用元素。
func (m hashedMessage) ref() json.RawMessage {
	return json.RawMessage(`{"ref":"` + hex.EncodeToString(m.hash[:]) + `"}`)
}

// Close 关闭文件句柄。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

type envelope struct {
	V       int             `json:"v"`
	Session string          `json:"session"`
	Kind    string          `json:"kind"`
	RunID   string          `json:"run_id,omitempty"`
	Turn    int             `json:"turn"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data"`
}

// ---------- data 定形 ----------

type wireScope struct {
	UserID    string `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type wireMessage struct {
	ID          string             `json:"id,omitempty"`
	Role        string             `json:"role"`
	Blocks      []json.RawMessage  `json:"blocks,omitempty"`
	ToolCalls   []message.ToolCall `json:"tool_calls,omitempty"` // 自带 json 标签
	ToolCallID  string             `json:"tool_call_id,omitempty"`
	IsError     bool               `json:"is_error,omitempty"`
	Interrupted bool               `json:"interrupted,omitempty"`
}

type wireToolResult struct {
	CallID  string            `json:"call_id,omitempty"`
	IsError bool              `json:"is_error,omitempty"`
	Blocks  []json.RawMessage `json:"blocks,omitempty"`
}

type wireUsage struct {
	In     int     `json:"in"`
	Out    int     `json:"out"`
	Cached int     `json:"cached"`
	Cost   float64 `json:"cost"`
}

type wireRunResult struct {
	RunID     string    `json:"run_id"`
	EndReason string    `json:"end_reason"`
	Turns     int       `json:"turns"`
	Usage     wireUsage `json:"usage"`
}

// marshalData 按 Kind 把 Data 载荷定形；未知 Kind 写 null（不静默丢事件）。
// agent_start 由 appendAgentStart 处理（去重规则见包注释）。
func marshalData(ev loop.Event) (json.RawMessage, error) {
	switch d := ev.Data.(type) {
	case loop.UserMessageInjectedData:
		w, err := messageOf(d.Message)
		if err != nil {
			return nil, err
		}
		return marshal(struct {
			Message wireMessage `json:"message"`
		}{w})
	case loop.TurnStartData:
		return marshal(struct {
			Turn          int    `json:"turn"`
			WindowSummary string `json:"window_summary,omitempty"`
		}{d.Turn, d.WindowSummary})
	case loop.TurnEndData:
		return marshal(struct {
			Turn  int       `json:"turn"`
			Usage wireUsage `json:"usage"`
		}{d.Turn, usageOf(d.Usage)})
	case loop.MessageStartData:
		return marshal(struct {
			Role      string `json:"role"`
			MessageID string `json:"message_id,omitempty"`
		}{string(d.Role), d.MessageID})
	case loop.MessageEndData:
		w, err := messageOf(d.Message)
		if err != nil {
			return nil, err
		}
		return marshal(struct {
			Message wireMessage `json:"message"`
			Usage   wireUsage   `json:"usage"`
		}{w, usageOf(d.Usage)})
	case *loop.MessageEndData:
		return marshalData(loop.Event{Kind: ev.Kind, Data: *d})
	case loop.ToolGuardDecisionData:
		w := struct {
			Call      message.ToolCall  `json:"call"`
			Action    int               `json:"action"`
			Reason    string            `json:"reason,omitempty"`
			Rewritten *message.ToolCall `json:"rewritten,omitempty"`
		}{Call: d.Call, Action: int(d.Action), Reason: d.Reason}
		if d.Rewritten != nil {
			r := *d.Rewritten
			w.Rewritten = &r
		}
		return marshal(w)
	case loop.ToolExecStartData:
		return marshal(struct {
			Call message.ToolCall `json:"call"`
		}{d.Call})
	case loop.ToolExecEndData:
		tr, err := toolResultOf(d.Result)
		if err != nil {
			return nil, err
		}
		return marshal(struct {
			Call   message.ToolCall `json:"call"`
			Result wireToolResult   `json:"result"`
			Denied bool             `json:"denied,omitempty"`
		}{d.Call, tr, d.Denied})
	case *loop.ToolExecEndData:
		return marshalData(loop.Event{Kind: ev.Kind, Data: *d})
	case loop.AgentEndData:
		w := struct {
			Result wireRunResult `json:"result"`
			Err    string        `json:"err,omitempty"`
		}{Result: wireRunResult{
			RunID: d.Result.RunID, EndReason: string(d.Result.EndReason),
			Turns: d.Result.Turns, Usage: usageOf(d.Result.Usage),
		}}
		if d.Err != nil {
			w.Err = d.Err.Error()
		}
		return marshal(w)
	case loop.WindowCompressedData:
		return marshal(d) // 自带 json 标签，字段按原样落盘
	default:
		return json.RawMessage("null"), nil
	}
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func wireScopeOf(s ctxx.Scope) wireScope {
	return wireScope{UserID: s.UserID, SessionID: s.SessionID, AgentID: s.AgentID, Namespace: s.Namespace}
}

func usageOf(u message.Usage) wireUsage {
	return wireUsage{In: u.In, Out: u.Out, Cached: u.Cached, Cost: u.Cost}
}

func messageOf(m message.Message) (wireMessage, error) {
	w := wireMessage{
		ID: m.ID, Role: string(m.Role),
		ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID,
		IsError: m.IsError, Interrupted: m.Interrupted,
	}
	for _, b := range m.Blocks {
		raw, err := marshalBlock(b)
		if err != nil {
			return wireMessage{}, err
		}
		w.Blocks = append(w.Blocks, raw)
	}
	return w, nil
}

func toolResultOf(r message.ToolResult) (wireToolResult, error) {
	w := wireToolResult{CallID: r.CallID, IsError: r.IsError}
	for _, b := range r.Blocks {
		raw, err := marshalBlock(b)
		if err != nil {
			return wireToolResult{}, err
		}
		w.Blocks = append(w.Blocks, raw)
	}
	return w, nil
}

// marshalBlock 把内建 Block 定形为带 type 标签的对象（值/指针形态都收，
// 与 message.CloneBlock 契约一致；nil 指针产出空对象占位，不丢位次）。
// 不支持的块返回错误并沿 Append 上抛：持久化链响亮中断——将来给
// pkg/message 新增 Block 类型而忘了配 wire 格式时立刻暴露，绝不静默把块
// 从 durable 历史里丢掉（durable 的价值就在载荷完整）。
func marshalBlock(b message.Block) (json.RawMessage, error) {
	switch b := b.(type) {
	case message.TextBlock:
		return marshal(taggedText{"text", b.Text})
	case *message.TextBlock:
		return marshal(taggedText{"text", deref(b).Text})
	case message.ImageBlock:
		return marshal(taggedImage{"image", b.MIME, b.URL, b64(b.Data)})
	case *message.ImageBlock:
		return marshal(taggedImage{"image", deref(b).MIME, deref(b).URL, b64(deref(b).Data)})
	case message.AudioBlock:
		return marshal(taggedAudio{"audio", b.MIME, b64(b.Data)})
	case *message.AudioBlock:
		return marshal(taggedAudio{"audio", deref(b).MIME, b64(deref(b).Data)})
	case message.FileBlock:
		return marshal(taggedFile{"file", b.Name, b.URI, b.MIME})
	case *message.FileBlock:
		return marshal(taggedFile{"file", deref(b).Name, deref(b).URI, deref(b).MIME})
	case message.ThoughtBlock:
		return marshal(taggedText{"thought", b.Text})
	case *message.ThoughtBlock:
		return marshal(taggedText{"thought", deref(b).Text})
	default:
		return nil, fmt.Errorf("jsonl: unsupported block %T", b)
	}
}

// deref 解引用非 nil 指针 Block；nil 返回零值（占位空块，保住位次）。
func deref[T any](b *T) T {
	if b == nil {
		var zero T
		return zero
	}
	return *b
}

type taggedText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type taggedImage struct {
	Type string `json:"type"`
	MIME string `json:"mime,omitempty"`
	URL  string `json:"url,omitempty"`
	Data string `json:"data,omitempty"`
}

type taggedAudio struct {
	Type string `json:"type"`
	MIME string `json:"mime,omitempty"`
	Data string `json:"data,omitempty"`
}

type taggedFile struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
	MIME string `json:"mime,omitempty"`
}

func b64(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}
