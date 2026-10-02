// Package hindsight 是测试期的记忆服务实现（docs/memory/options.md「测试期后端」）：以 Hindsight
// （https://github.com/vectorize-io/hindsight）为检索引擎，暂存与会话级提交由本包在 ARiA 进程内完成，
// 对外实现 runtime/memory.Service。Hindsight 的 retain 完成后即可检索，没有暂存状态，因此：
//
//	Stage  要点写入 <Dir>/<namespace>/<session_id>.jsonl（每行一个批次，整体改写后原子改名），按 batch_id 去重
//	End    读取该会话全部批次，每批一个 retain item（document_id = <session_id>#<seq>，update_mode =
//	       replace，以 document_id 幂等）；承诺另成 item（tags = commitment，metadata.due = 到期时间）；
//	       一次 retain 成功后文件改名 .committed，同一 namespace 只保留最后一批暂存时刻最新的 1 个 .committed
//	Start  后台提交目录中未结束的会话（进程异常退出的遗留，不阻塞 Start），同时组装会话开始的召回：
//	       成员名单、每位成员的背景（并发 recall）、未到期的承诺（tags = commitment，按 metadata.due 判定）、
//	       上一会话的最近要点（current 以外最近一个会话的文件，已提交或未提交均计入）
//	Recall 调用 Hindsight recall，再合并本地未提交要点中与查询词匹配的条目（Source = staged）
//	Close  等待 Start 发起的后台提交结束
//
// bank_id = namespace，首次使用时 PUT 建立。合并由 Hindsight 的事实提取与 observation 整合承担。
package hindsight

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"aria/runtime/memory"
)

const (
	DefaultBaseURL = "http://127.0.0.1:8888" // Hindsight 单容器的 API 端口
	DefaultDir     = "memory-staging"        // 暂存目录缺省值（相对工作目录）

	httpTimeout = 120 * time.Second // 单次 HTTP 调用上限：同步 retain 含 LLM 提取

	// Start 返回内容的上限（docs/memory/options.md 已定第 8 项）。
	backgroundPerMember = 10   // 每位成员的背景条数
	backgroundMaxChars  = 1500 // 用户背景合计字数（rune）
	recentPointCount    = 10   // 上一会话的最近要点条数
	// commitmentWindow 是 Start 保留承诺的到期范围：metadata.due 在 [now, now + commitmentWindow] 内。
	commitmentWindow = 30 * 24 * time.Hour
	// commitmentTag 是承诺 item 的 tag，Start 据此检索。
	commitmentTag = "commitment"
	// recallConcurrency 是 Start 并发 recall 的上限。
	recallConcurrency = 4

	stagedExt      = ".jsonl"
	committedExt   = ".committed"
	retainContext  = "ARiA 家庭对话要点"
	previousPrefix = "上一会话："
	apiPrefix      = "/v1/default/banks/"
)

// Config 是适配层的参数。
type Config struct {
	BaseURL string           // Hindsight API 根地址；空 = DefaultBaseURL
	Dir     string           // 暂存目录；空 = DefaultDir
	Members []string         // 家庭成员名字：Start 返回名单并逐人检索身份、偏好与重要事实
	Now     func() time.Time // nil = time.Now
}

// Service 实现 memory.Service。文件操作与在途提交记录经 mu 串行；HTTP 调用不持锁。
type Service struct {
	base    string
	dir     string
	members []string
	http    *http.Client
	now     func() time.Time
	log     *slog.Logger

	mu         sync.Mutex
	committing map[string]struct{} // 在途提交的暂存文件路径（受 mu 保护）
	bankMu     sync.Mutex
	banks      map[string]bool // 已 PUT 建立的 bank（namespace，受 bankMu 保护）
	bg         sync.WaitGroup  // Start 发起的后台遗留提交
}

// New 建立适配层并创建暂存目录；log 为 nil 时使用 slog.Default()。
func New(cfg Config, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("hindsight: base_url %q 不合法", cfg.BaseURL)
	}
	dir := cfg.Dir
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("hindsight: 暂存目录建立失败: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		base: base, dir: dir, members: append([]string(nil), cfg.Members...),
		http: &http.Client{Timeout: httpTimeout}, now: now, log: log,
		committing: map[string]struct{}{}, banks: map[string]bool{},
	}, nil
}

// Close 等待 Start 发起的后台遗留提交结束；ctx 先结束时返回 ctx.Err()，提交继续在后台进行。
func (s *Service) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------- 暂存文件 ----------

