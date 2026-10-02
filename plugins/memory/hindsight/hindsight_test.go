package hindsight

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aria/runtime/memory"
)

// fakeHindsight 记录收到的请求，按查询词返回预设的 recall 结果。
type fakeHindsight struct {
	mu      sync.Mutex
	puts    []string         // PUT bank 路径
	retains []retainRequest  // POST retain 请求体
	recalls []recallRequest  // POST recall 请求体
	results map[string][]any // recall 查询包含的子串 → results
	status  map[string]int   // 路径后缀 → 强制返回的状态码
	srv     *httptest.Server
}

func newFake(t *testing.T) *fakeHindsight {
	t.Helper()
	f := &fakeHindsight{results: map[string][]any{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		for suffix, code := range f.status {
			if strings.HasSuffix(r.URL.Path, suffix) {
				w.WriteHeader(code)
				io.WriteString(w, `{"detail":"forced"}`)
				return
			}
		}
		switch {
		case r.Method == http.MethodPut:
			f.puts = append(f.puts, r.URL.Path)
			io.WriteString(w, `{"bank_id":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/memories/recall"):
			var rq recallRequest
			_ = json.Unmarshal(body, &rq)
			f.recalls = append(f.recalls, rq)
			var results []any
			for key, rs := range f.results {
				if strings.Contains(rq.Query, key) {
					results = append(results, rs...)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case strings.HasSuffix(r.URL.Path, "/memories"):
			var rq retainRequest
			_ = json.Unmarshal(body, &rq)
			f.retains = append(f.retains, rq)
			io.WriteString(w, `{"success":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

var fixedNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T, f *fakeHindsight, members ...string) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{BaseURL: f.srv.URL, Dir: dir, Members: members, Now: func() time.Time { return fixedNow }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func stage(t *testing.T, s *Service, sid, batch string, seq int, points ...memory.Point) {
	t.Helper()
	if err := s.Stage(context.Background(), memory.StageRequest{Namespace: "home", SessionID: sid, BatchID: batch, Seq: seq, Points: points}); err != nil {
		t.Fatal(err)
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func texts(items []memory.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

// Stage：同一 batch_id 只写一次；不同批次追加；空要点不写。
func TestStageDedupesBatch(t *testing.T) {
	s, dir := newService(t, newFake(t))
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "小明喜欢猫", Kind: memory.KindPreference})
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "重放"})
	stage(t, s, "s1", "b2", 2, memory.Point{Text: "周日去大阪", Kind: memory.KindEvent})
	stage(t, s, "s1", "b3", 3)
	path := filepath.Join(dir, "home", "s1"+stagedExt)
	got := lines(t, path)
	if len(got) != 2 || !strings.Contains(got[0], `"batch_id":"b1"`) || strings.Contains(got[0], "重放") || !strings.Contains(got[1], "大阪") {
		t.Fatalf("暂存文件内容不符：%q", got)
	}
}

// End：每批一个 retain item，承诺另成带 tag 的 item；成功后文件改名 .committed；再次 End 不再调用引擎。
func TestEndRetainsAndCommits(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	due := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "小明喜欢猫", Kind: memory.KindPreference, Speakers: []string{"小明"}})
	stage(t, s, "s1", "b2", 2,
		memory.Point{Text: "周日去大阪", Kind: memory.KindEvent},
		memory.Point{Text: "ARiA 答应明早八点提醒小明带伞", Kind: memory.KindCommitment, Due: due})
	if err := s.End(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s1", At: fixedNow}); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || f.puts[0] != "/v1/default/banks/home" {
		t.Fatalf("应先 PUT 建立 bank：%v", f.puts)
	}
	if len(f.retains) != 1 || f.retains[0].Async {
		t.Fatalf("应同步 retain 1 次：%+v", f.retains)
	}
	items := f.retains[0].Items
	if len(items) != 3 {
		t.Fatalf("应有 3 个 item（2 批 + 1 承诺）：%+v", items)
	}
	if items[0].DocumentID != "s1#1" || items[0].UpdateMode != "replace" || items[0].Context != retainContext ||
		!strings.Contains(items[0].Content, "[偏好] 小明喜欢猫（说话人：小明）") || items[0].Metadata["session_id"] != "s1" {
		t.Fatalf("第 1 批 item 不符：%+v", items[0])
	}
	if items[1].DocumentID != "s1#2" || !strings.Contains(items[1].Content, "[事件] 周日去大阪\n[承诺] ARiA 答应明早八点提醒小明带伞（到期 2026-10-03T08:00:00Z）") {
		t.Fatalf("第 2 批 item 不符：%+v", items[1])
	}
	if items[2].DocumentID != "s1#2#c1" || len(items[2].Tags) != 1 || items[2].Tags[0] != CommitmentTag || items[2].Timestamp != "2026-10-03T08:00:00Z" {
		t.Fatalf("承诺 item 不符：%+v", items[2])
	}
	if _, err := os.Stat(filepath.Join(dir, "home", "s1"+stagedExt)); !os.IsNotExist(err) {
		t.Fatalf("暂存文件应已改名：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "home", "s1"+committedExt)); err != nil {
		t.Fatalf("应有 .committed 文件：%v", err)
	}
	if err := s.End(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s1"}); err != nil || len(f.retains) != 1 {
		t.Fatalf("无暂存时 End 不应调用引擎：err %v retains %d", err, len(f.retains))
	}
}

// End：引擎返回错误时 End 返回错误，暂存文件保留。
func TestEndKeepsStagedOnError(t *testing.T) {
	f := newFake(t)
	f.status["/memories"] = http.StatusInternalServerError
	s, dir := newService(t, f)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "x"})
	err := s.End(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("应返回 HTTP 500 错误：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "home", "s1"+stagedExt)); err != nil {
		t.Fatalf("失败时暂存文件应保留：%v", err)
	}
}

// Start：后台提交遗留会话；返回成员背景、未到期承诺与上一会话最近要点；同一 namespace 只保留 1 个 .committed。
func TestStartCommitsLeftoversAndBuildsBackground(t *testing.T) {
	f := newFake(t)
	f.results["小明的身份"] = []any{
		map[string]any{"text": "小明是家里的大儿子", "metadata": map[string]any{"session_id": "s0"}, "occurred_start": "2026-09-30T10:00:00Z"},
		map[string]any{"text": "小明喜欢猫", "mentioned_at": "2026-09-30T10:05:00Z"},
		map[string]any{"text": "小明喜欢猫"},
	}
	f.results["答应过的事"] = []any{
		map[string]any{"text": "ARiA 答应 10 月 3 日提醒小明带伞", "occurred_start": "2026-10-03T08:00:00Z", "tags": []string{CommitmentTag}},
		map[string]any{"text": "ARiA 答应 9 月 1 日提醒小明交作业", "occurred_start": "2026-09-01T08:00:00Z"},
		map[string]any{"text": "ARiA 答应明年提醒小明", "occurred_start": "2027-10-03T08:00:00Z"},
	}
	s, dir := newService(t, f, "小明")
	// 两个此前未结束的会话：s0 只有 1 条要点，s1 有 12 条；目录顺序为 s0、s1，s1 为最近提交的会话
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"})
	for i := 1; i <= 12; i++ {
		stage(t, s, "s1", "b"+string(rune('a'+i)), i, memory.Point{Text: "s1 第 " + string(rune('0'+i%10)) + " 条"})
	}
	items, err := s.Start(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s2", At: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	s.bg.Wait() // 遗留提交在后台执行
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.retains) != 2 {
		t.Fatalf("应提交 2 个遗留会话：%d", len(f.retains))
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "home"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || !strings.HasSuffix(names[0], committedExt) {
		t.Fatalf("应只剩 1 个 .committed 文件：%v", names)
	}
	got := texts(items)
	wantHead := []string{"小明是家里的大儿子", "小明喜欢猫", "ARiA 答应 10 月 3 日提醒小明带伞"}
	if len(got) < len(wantHead) || strings.Join(got[:3], "|") != strings.Join(wantHead, "|") {
		t.Fatalf("背景与承诺不符：%v", got)
	}
	if items[0].SessionID != "s0" || items[0].At.IsZero() || items[1].At.IsZero() {
		t.Fatalf("会话与时间映射不符：%+v", items[:2])
	}
	recent := got[3:]
	if len(recent) != RecentPoints || recent[0] != previousPrefix+"s1 第 3 条" || recent[len(recent)-1] != previousPrefix+"s1 第 2 条" {
		t.Fatalf("上一会话最近要点不符（应取最近提交的会话 s1 的最后 10 条）：%v", recent)
	}
	if len(f.recalls) != 2 || !f.recalls[0].PreferObservations || f.recalls[0].Budget != "low" || len(f.recalls[1].Tags) != 1 {
		t.Fatalf("recall 请求不符：%+v", f.recalls)
	}
}

// Recall：引擎结果与本地暂存匹配合并，暂存标 staged，按时间升序。
func TestRecallMergesStaged(t *testing.T) {
	f := newFake(t)
	f.results["大阪"] = []any{
		map[string]any{"text": "小明去年去过大阪", "occurred_start": "2025-10-01T00:00:00Z", "document_id": "s0#1"},
	}
	s, _ := newService(t, f)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "周日改去大阪看演唱会"}, memory.Point{Text: "小明喜欢猫"})
	items, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "home", SessionID: "s1", Query: "大阪 演唱会", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应合并为 2 条：%v", texts(items))
	}
	if items[0].Text != "小明去年去过大阪" || items[0].SessionID != "s0" || items[0].Source != memory.SourceCommitted {
		t.Fatalf("引擎条目不符：%+v", items[0])
	}
	if items[1].Text != "周日改去大阪看演唱会" || items[1].Source != memory.SourceStaged || items[1].SessionID != "s1" {
		t.Fatalf("暂存条目不符：%+v", items[1])
	}
	if f.recalls[0].Budget != "mid" || f.recalls[0].MaxTokens != 1200 {
		t.Fatalf("recall 请求不符：%+v", f.recalls[0])
	}
}

// queryTokens：CJK 二字组合与小写原词，长度 1 丢弃。
func TestQueryTokens(t *testing.T) {
	got := strings.Join(queryTokens("大阪 演唱会 Alice x"), ",")
	if got != "大阪,演唱,唱会,alice" {
		t.Fatalf("tokens: %q", got)
	}
}

// 路径分量校验：namespace 与 session_id 含路径分隔符时拒绝。
func TestRejectsPathComponents(t *testing.T) {
	s, _ := newService(t, newFake(t))
	if err := s.Stage(context.Background(), memory.StageRequest{Namespace: "../x", SessionID: "s", BatchID: "b", Points: []memory.Point{{Text: "x"}}}); err == nil {
		t.Fatal("应拒绝 namespace ../x")
	}
	if _, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "a/b", Query: "x"}); err == nil {
		t.Fatal("应拒绝 namespace a/b")
	}
}
