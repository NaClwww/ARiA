package jsonl

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
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