// batchRecord 是暂存文件的一行：一个已暂存的批次。
type batchRecord struct {
	BatchID string        `json:"batch_id"`
	Seq     int           `json:"seq"`
	At      time.Time     `json:"at"` // 暂存时刻
	Points  []pointRecord `json:"points"`
}

type pointRecord struct {
	Text     string    `json:"text"`
	Kind     string    `json:"kind,omitempty"`
	Speakers []string  `json:"speakers,omitempty"`
	Due      time.Time `json:"due"`
}

// sessionFile 是目录中一个会话的文件：未提交（.jsonl）或已提交（.committed）。
type sessionFile struct {
	sid     string
	path    string
	staged  bool
	batches []batchRecord
}

// lastAt 返回最后一批的暂存时刻；没有批次时为零值。
func (f sessionFile) lastAt() time.Time {
	if len(f.batches) == 0 {
		return time.Time{}
	}
	return f.batches[len(f.batches)-1].At
}

// newer 判断 a 是否比 b 更新：最后一批暂存时刻较晚者；相同时取会话标识较大者。
func newer(a, b sessionFile) bool {
	if !a.lastAt().Equal(b.lastAt()) {
		return a.lastAt().After(b.lastAt())
	}
	return a.sid > b.sid
}

// pathComponent 校验 namespace 与 session_id 可作为文件名（不含路径分隔符与相对目录）。
func pathComponent(kind, s string) error {
	if s == "" || s == "." || s == ".." || s != filepath.Base(s) || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("hindsight: %s %q 不能作为文件名", kind, s)
	}
	return nil
}

// stagedPath 返回会话的暂存文件路径 <dir>/<namespace>/<session_id>.jsonl。
func (s *Service) stagedPath(ns, sid string) (string, error) {
	if err := pathComponent("namespace", ns); err != nil {
		return "", err
	}
	if err := pathComponent("session_id", sid); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, ns, sid+stagedExt), nil
}

