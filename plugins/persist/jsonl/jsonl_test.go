package jsonl

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aria/core/loop"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

type rawBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Data string `json:"data"`
	MIME string `json:"mime"`
}

type rawMessage struct {
	Role   string     `json:"role"`
	Blocks []rawBlock `json:"blocks"`
}

func readEnvelopes(t *testing.T, path string) []envelope {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []envelope
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var env envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, env)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStoreAppendAndShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log", "session.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.Append(ctx, "sess-1", loop.Event{
		Kind: loop.KindAgentStart, RunID: "r1", At: time.Unix(999, 0).UTC(),
		Data: loop.AgentStartData{
			Scope:        ctxx.Scope{UserID: "u1", SessionID: "sess-1"},
			InitialInput: []message.Message{message.NewUser("你好")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "sess-1", loop.Event{
		Kind: loop.KindMessageEnd, RunID: "r1", Turn: 2,
		At: time.Unix(1000, 0).UTC(),
		Data: loop.MessageEndData{
			Message: message.Message{
				Role: message.RoleUser,
				Blocks: []message.Block{
					message.TextBlock{Text: "看看这张"},
					message.ImageBlock{MIME: "image/png", Data: []byte{1, 2, 3}},
					&message.TextBlock{Text: "指针形态也合法"},
				},
			},
			Usage: message.Usage{In: 10, Out: 5},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "sess-1", loop.Event{
		Kind: loop.KindToolExecEnd, RunID: "r1", Turn: 3, At: time.Unix(1001, 0).UTC(),
		Data: loop.ToolExecEndData{
			Call:   message.ToolCall{ID: "c1", Name: "now"},
			Result: message.ToolResult{CallID: "c1", Blocks: []message.Block{message.TextBlock{Text: "12:00"}}},
			Denied: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	envs := readEnvelopes(t, path)
	if len(envs) != 3 {
		t.Fatalf("lines = %d", len(envs))
	}

	e0 := envs[0]
	if e0.V != FormatVersion || e0.Session != "sess-1" || e0.Kind != string(loop.KindAgentStart) {
		t.Fatalf("envelope0: %+v", e0)
	}
	var d0 struct {
		Scope        wireScope    `json:"scope"`
		InitialInput []rawMessage `json:"initial_input"`
	}
	if err := json.Unmarshal(e0.Data, &d0); err != nil {
		t.Fatal(err)
	}
	if d0.Scope.UserID != "u1" || d0.Scope.SessionID != "sess-1" {
		t.Fatalf("scope: %+v", d0.Scope)
	}
	if len(d0.InitialInput) != 1 || d0.InitialInput[0].Blocks[0].Text != "你好" {
		t.Fatalf("initial_input: %+v", d0.InitialInput)
	}

	e1 := envs[1]
	if e1.Turn != 2 {
		t.Fatalf("envelope1 turn: %+v", e1)
	}
	var d1 struct {
		Message rawMessage `json:"message"`
	}
	if err := json.Unmarshal(e1.Data, &d1); err != nil {
		t.Fatal(err)
	}
	if d1.Message.Role != "user" || len(d1.Message.Blocks) != 3 {
		t.Fatalf("message: %+v", d1.Message)
	}
	b0, b1, b2 := d1.Message.Blocks[0], d1.Message.Blocks[1], d1.Message.Blocks[2]
	if b0.Type != "text" || b0.Text != "看看这张" {
		t.Fatalf("block0: %+v", b0)
	}
	if b1.Type != "image" || b1.MIME != "image/png" || b1.Data != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("block1: %+v", b1)
	}
	if b2.Text != "指针形态也合法" {
		t.Fatalf("block2: %+v", b2)
	}

	e2 := envs[2]
	if !strings.Contains(string(e2.Data), `"denied":true`) || !strings.Contains(string(e2.Data), `"name":"now"`) {
		t.Fatalf("tool_exec_end data: %s", e2.Data)
	}
}

func TestStoreReopenAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), "a", loop.Event{Kind: loop.KindAgentEnd, Data: loop.AgentEndData{
		Result: loop.RunResult{RunID: "r", EndReason: loop.EndError, Turns: 1},
		Err:    context.Canceled,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Append(context.Background(), "a", loop.Event{Kind: loop.KindTurnStart, Data: loop.TurnStartData{Turn: 1}}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 2 {
		t.Fatalf("lines = %d", got)
	}
	if !strings.Contains(string(b), `"err":"context canceled"`) {
		t.Fatalf("agent_end err missing: %s", b)
	}
	if !strings.Contains(string(b), `"end_reason":"error"`) {
		t.Fatalf("end_reason missing: %s", b)
	}
}

func TestStoreUnknownKindKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Append(context.Background(), "a", loop.Event{Kind: loop.Kind("future_kind"), Data: struct{ X int }{7}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"future_kind"`) {
		t.Fatalf("unknown kind dropped: %s", b)
	}
}

func TestStoreWindowCompressedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rep := loop.WindowCompressedData{
		InMessages: 24, InChars: 21482, OutMessages: 2, OutChars: 640,
	}
	if err := s.Append(context.Background(), "aria", loop.Event{Kind: loop.KindWindowCompressed, Data: rep}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"window_compressed"`, `"in_messages":24`, `"in_chars":21482`, `"out_messages":2`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("压缩事件缺字段 %s：%s", want, b)
		}
	}
}

// initial_input 按内容去重：同一 Store 实例内重复出现的消息（含同一行内的重复）写为
// {"ref":hash}，首次出现的消息写全文并带 hash；重新打开文件后去重状态清空。
func TestStoreInitialInputDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	sys := message.NewSystem("人设")
	start := func(st *Store, msgs ...message.Message) {
		t.Helper()
		if err := st.Append(context.Background(), "a", loop.Event{Kind: loop.KindAgentStart,
			Data: loop.AgentStartData{Scope: ctxx.Scope{SessionID: "a"}, InitialInput: msgs}}); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	start(s, sys, message.NewUser("第一句"))
	start(s, sys, message.NewUser("第一句"), message.NewUser("第二句"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	start(s2, sys, sys)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	type elem struct {
		Hash   string     `json:"hash"`
		Ref    string     `json:"ref"`
		Role   string     `json:"role"`
		Blocks []rawBlock `json:"blocks"`
	}
	var lines [][]elem
	for _, env := range readEnvelopes(t, path) {
		var d struct {
			InitialInput []elem `json:"initial_input"`
		}
		if err := json.Unmarshal(env.Data, &d); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, d.InitialInput)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	l0, l1, l2 := lines[0], lines[1], lines[2]
	if len(l0) != 2 || l0[0].Hash == "" || l0[0].Ref != "" || l0[0].Role != "system" || l0[1].Hash == "" {
		t.Fatalf("首行应全部写全文：%+v", l0)
	}
	if len(l1) != 3 || l1[0].Ref != l0[0].Hash || l1[1].Ref != l0[1].Hash ||
		l1[2].Hash == "" || len(l1[2].Blocks) != 1 || l1[2].Blocks[0].Text != "第二句" {
		t.Fatalf("第二行应为两条 ref 加一条全文：%+v", l1)
	}
	if len(l2) != 2 || l2[0].Hash != l0[0].Hash || l2[0].Ref != "" || l2[1].Ref != l2[0].Hash {
		t.Fatalf("重新打开后应重写全文，同一行内的重复写 ref：%+v", l2)
	}
}

// 比较基准最多保留 maxBaselines 个会话：超出时移除最早建立基准的会话，其下一条 agent_start 写全文；
// 仍在保留范围内的会话照常写 ref。
func TestStoreBaselinesCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := message.NewSystem("人设")
	start := func(sid string) {
		t.Helper()
		if err := s.Append(context.Background(), sid, loop.Event{Kind: loop.KindAgentStart,
			Data: loop.AgentStartData{Scope: ctxx.Scope{SessionID: sid}, InitialInput: []message.Message{sys}}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i <= maxBaselines; i++ { // maxBaselines + 1 个会话：s0 的基准被移除
		start(fmt.Sprintf("s%d", i))
	}
	start("s0")
	start(fmt.Sprintf("s%d", maxBaselines))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(s.prev) != maxBaselines || len(s.order) != maxBaselines {
		t.Fatalf("基准应保留 %d 个会话：prev %d order %d", maxBaselines, len(s.prev), len(s.order))
	}

	type elem struct {
		Hash string `json:"hash"`
		Ref  string `json:"ref"`
	}
	envs := readEnvelopes(t, path)
	last := func(i int) elem {
		t.Helper()
		var d struct {
			InitialInput []elem `json:"initial_input"`
		}
		if err := json.Unmarshal(envs[i].Data, &d); err != nil || len(d.InitialInput) != 1 {
			t.Fatalf("第 %d 行解析失败：%v", i, err)
		}
		return d.InitialInput[0]
	}
	n := len(envs)
	if e := last(n - 2); e.Hash == "" || e.Ref != "" {
		t.Fatalf("s0 的基准已移除，应写全文：%+v", e)
	}
	if e := last(n - 1); e.Ref == "" {
		t.Fatalf("s%d 的基准仍保留，应写 ref：%+v", maxBaselines, e)
	}
}
