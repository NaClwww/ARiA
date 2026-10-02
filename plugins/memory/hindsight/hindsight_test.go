package hindsight

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aria/runtime/memory"
)

// fakeHindsight 记录收到的请求，按查询词返回预设的 recall 结果；可按路径后缀强制状态码、延迟 PUT、
// 让 retain 在记录后等待放行（gate）。
type fakeHindsight struct {
	mu        sync.Mutex
	puts      []string
	retains   []retainRequest
	recalls   []recallRequest
	results   map[string][]any // recall 查询包含的子串 → results
	status    map[string]int   // 路径后缀 → 强制返回的状态码
	failQuery string           // recall 查询包含该子串时返回 500
	rawRecall string           // 非空时 recall 原样返回该正文
	putDelay  time.Duration
	gate      chan struct{} // 非 nil 时 retain 在记录后等待其关闭
	inflight  atomic.Int32  // 在途的 retain 数
	srv       *httptest.Server
}

func newFake(t *testing.T) *fakeHindsight {
	t.Helper()
	f := &fakeHindsight{results: map[string][]any{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		for suffix, code := range f.status {
			if strings.HasSuffix(r.URL.Path, suffix) {
				f.mu.Unlock()
				w.WriteHeader(code)
				io.WriteString(w, `{"detail":"forced"}`)
				return
			}
		}
		switch {
		case r.Method == http.MethodPut:
			f.puts = append(f.puts, r.URL.Path)
			delay := f.putDelay
			f.mu.Unlock()
			time.Sleep(delay)
			io.WriteString(w, `{"bank_id":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/memories/recall"):
			var rq recallRequest
			_ = json.Unmarshal(body, &rq)
			f.recalls = append(f.recalls, rq)
			if f.failQuery != "" && strings.Contains(rq.Query, f.failQuery) {
				f.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if f.rawRecall != "" {
				raw := f.rawRecall
				f.mu.Unlock()
				io.WriteString(w, raw)
				return
			}
			var results []any
			for key, rs := range f.results {
				if strings.Contains(rq.Query, key) {
					results = append(results, rs...)
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case strings.HasSuffix(r.URL.Path, "/memories"):
			var rq retainRequest
			_ = json.Unmarshal(body, &rq)
			f.retains = append(f.retains, rq)
			gate := f.gate
			f.mu.Unlock()
			f.inflight.Add(1)
			if gate != nil {
				<-gate
			}
			f.inflight.Add(-1)
			io.WriteString(w, `{"success":true}`)
		default:
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHindsight) retainCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.retains)
}

// waitInflight 等待在途 retain 数达到 n。
func (f *fakeHindsight) waitInflight(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.inflight.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("在途 retain 未达到 %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var fixedNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// clock 每次取时刻前进 1 s，使批次的暂存时刻单调递增。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newService(t *testing.T, f *fakeHindsight, members ...string) (*Service, string) {
	t.Helper()
	return newServiceLog(t, f, quietLog(), members...)
}

func newServiceLog(t *testing.T, f *fakeHindsight, log *slog.Logger, members ...string) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	c := &clock{t: fixedNow}
	s, err := New(Config{BaseURL: f.srv.URL, Dir: dir, Members: members, Now: c.now}, log)
	if err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(dir, "home")
}

func stage(t *testing.T, s *Service, sid, batch string, seq int, points ...memory.Point) {
	t.Helper()
	if err := s.Stage(context.Background(), memory.StageRequest{Namespace: "home", SessionID: sid, BatchID: batch, Seq: seq, Points: points}); err != nil {
		t.Fatal(err)
	}
}

func end(s *Service, sid string) error {
	return s.End(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: sid, At: fixedNow})
}

func start(t *testing.T, s *Service, sid string) []memory.Item {
	t.Helper()
	items, err := s.Start(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: sid, At: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func texts(items []memory.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

func result(text string, kv ...any) map[string]any {
	m := map[string]any{"text": text}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// Stage：同一 batch_id 只写一次；不同批次追加；空要点不写。
func TestStageDedupesBatch(t *testing.T) {
	s, dir := newService(t, newFake(t))
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "小明喜欢猫", Kind: memory.KindPreference})
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "重放"})
	stage(t, s, "s1", "b2", 2, memory.Point{Text: "周日去大阪", Kind: memory.KindEvent})
	stage(t, s, "s1", "b3", 3)
	data, err := os.ReadFile(filepath.Join(dir, "s1"+stagedExt))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 2 || !strings.Contains(got[0], `"batch_id":"b1"`) || strings.Contains(got[0], "重放") || !strings.Contains(got[1], "大阪") {
		t.Fatalf("暂存文件内容不符：%q", got)
	}
	if n := names(t, dir); len(n) != 1 {
		t.Fatalf("不应残留临时文件：%v", n)
	}
}

// End：每批一个 retain item（timestamp = 暂存时刻），承诺另成带 tag 与 metadata.due 的 item；成功后文件改名
// .committed；再次 End 不再调用引擎。
func TestEndRetainsAndCommits(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	due := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "小明喜欢猫", Kind: memory.KindPreference, Speakers: []string{"小明"}})
	stage(t, s, "s1", "b2", 2,
		memory.Point{Text: "周日去大阪", Kind: memory.KindEvent},
		memory.Point{Text: "ARiA 答应明早八点提醒小明带伞", Kind: memory.KindCommitment, Due: due},
		memory.Point{Text: "ARiA 答应下次聊聊猫", Kind: memory.KindCommitment})
	if err := end(s, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || f.puts[0] != "/v1/default/banks/home" {
		t.Fatalf("应先 PUT 建立 bank：%v", f.puts)
	}
	if len(f.retains) != 1 || f.retains[0].Async {
		t.Fatalf("应同步 retain 1 次：%+v", f.retains)
	}
	items := f.retains[0].Items
	if len(items) != 4 {
		t.Fatalf("应有 4 个 item（2 批 + 2 承诺）：%+v", items)
	}
	if items[0].DocumentID != "s1#1" || items[0].UpdateMode != "replace" || items[0].Context != retainContext ||
		items[0].Timestamp != "2026-10-02T12:00:01Z" ||
		!strings.Contains(items[0].Content, "[偏好] 小明喜欢猫（说话人：小明）") || items[0].Metadata["session_id"] != "s1" {
		t.Fatalf("第 1 批 item 不符：%+v", items[0])
	}
	if items[1].DocumentID != "s1#2" || items[1].Timestamp != "2026-10-02T12:00:02Z" ||
		!strings.Contains(items[1].Content, "[事件] 周日去大阪\n[承诺] ARiA 答应明早八点提醒小明带伞（到期 2026-10-03T08:00:00Z）\n[承诺] ARiA 答应下次聊聊猫") {
		t.Fatalf("第 2 批 item 不符：%+v", items[1])
	}
	if items[2].DocumentID != "s1#2#c1" || len(items[2].Tags) != 1 || items[2].Tags[0] != commitmentTag ||
		items[2].Timestamp != "2026-10-02T12:00:02Z" || items[2].Metadata["due"] != "2026-10-03T08:00:00Z" {
		t.Fatalf("承诺 item 不符：%+v", items[2])
	}
	if items[3].DocumentID != "s1#2#c2" || items[3].Metadata["due"] != "" {
		t.Fatalf("无到期时间的承诺 item 不符：%+v", items[3])
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+committedExt {
		t.Fatalf("暂存文件应已改名：%v", n)
	}
	if err := end(s, "s1"); err != nil || len(f.retains) != 1 {
		t.Fatalf("无暂存时 End 不应调用引擎：err %v retains %d", err, len(f.retains))
	}
}

// End：引擎返回错误时 End 返回错误，暂存文件保留；PUT 失败同样返回错误。
func TestEndKeepsStagedOnError(t *testing.T) {
	f := newFake(t)
	f.status["/memories"] = http.StatusInternalServerError
	s, dir := newService(t, f)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "x"})
	if err := end(s, "s1"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("应返回 HTTP 500 错误：%v", err)
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+stagedExt {
		t.Fatalf("失败时暂存文件应保留：%v", n)
	}
	// bank 尚未建立成功的服务：PUT 失败同样返回错误
	f2 := newFake(t)
	f2.status["/banks/home"] = http.StatusBadRequest
	s2, _ := newService(t, f2)
	stage(t, s2, "s1", "b1", 1, memory.Point{Text: "x"})
	if err := end(s2, "s1"); err == nil || !strings.Contains(err.Error(), "建立 bank") {
		t.Fatalf("PUT 失败应返回建立 bank 的错误：%v", err)
	}
}

// End：retain 期间同会话新增批次时返回错误、文件保留；重试后以全部批次重新 retain。
func TestEndRejectsStageDuringCommit(t *testing.T) {
	f := newFake(t)
	f.gate = make(chan struct{})
	s, dir := newService(t, f)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "一"})
	errc := make(chan error, 1)
	go func() { errc <- end(s, "s1") }()
	f.waitInflight(t, 1)
	stage(t, s, "s1", "b2", 2, memory.Point{Text: "二"})
	close(f.gate)
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "新增 1 个批次") {
		t.Fatalf("提交期间新增批次应返回错误：%v", err)
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+stagedExt {
		t.Fatalf("文件应保留为暂存：%v", n)
	}
	f.gate = nil
	if err := end(s, "s1"); err != nil {
		t.Fatal(err)
	}
	if f.retainCount() != 2 || len(f.retains[1].Items) != 2 {
		t.Fatalf("重试应以 2 个批次 retain：%+v", f.retains)
	}
}

// End：全部批次都没有要点的文件不调用引擎，直接改名 .committed。
func TestEndEmptyBatchesSkipsEngine(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	if err := writeBatches(filepath.Join(dir, "s1"+stagedExt), []batchRecord{{BatchID: "b1", Seq: 1, At: fixedNow}}); err != nil {
		t.Fatal(err)
	}
	if err := end(s, "s1"); err != nil || f.retainCount() != 0 {
		t.Fatalf("空批次不应 retain：err %v retains %d", err, f.retainCount())
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+committedExt {
		t.Fatalf("应改名 .committed：%v", n)
	}
}

// Start：返回成员名单、每位成员最多 10 条（跨成员去重）、按 metadata.due 过滤的承诺。
func TestStartBackground(t *testing.T) {
	f := newFake(t)
	var jia []any
	for i := 0; i < 12; i++ {
		jia = append(jia, result("甲的事实 "+string(rune('A'+i)), "metadata", map[string]any{"session_id": "s0"}, "occurred_start", "2026-09-30T10:00:00Z"))
	}
	jia = append(jia, result("甲的事实 A"))
	f.results["甲的身份"] = jia
	f.results["乙的身份"] = []any{result("甲的事实 B"), result("乙喜欢猫", "mentioned_at", "2026-09-30T10:05:00Z")}
	f.results["答应过的事"] = []any{
		result("无到期的承诺", "metadata", map[string]any{"due": ""}, "mentioned_at", "2026-09-30T10:00:00Z"),
		result("缺少 due 键的承诺", "mentioned_at", "2026-09-30T10:00:00Z"),
		result("已过期的承诺", "metadata", map[string]any{"due": "2026-09-01T08:00:00Z"}),
		result("窗口内的承诺", "metadata", map[string]any{"due": "2026-10-20T08:00:00Z"}, "mentioned_at", "2026-09-30T10:00:00Z"),
		result("窗口外的承诺", "metadata", map[string]any{"due": "2027-01-01T08:00:00Z"}),
		result("到期无法解析的承诺", "metadata", map[string]any{"due": "明天"}),
	}
	s, _ := newService(t, f, "甲", "乙")
	items := start(t, s, "s1")
	got := texts(items)
	want := []string{"家庭成员：甲、乙"}
	for i := 0; i < 10; i++ {
		want = append(want, "甲的事实 "+string(rune('A'+i)))
	}
	want = append(want, "乙喜欢猫", "无到期的承诺", "缺少 due 键的承诺", "窗口内的承诺", "到期无法解析的承诺")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("背景不符：\n got %v\nwant %v", got, want)
	}
	if items[1].SessionID != "s0" || items[1].At.IsZero() || items[11].At.IsZero() {
		t.Fatalf("会话与时间映射不符：%+v %+v", items[1], items[11])
	}
	if len(f.recalls) != 3 {
		t.Fatalf("应 recall 3 次：%+v", f.recalls)
	}
	for _, rq := range f.recalls {
		if rq.Budget != "low" || rq.QueryTimestamp == "" {
			t.Fatalf("recall 请求不符：%+v", rq)
		}
		if len(rq.Tags) == 1 && (rq.Tags[0] != commitmentTag || rq.PreferObservations) {
			t.Fatalf("承诺 recall 请求不符：%+v", rq)
		}
		if len(rq.Tags) == 0 && !rq.PreferObservations {
			t.Fatalf("成员 recall 应 prefer_observations：%+v", rq)
		}
	}
	s.bg.Wait()
}

// Start：合计字数到达 1500 即停止追加；没有成员时不输出名单。
func TestStartCharLimitAndNoMembers(t *testing.T) {
	f := newFake(t)
	long := strings.Repeat("长", 200)
	var rs []any
	for i := 0; i < 10; i++ {
		rs = append(rs, result(long+string(rune('A'+i))))
	}
	f.results["丙的身份"] = rs
	s, _ := newService(t, f, "丙")
	items := start(t, s, "s1")
	// 名单 6 字 + 7 条 × 201 字 = 1413；第 8 条使合计 1614 > 1500
	if len(items) != 8 || items[0].Text != "家庭成员：丙" {
		t.Fatalf("字数上限不符：%d 条 %v", len(items), texts(items)[:1])
	}
	s.bg.Wait()

	f2 := newFake(t)
	f2.results["答应过的事"] = []any{result("无到期的承诺")}
	s2, _ := newService(t, f2)
	if got := texts(start(t, s2, "s1")); len(got) != 1 || got[0] != "无到期的承诺" || len(f2.recalls) != 1 {
		t.Fatalf("无成员时只应有承诺 recall：%v %d", got, len(f2.recalls))
	}
	s2.bg.Wait()
}

// Start：单个成员的 recall 失败只跳过；全部失败且无本地条目时返回错误；全部失败但有本地条目时返回本地条目。
func TestStartPartialFailure(t *testing.T) {
	f := newFake(t)
	f.results["甲的身份"] = []any{result("甲是老大")}
	f.failQuery = "乙的身份"
	s, _ := newService(t, f, "甲", "乙")
	if got := texts(start(t, s, "s1")); strings.Join(got, "|") != "家庭成员：甲、乙|甲是老大" {
		t.Fatalf("单项失败应跳过：%v", got)
	}
	s.bg.Wait()

	f.status["/memories/recall"] = http.StatusInternalServerError
	if items, err := s.Start(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s2"}); err == nil || items != nil {
		t.Fatalf("全部失败且无本地条目应返回错误：%v %v", items, err)
	}
	s.bg.Wait()
	stage(t, s, "s2", "b1", 1, memory.Point{Text: "本地要点"})
	items, err := s.Start(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: "s3"})
	if err != nil || strings.Join(texts(items), "|") != "家庭成员：甲、乙|"+previousPrefix+"本地要点" {
		t.Fatalf("全部失败但有本地条目应返回名单与本地条目：%v %v", items, err)
	}
	s.bg.Wait()
}

// Start：遗留会话在后台提交；上一会话按最后一批暂存时刻选取（晚于会话标识顺序）；只保留最新的 1 个 .committed。
func TestStartCommitsLeftoversAndRecent(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	for i := 1; i <= 12; i++ {
		stage(t, s, "s1", "b"+string(rune('a'+i)), i, memory.Point{Text: "s1 第 " + string(rune('0'+i%10)) + " 条"})
	}
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"}) // 暂存时刻晚于 s1 的全部批次
	items := start(t, s, "s2")
	s.bg.Wait()
	if f.retainCount() != 2 {
		t.Fatalf("应提交 2 个遗留会话：%d", f.retainCount())
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s0"+committedExt {
		t.Fatalf("应只保留最新的 s0.committed：%v", n)
	}
	if got := texts(items); len(got) != 1 || got[0] != previousPrefix+"s0 的要点" || items[0].SessionID != "s0" {
		t.Fatalf("上一会话应取暂存时刻最新的 s0：%v", got)
	}
	// 当前会话以外最新的是 s0.committed；recentPoints 对 .committed 返回 Source = committed
	if rp := s.recentPoints(dir, "s3"); len(rp) != 1 || rp[0].Source != memory.SourceCommitted {
		t.Fatalf("已提交文件的最近要点 Source 应为 committed：%+v", rp)
	}
}

// recentPoints：未提交文件的条目 Source = staged，取最后 10 条。
func TestRecentPointsStagedSource(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	for i := 1; i <= 12; i++ {
		stage(t, s, "s1", "b"+string(rune('a'+i)), i, memory.Point{Text: "第 " + string(rune('0'+i%10)) + " 条"})
	}
	rp := s.recentPoints(dir, "s2")
	if len(rp) != recentPointCount || rp[0].Text != previousPrefix+"第 3 条" || rp[9].Text != previousPrefix+"第 2 条" || rp[0].Source != memory.SourceStaged {
		t.Fatalf("最近要点不符：%+v", texts(rp))
	}
}

// 遗留提交：pruneCommitted 保留最后一批暂存时刻最新的 .committed，不以本次提交的文件为准。
func TestPruneKeepsNewestCommitted(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"})
	f.status["/memories"] = http.StatusInternalServerError
	if err := end(s, "s0"); err == nil {
		t.Fatal("s0 的 End 应失败")
	}
	delete(f.status, "/memories")
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "s1 的要点"})
	if err := end(s, "s1"); err != nil {
		t.Fatal(err)
	}
	start(t, s, "s2")
	s.bg.Wait()
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+committedExt {
		t.Fatalf("应保留最新的 s1.committed：%v", n)
	}
	if rp := s.recentPoints(dir, "s3"); len(rp) != 1 || rp[0].SessionID != "s1" {
		t.Fatalf("上一会话应为 s1：%+v", rp)
	}
}

// 两次 Start 的后台提交重叠时，同一遗留文件只 retain 一次，不输出 Error 日志。
func TestConcurrentLeftoverCommitOnce(t *testing.T) {
	f := newFake(t)
	f.gate = make(chan struct{})
	var logBuf bytes.Buffer
	s, dir := newServiceLog(t, f, slog.New(slog.NewTextHandler(&logBuf, nil)))
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"})
	start(t, s, "a")
	f.waitInflight(t, 1)
	start(t, s, "b")
	close(f.gate)
	s.bg.Wait()
	if f.retainCount() != 1 {
		t.Fatalf("同一文件应只 retain 1 次：%d", f.retainCount())
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s0"+committedExt {
		t.Fatalf("应保留 s0.committed：%v", n)
	}
	if strings.Contains(logBuf.String(), "leftover session commit failed") {
		t.Fatalf("不应输出提交失败日志：%s", logBuf.String())
	}
}

// ensureBank 的 PUT 不持锁：后台提交的慢 PUT 不阻塞 Recall 在自身期限内返回。
func TestEnsureBankDoesNotBlockRecall(t *testing.T) {
	f := newFake(t)
	f.putDelay = 400 * time.Millisecond
	s, _ := newService(t, f)
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, _ = s.Start(ctx, memory.SessionRequest{Namespace: "home", SessionID: "s1"})
	cancel()
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer rcancel()
	begin := time.Now()
	_, err := s.Recall(rctx, memory.RecallRequest{Namespace: "home", Query: "要点", Limit: 5})
	if elapsed := time.Since(begin); err == nil || elapsed > 250*time.Millisecond {
		t.Fatalf("Recall 应在自身期限内返回错误：err %v 耗时 %v", err, elapsed)
	}
	s.bg.Wait()
}

// Recall：引擎结果与本地暂存匹配合并，暂存标 staged，按时间升序。
func TestRecallMergesStaged(t *testing.T) {
	f := newFake(t)
	f.results["大阪"] = []any{result("小明去年去过大阪", "occurred_start", "2025-10-01T00:00:00Z", "document_id", "s0#1")}
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

// Recall：limit 与 reserve 的边界、同文本去重、无时间条目在后、暂存按匹配词数排序。
func TestRecallLimits(t *testing.T) {
	f := newFake(t)
	var engine []any
	for i := 0; i < 6; i++ {
		engine = append(engine, result("引擎 "+string(rune('A'+i)), "occurred_start", "2026-09-0"+string(rune('1'+i))+"T00:00:00Z"))
	}
	engine = append(engine, result("无时间的引擎条目"), result("大阪 演唱会 周日"))
	f.results["大阪"] = engine
	s, _ := newService(t, f)
	stage(t, s, "s1", "b1", 1,
		memory.Point{Text: "大阪"}, memory.Point{Text: "大阪 演唱会"}, memory.Point{Text: "大阪 演唱会 周日"},
		memory.Point{Text: "演唱会"}, memory.Point{Text: "周日大阪"})
	rc := func(limit int) []memory.Item {
		t.Helper()
		items, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "home", Query: "大阪 演唱会 周日", Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	if got := rc(1); len(got) != 1 || got[0].Source != memory.SourceCommitted || f.recalls[0].MaxTokens != 512 {
		t.Fatalf("limit 1：%v maxTokens %d", texts(got), f.recalls[0].MaxTokens)
	}
	got := rc(4)
	staged, committed := 0, 0
	for _, it := range got {
		if it.Source == memory.SourceStaged {
			staged++
		} else {
			committed++
		}
	}
	if len(got) != 4 || staged != 2 || committed != 2 {
		t.Fatalf("limit 4 应为引擎 2 条 + 暂存 2 条：%+v", got)
	}
	if got = rc(0); f.recalls[2].MaxTokens != 1200 || len(got) != 10 {
		t.Fatalf("limit 0 应取缺省 10：%v", texts(got))
	}
	// limit 20：引擎 8 条全部进入（含 2 条无时间），暂存补 4 条（同文本者只保留引擎条目），无时间条目在后
	got = rc(20)
	if f.recalls[3].MaxTokens != 2400 || len(got) != 12 || !got[10].At.IsZero() || !got[11].At.IsZero() || got[9].At.IsZero() {
		t.Fatalf("limit 20 的合并与排序不符：%v", texts(got))
	}
	for _, it := range got {
		if it.Text == "大阪 演唱会 周日" && it.Source != memory.SourceCommitted {
			t.Fatalf("同文本应只保留引擎条目：%+v", it)
		}
	}
	m := s.stagedMatches(filepath.Join(s.dir, "home"), "大阪 演唱会 周日")
	if strings.Join(texts(m), "|") != "大阪 演唱会 周日|大阪 演唱会|演唱会|周日大阪|大阪" {
		t.Fatalf("暂存匹配应按匹配词数降序：%v", texts(m))
	}
}

// 损坏的暂存文件：Stage 与 End 返回错误并保留文件；Recall 与 Start 跳过该文件。
func TestCorruptedStagedFile(t *testing.T) {
	f := newFake(t)
	s, dir := newService(t, f)
	stage(t, s, "s1", "b1", 1, memory.Point{Text: "好的一行"})
	path := filepath.Join(dir, "s1"+stagedExt)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString("{not json\n")
	fh.Close()
	if err := s.Stage(context.Background(), memory.StageRequest{Namespace: "home", SessionID: "s1", BatchID: "b2", Points: []memory.Point{{Text: "x"}}}); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("Stage 应返回损坏错误：%v", err)
	}
	if err := end(s, "s1"); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("End 应返回损坏错误：%v", err)
	}
	if n := names(t, dir); len(n) != 1 || n[0] != "s1"+stagedExt {
		t.Fatalf("损坏文件应保留：%v", n)
	}
	if items, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "home", Query: "好的"}); err != nil || len(items) != 0 {
		t.Fatalf("Recall 应跳过损坏文件：%v %v", items, err)
	}
	if items := start(t, s, "s2"); len(items) != 0 {
		t.Fatalf("Start 应跳过损坏文件：%v", texts(items))
	}
	s.bg.Wait()
}

// HTTP 错误：recall 非 2xx 与响应解码失败均返回错误。
func TestRecallHTTPErrors(t *testing.T) {
	f := newFake(t)
	f.status["/memories/recall"] = http.StatusInternalServerError
	s, _ := newService(t, f)
	if _, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "home", Query: "x"}); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("应返回 HTTP 500：%v", err)
	}
	delete(f.status, "/memories/recall")
	f.rawRecall = "not json"
	if _, err := s.Recall(context.Background(), memory.RecallRequest{Namespace: "home", Query: "x"}); err == nil || !strings.Contains(err.Error(), "响应解码失败") {
		t.Fatalf("应返回解码错误：%v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("顺序调用时 bank 只应 PUT 1 次：%v", f.puts)
	}
}

// Close：后台提交未结束时按 ctx 超时返回错误；结束后返回 nil。
func TestCloseWaitsForBackground(t *testing.T) {
	f := newFake(t)
	f.gate = make(chan struct{})
	s, _ := newService(t, f)
	stage(t, s, "s0", "a1", 1, memory.Point{Text: "s0 的要点"})
	start(t, s, "s1")
	f.waitInflight(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); err == nil {
		t.Fatal("提交未结束时 Close 应超时")
	}
	close(f.gate)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("提交结束后 Close 应返回 nil：%v", err)
	}
}

// queryTokens：CJK 二字组合与小写原词，长度 1 丢弃。
func TestQueryTokens(t *testing.T) {
	if got := strings.Join(queryTokens("大阪 演唱会 Alice x"), ","); got != "大阪,演唱,唱会,alice" {
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
	if _, err := s.Start(context.Background(), memory.SessionRequest{Namespace: "home", SessionID: ".."}); err == nil {
		t.Fatal("应拒绝 session_id ..")
	}
}