// readBatches 读取一个暂存或已提交文件的全部批次；文件不存在时返回 fs.ErrNotExist。
func readBatches(path string) ([]batchRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []batchRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var b batchRecord
		if err := json.Unmarshal(line, &b); err != nil {
			return nil, fmt.Errorf("hindsight: 暂存文件 %s 损坏: %w", path, err)
		}
		out = append(out, b)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// writeBatches 把全部批次写入临时文件后改名为 path（整体替换，进程中断不产生半行）。
func writeBatches(path string, batches []batchRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, b := range batches {
		line, err := json.Marshal(b)
		if err != nil {
			f.Close()
			return err
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listSessions 读取 dir 中全部会话文件（.jsonl 与 .committed）及其批次；读取失败的文件输出 Warn 日志后跳过。
// 调用方持有 s.mu。
func (s *Service) listSessions(dir string) []sessionFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sessionFile
	for _, e := range entries {
		name := e.Name()
		var f sessionFile
		switch {
		case e.IsDir():
			continue
		case strings.HasSuffix(name, stagedExt):
			f = sessionFile{sid: strings.TrimSuffix(name, stagedExt), staged: true}
		case strings.HasSuffix(name, committedExt):
			f = sessionFile{sid: strings.TrimSuffix(name, committedExt)}
		default:
			continue
		}
		f.path = filepath.Join(dir, name)
		if f.batches, err = readBatches(f.path); err != nil {
			s.log.Warn("hindsight: read session file failed", "path", f.path, "err", err)
			continue
		}
		out = append(out, f)
	}
	return out
}

// Stage 把一批要点追加到该会话的暂存文件；同一 batch_id 第二次到达时不写入。
func (s *Service) Stage(_ context.Context, req memory.StageRequest) error {
	if len(req.Points) == 0 {
		return nil
	}
	path, err := s.stagedPath(req.Namespace, req.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batches, err := readBatches(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, b := range batches {
		if b.BatchID == req.BatchID {
			return nil
		}
	}
	rec := batchRecord{BatchID: req.BatchID, Seq: req.Seq, At: s.now(), Points: make([]pointRecord, 0, len(req.Points))}
	for _, p := range req.Points {
		rec.Points = append(rec.Points, pointRecord{Text: p.Text, Kind: string(p.Kind), Speakers: p.Speakers, Due: p.Due})
	}
	return writeBatches(path, append(batches, rec))
}

// End 把该会话的全部暂存批次一次 retain 到 Hindsight，成功后文件改名 .committed；没有暂存文件时不调用引擎。
func (s *Service) End(ctx context.Context, req memory.SessionRequest) error {
	path, err := s.stagedPath(req.Namespace, req.SessionID)
	if err != nil {
		return err
	}
	return s.commit(ctx, req.Namespace, req.SessionID, path)
}

// commit 读取 path 的批次并 retain（期间不持锁），成功后改名为 .committed 并清理更早的 .committed。
// 同一文件已有在途提交时直接返回（由先到者完成）；retain 期间文件新增了批次时返回错误，
// 由 Client.End 重试重新读取并以 replace 幂等 retain。
func (s *Service) commit(ctx context.Context, ns, sid, path string) error {
	s.mu.Lock()
	if _, busy := s.committing[path]; busy {
		s.mu.Unlock()
		return nil
	}
	batches, err := readBatches(path)
	if err == nil {
		s.committing[path] = struct{}{}
	}
	s.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		delete(s.committing, path)
		s.mu.Unlock()
	}()

	if items := retainItems(ns, sid, batches); len(items) > 0 {
		if err := s.ensureBank(ctx, ns); err != nil {
			return err
		}
		if err := s.do(ctx, http.MethodPost, apiPrefix+url.PathEscape(ns)+"/memories",
			retainRequest{Items: items, Async: false}, nil); err != nil {
			return fmt.Errorf("hindsight: retain 失败: %w", err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	again, err := readBatches(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(again) != len(batches) {
		return fmt.Errorf("hindsight: 会话 %s 在提交期间新增 %d 个批次，等待重试", sid, len(again)-len(batches))
	}
	if err := os.Rename(path, strings.TrimSuffix(path, stagedExt)+committedExt); err != nil {
		return err
	}
	s.pruneCommittedLocked(filepath.Dir(path))
	return nil
}

// pruneCommittedLocked 只保留 dir 中最新的 1 个 .committed（newer 规则），其余删除；调用方持有 s.mu。
func (s *Service) pruneCommittedLocked(dir string) {
	var committed []sessionFile
	for _, f := range s.listSessions(dir) {
		if !f.staged {
			committed = append(committed, f)
		}
	}
	if len(committed) == 0 {
		return
	}
	keep := committed[0]
	for _, f := range committed[1:] {
		if newer(f, keep) {
			keep = f
		}
	}
	for _, f := range committed {
		if f.path == keep.path {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			s.log.Warn("hindsight: remove old committed file failed", "path", f.path, "err", err)
		}
	}
}

// ---------- retain ----------

type retainRequest struct {
	Items []retainItem `json:"items"`
	Async bool         `json:"async"`
}

type retainItem struct {
	Content    string            `json:"content"`
	Context    string            `json:"context,omitempty"`
	Timestamp  string            `json:"timestamp,omitempty"`
	DocumentID string            `json:"document_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	UpdateMode string            `json:"update_mode,omitempty"`
}

var kindLabel = map[string]string{
	string(memory.KindFact):       "事实",
	string(memory.KindPreference): "偏好",
	string(memory.KindEvent):      "事件",
	string(memory.KindCommitment): "承诺",
}

// pointLine 把一条要点写成 retain 文本的一行：[类别] 文本（说话人：…）（到期 …）。
func pointLine(p pointRecord) string {
	var b strings.Builder
	if label, ok := kindLabel[p.Kind]; ok {
		b.WriteString("[" + label + "] ")
	}
	b.WriteString(p.Text)
	if len(p.Speakers) > 0 {
		b.WriteString("（说话人：" + strings.Join(p.Speakers, "、") + "）")
	}
	if p.Kind == string(memory.KindCommitment) && !p.Due.IsZero() {
		b.WriteString("（到期 " + p.Due.Format(time.RFC3339) + "）")
	}
	return b.String()
}

// retainItems 把会话的批次转为 retain items：每批一个 item（document_id = sid#seq，含该批全部要点，
// timestamp = 暂存时刻）；承诺另成 item（document_id = sid#seq#c<i>，tags = commitment，
// metadata.due = 到期时间的 RFC 3339 文本，无到期时间为空串）。
func retainItems(ns, sid string, batches []batchRecord) []retainItem {
	var items []retainItem
	for _, b := range batches {
		if len(b.Points) == 0 {
			continue
		}
		meta := map[string]string{"session_id": sid, "namespace": ns, "seq": strconv.Itoa(b.Seq)}
		lines := make([]string, 0, len(b.Points))
		for _, p := range b.Points {
			lines = append(lines, pointLine(p))
		}
		docID := sid + "#" + strconv.Itoa(b.Seq)
		at := b.At.Format(time.RFC3339)
		items = append(items, retainItem{
			Content: strings.Join(lines, "\n"), Context: retainContext, Timestamp: at,
			DocumentID: docID, Metadata: meta, UpdateMode: "replace",
		})
		for i, p := range b.Points {
			if p.Kind != string(memory.KindCommitment) {
				continue
			}
			cm := map[string]string{"session_id": sid, "namespace": ns, "seq": meta["seq"], "due": ""}
			if !p.Due.IsZero() {
				cm["due"] = p.Due.Format(time.RFC3339)
			}
			items = append(items, retainItem{
				Content: pointLine(p), Context: retainContext, Timestamp: at,
				DocumentID: docID + "#c" + strconv.Itoa(i), Metadata: cm, Tags: []string{commitmentTag},
				UpdateMode: "replace",
			})
		}
	}
	return items
}

// ---------- Start ----------

// Start 组装会话开始的召回：成员名单、成员背景、未到期的承诺、上一会话的最近要点（本地读取，先于引擎调用）。
// 遗留会话的提交在后台 goroutine 执行（同步 retain 含 LLM 提取，时长超过 Start 的超时；ctx 取消不影响提交），
// 提交前其要点仍可经 Recall 的暂存匹配召回。单次 recall 失败只跳过该项；引擎调用全部失败且没有本地条目时返回错误。
func (s *Service) Start(ctx context.Context, req memory.SessionRequest) ([]memory.Item, error) {
	path, err := s.stagedPath(req.Namespace, req.SessionID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.commitLeftovers(context.WithoutCancel(ctx), req.Namespace, req.SessionID, dir)
	}()

	recent := s.recentPoints(dir, req.SessionID)
	items, err := s.background(ctx, req.Namespace)
	if err != nil && len(recent) == 0 {
		return nil, err
	}
	return append(items, recent...), nil
}

// startQuery 是 Start 的一次 recall：label 用于日志，commitment 为真时按 tag 检索承诺。
type startQuery struct {
	label      string
	rq         recallRequest
	commitment bool
}

// background 组装用户背景：成员名单 1 条，每位成员最多 backgroundPerMember 条，承诺按 metadata.due 保留
// 空值或 [now, now + commitmentWindow] 内的条目；同文本只保留首条，合计不超过 backgroundMaxChars 字。
// recall 并发执行（上限 recallConcurrency），单次失败输出 Warn 日志并跳过；全部失败时返回首个错误。
func (s *Service) background(ctx context.Context, ns string) ([]memory.Item, error) {
	now := s.now()
	stamp := now.Format(time.RFC3339)
	queries := make([]startQuery, 0, len(s.members)+1)
	for _, m := range s.members {
		queries = append(queries, startQuery{label: m, rq: recallRequest{
			Query: m + "的身份、偏好与重要事实", Budget: "low", MaxTokens: 1024,
			PreferObservations: true, QueryTimestamp: stamp,
		}})
	}
	queries = append(queries, startQuery{label: commitmentTag, commitment: true, rq: recallRequest{
		Query: "ARiA 答应过的事与待办的提醒", Budget: "low", MaxTokens: 1024,
		Tags: []string{commitmentTag}, TagsMatch: "any", QueryTimestamp: stamp,
	}})

	results := make([][]hit, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, recallConcurrency)
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q startQuery) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = s.recall(ctx, ns, q.rq)
			if errs[i] != nil {
				s.log.Warn("hindsight: start recall failed", "query", q.label, "err", errs[i])
			}
		}(i, q)
	}
	wg.Wait()

	var items []memory.Item
	seen := map[string]bool{}
	chars := 0
	add := func(it memory.Item) bool {
		if seen[it.Text] {
			return false
		}
		n := utf8.RuneCountInString(it.Text)
		if chars+n > backgroundMaxChars {
			return false
		}
		seen[it.Text] = true
		chars += n
		items = append(items, it)
		return true
	}
	if len(s.members) > 0 {
		add(memory.Item{Text: "家庭成员：" + strings.Join(s.members, "、"), Source: memory.SourceCommitted})
	}
	var firstErr error
	failed := 0
	for i, q := range queries {
		if errs[i] != nil {
			failed++
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		n := 0
		for _, h := range results[i] {
			if q.commitment {
				if !s.commitmentDue(h, now) {
					continue
				}
			} else if n >= backgroundPerMember {
				break
			}
			if add(h.item) {
				n++
			}
		}
	}
	if failed == len(queries) {
		return items, firstErr
	}
	return items, nil
}

// commitmentDue 判断承诺是否保留：metadata.due 为空保留；解析成功时保留 [now, now + commitmentWindow] 内的；
// 解析失败输出 Warn 日志后保留。
func (s *Service) commitmentDue(h hit, now time.Time) bool {
	if h.due == "" {
		return true
	}
	due, err := time.Parse(time.RFC3339, h.due)
	if err != nil {
		s.log.Warn("hindsight: commitment due unparsable", "session", h.item.SessionID, "due", h.due)
		return true
	}
	return !due.Before(now) && !due.After(now.Add(commitmentWindow))
}

// commitLeftovers 提交 dir 中除 current 以外的暂存文件（此前未结束的会话）；失败只记日志，留待下一次 Start。
func (s *Service) commitLeftovers(ctx context.Context, ns, current, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, stagedExt) {
			continue
		}
		sid := strings.TrimSuffix(name, stagedExt)
		if sid == current {
			continue
		}
		if err := s.commit(ctx, ns, sid, filepath.Join(dir, name)); err != nil {
			s.log.Error("hindsight: leftover session commit failed", "session", sid, "err", err)
		}
	}
}

// recentPoints 返回 current 以外最新一个会话（newer 规则，已提交与未提交的文件均计入）的最后 recentPointCount
// 条要点，文本带「上一会话：」前缀；未提交文件的条目 Source = staged。
func (s *Service) recentPoints(dir, current string) []memory.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *sessionFile
	for _, f := range s.listSessions(dir) {
		if f.sid == current || len(f.batches) == 0 {
			continue
		}
		if best == nil || newer(f, *best) {
			f := f
			best = &f
		}
	}
	if best == nil {
		return nil
	}
	source := memory.SourceCommitted
	if best.staged {
		source = memory.SourceStaged
	}
	var all []memory.Item
	for _, b := range best.batches {
		for _, p := range b.Points {
			all = append(all, memory.Item{Text: previousPrefix + p.Text, Source: source, SessionID: best.sid, At: b.At})
		}
	}
	return all[max(0, len(all)-recentPointCount):]
}

// ---------- Recall ----------

type recallRequest struct {
	Query              string   `json:"query"`
	Budget             string   `json:"budget,omitempty"`
	MaxTokens          int      `json:"max_tokens,omitempty"`
	PreferObservations bool     `json:"prefer_observations,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	TagsMatch          string   `json:"tags_match,omitempty"`
	QueryTimestamp     string   `json:"query_timestamp,omitempty"`
}

type recallResponse struct {
	Results []recallResult `json:"results"`
}

type recallResult struct {
	Text          string         `json:"text"`
	OccurredStart *string        `json:"occurred_start"`
	MentionedAt   *string        `json:"mentioned_at"`
	DocumentID    *string        `json:"document_id"`
	Metadata      map[string]any `json:"metadata"`
}

// hit 是一条 recall 结果：映射后的条目与 metadata.due 原文（只对承诺 item 非空）。
type hit struct {
	item memory.Item
	due  string
}

// recall 调用 Hindsight recall 并映射结果（Source = committed；SessionID 取 metadata.session_id，缺省取
// document_id 的 # 前部分；At 取 occurred_start，缺省 mentioned_at），同文本只保留首条。
func (s *Service) recall(ctx context.Context, ns string, rq recallRequest) ([]hit, error) {
	if err := s.ensureBank(ctx, ns); err != nil {
		return nil, err
	}
	var resp recallResponse
	if err := s.do(ctx, http.MethodPost, apiPrefix+url.PathEscape(ns)+"/memories/recall", rq, &resp); err != nil {
		return nil, fmt.Errorf("hindsight: recall 失败: %w", err)
	}
	out := make([]hit, 0, len(resp.Results))
	seen := map[string]bool{}
	for _, r := range resp.Results {
		text := strings.TrimSpace(r.Text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		h := hit{item: memory.Item{Text: text, Source: memory.SourceCommitted}}
		if sid, ok := r.Metadata["session_id"].(string); ok {
			h.item.SessionID = sid
		} else if r.DocumentID != nil {
			h.item.SessionID, _, _ = strings.Cut(*r.DocumentID, "#")
		}
		h.due, _ = r.Metadata["due"].(string)
		if h.item.At = parseTime(r.OccurredStart); h.item.At.IsZero() {
			h.item.At = parseTime(r.MentionedAt)
		}
		out = append(out, h)
	}
	return out, nil
}

func parseTime(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, *s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Recall 在 Hindsight 与本地未提交的暂存中检索：引擎结果最多 limit − reserve 条（reserve = min(暂存匹配数,
// limit/2)），暂存匹配按匹配词数补足至 limit；合并后按时间升序，无时间的条目在后。
func (s *Service) Recall(ctx context.Context, req memory.RecallRequest) ([]memory.Item, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = memory.DefaultRecallToolLimit
	}
	if err := pathComponent("namespace", req.Namespace); err != nil {
		return nil, err
	}
	hits, err := s.recall(ctx, req.Namespace, recallRequest{
		Query: req.Query, Budget: "mid", MaxTokens: min(max(limit*120, 512), 4096),
		QueryTimestamp: s.now().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	staged := s.stagedMatches(filepath.Join(s.dir, req.Namespace), req.Query)
	reserve := min(len(staged), limit/2)
	seen := map[string]bool{}
	out := make([]memory.Item, 0, limit)
	for _, h := range hits[:min(len(hits), limit-reserve)] {
		seen[h.item.Text] = true
		out = append(out, h.item)
	}
	for _, it := range staged {
		if len(out) >= limit {
			break
		}
		if !seen[it.Text] {
			seen[it.Text] = true
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].At, out[j].At
		switch {
		case a.IsZero():
			return false
		case b.IsZero():
			return true
		default:
			return a.Before(b)
		}
	})
	return out, nil
}

// stagedMatches 返回 dir 中全部未提交要点里与 query 匹配的条目（Source = staged），按匹配词数降序、时间降序。
func (s *Service) stagedMatches(dir, query string) []memory.Item {
	tokens := queryTokens(query)
	if len(tokens) == 0 {
		return nil
	}
	type scored struct {
		it    memory.Item
		score int
	}
	var cands []scored
	s.mu.Lock()
	for _, f := range s.listSessions(dir) {
		if !f.staged {
			continue
		}
		for _, b := range f.batches {
			for _, p := range b.Points {
				lower := strings.ToLower(p.Text)
				n := 0
				for _, tk := range tokens {
					if strings.Contains(lower, tk) {
						n++
					}
				}
				if n > 0 {
					cands = append(cands, scored{memory.Item{Text: p.Text, Source: memory.SourceStaged, SessionID: f.sid, At: b.At}, n})
				}
			}
		}
	}
	s.mu.Unlock()
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].it.At.After(cands[j].it.At)
	})
	out := make([]memory.Item, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.it)
	}
	return out
}

// queryTokens 把查询拆成匹配词：按非字母数字切分；CJK 片段取全部二字组合，其余片段取小写原词；长度 1 的丢弃。
func queryTokens(q string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if utf8.RuneCountInString(t) >= 2 && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	isHan := func(r rune) bool { return unicode.Is(unicode.Han, r) }
	for _, field := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		rs := []rune(field)
		for i := 0; i+1 < len(rs); i++ {
			if isHan(rs[i]) && isHan(rs[i+1]) {
				add(string(rs[i : i+2]))
			}
		}
		for _, w := range strings.FieldsFunc(field, isHan) {
			add(w)
		}
	}
	return out
}

// ---------- HTTP ----------

// ensureBank 首次使用某 namespace 时 PUT 建立同名 bank；HTTP 调用不持锁，并发的首次调用各自 PUT 一次
// （重复 PUT 为更新，无副作用）。
func (s *Service) ensureBank(ctx context.Context, ns string) error {
	s.bankMu.Lock()
	ok := s.banks[ns]
	s.bankMu.Unlock()
	if ok {
		return nil
	}
	if err := s.do(ctx, http.MethodPut, apiPrefix+url.PathEscape(ns), map[string]string{"name": ns}, nil); err != nil {
		return fmt.Errorf("hindsight: 建立 bank %q 失败: %w", ns, err)
	}
	s.bankMu.Lock()
	s.banks[ns] = true
	s.bankMu.Unlock()
	return nil
}

// do 发送 JSON 请求；非 2xx 返回含状态码与响应前 300 字节的错误；out 非 nil 时解码响应。
func (s *Service) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, data[:min(len(data), 300)])
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: 响应解码失败: %w", method, path, err)
		}
	}
	return nil
}
