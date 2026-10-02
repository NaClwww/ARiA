// Package hindsight 是测试期的记忆服务实现（docs/memory/options.md「测试期后端」）：以 Hindsight
// （https://github.com/vectorize-io/hindsight）为检索引擎，暂存与会话级提交由本包在 ARiA 进程内完成，
// 对外实现 runtime/memory.Service。Hindsight 的 retain 完成后即可检索，没有暂存状态，因此：
//
//	Stage  要点追加写入 <Dir>/<namespace>/<session_id>.jsonl（每行一个批次），按 batch_id 去重
//	End    读取该会话全部批次，每批一个 retain item（document_id = <session_id>#<seq>，update_mode =
//	       replace，以 document_id 幂等）；承诺另成 item（tags = commitment、timestamp = 到期时间）；
//	       一次 retain 成功后文件改名 .committed，同一 namespace 只保留最近 1 个 .committed
//	Start  后台提交目录中未结束的会话（进程异常退出的遗留，不阻塞 Start），同时组装会话开始的召回：
//	       每位成员的背景（一次 recall）、未到期的承诺（tags = commitment）、上一会话的最近要点
//	       （current 以外最近一个会话的文件，已提交或未提交均计入）
//	Recall 调用 Hindsight recall，再合并本地未提交要点中与查询词匹配的条目（Source = staged）
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
	DefaultBaseURL     = "http://127.0.0.1:8888" // Hindsight 单容器的 API 端口
	DefaultDir         = "memory-staging"        // 暂存目录缺省值（相对工作目录）
	DefaultHTTPTimeout = 120 * time.Second       // 单次 HTTP 调用上限：同步 retain 含 LLM 提取

	// Start 返回内容的上限（docs/memory/options.md 已定第 8 项）。
	BackgroundPerMember = 10   // 每位成员的背景条数
	BackgroundMaxChars  = 1500 // 用户背景合计字数（rune）
	RecentPoints        = 10   // 上一会话的最近要点条数
	// CommitmentWindow 是 Start 查询未到期承诺的时间范围：到期时间在 [now, now + CommitmentWindow] 内。
	CommitmentWindow = 30 * 24 * time.Hour
	// CommitmentTag 是承诺 item 的 tag，Start 据此检索。
	CommitmentTag = "commitment"
	// DefaultRecallLimit 是 Recall 请求 Limit <= 0 时的条数。
	DefaultRecallLimit = 10

	stagedExt      = ".jsonl"
	committedExt   = ".committed"
	retainContext  = "ARiA 家庭对话要点"
	previousPrefix = "上一会话："
	apiPrefix      = "/v1/default/banks/"
)

// Config 是适配层的参数。
type Config struct {
	BaseURL    string       // Hindsight API 根地址；空 = DefaultBaseURL
	Dir        string       // 暂存目录；空 = DefaultDir
	Members    []string     // 家庭成员名字：Start 时逐人检索身份、偏好与重要事实
	HTTPClient *http.Client // nil = 超时 DefaultHTTPTimeout 的客户端
	Now        func() time.Time
}

// Service 实现 memory.Service。文件操作经 mu 串行；HTTP 调用不持锁。
type Service struct {
	base    string
	dir     string
	members []string
	http    *http.Client
	now     func() time.Time
	log     *slog.Logger

	mu     sync.Mutex
	bankMu sync.Mutex
	banks  map[string]bool // 已 PUT 建立的 bank（namespace）
	bg     sync.WaitGroup  // Start 发起的后台遗留提交
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
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		base: base, dir: dir, members: append([]string(nil), cfg.Members...),
		http: hc, now: now, log: log, banks: map[string]bool{},
	}, nil
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

// pathComponent 校验 namespace 与 session_id 可作为文件名（不含路径分隔符与相对目录）。
func pathComponent(kind, s string) error {
	if s == "" || s == "." || s == ".." || s != filepath.Base(s) || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("hindsight: %s %q 不能作为文件名", kind, s)
	}
	return nil
}

func (s *Service) nsDir(ns string) (string, error) {
	if err := pathComponent("namespace", ns); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, ns), nil
}

func (s *Service) stagedPath(ns, sid string) (string, error) {
	dir, err := s.nsDir(ns)
	if err != nil {
		return "", err
	}
	if err := pathComponent("session_id", sid); err != nil {
		return "", err
	}
	return filepath.Join(dir, sid+stagedExt), nil
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
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// End 把该会话的全部暂存批次一次 retain 到 Hindsight，成功后文件改名 .committed；没有暂存文件时不调用引擎。
func (s *Service) End(ctx context.Context, req memory.SessionRequest) error {
	path, err := s.stagedPath(req.Namespace, req.SessionID)
	if err != nil {
		return err
	}
	return s.commit(ctx, req.Namespace, req.SessionID, path)
}

// commit 读取 path 的批次并 retain；retain 期间不持锁。成功后改名为 .committed 并删除同目录更早的 .committed。
func (s *Service) commit(ctx context.Context, ns, sid, path string) error {
	s.mu.Lock()
	batches, err := readBatches(path)
	s.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
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
	dst := strings.TrimSuffix(path, stagedExt) + committedExt
	_ = os.Remove(dst)
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	s.pruneCommitted(filepath.Dir(path), dst)
	return nil
}

// pruneCommitted 删除 dir 中除 keep 以外的 .committed 文件（调用方持有 s.mu）。
func (s *Service) pruneCommitted(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || !strings.HasSuffix(e.Name(), committedExt) || p == keep {
			continue
		}
		if err := os.Remove(p); err != nil {
			s.log.Warn("hindsight: remove old committed file failed", "path", p, "err", err)
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

// retainItems 把会话的批次转为 retain items：每批一个 item（document_id = sid#seq，含该批全部要点），
// 承诺另成 item（document_id = sid#seq#c<i>，tags = commitment，timestamp = 到期时间）。
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
		items = append(items, retainItem{
			Content: strings.Join(lines, "\n"), Context: retainContext, Timestamp: b.At.Format(time.RFC3339),
			DocumentID: docID, Metadata: meta, UpdateMode: "replace",
		})
		for i, p := range b.Points {
			if p.Kind != string(memory.KindCommitment) {
				continue
			}
			at := b.At
			if !p.Due.IsZero() {
				at = p.Due
			}
			items = append(items, retainItem{
				Content: pointLine(p), Context: retainContext, Timestamp: at.Format(time.RFC3339),
				DocumentID: docID + "#c" + strconv.Itoa(i), Metadata: meta, Tags: []string{CommitmentTag},
				UpdateMode: "replace",
			})
		}
	}
	return items
}

// ---------- Start ----------

// Start 组装会话开始的召回：成员背景、未到期的承诺、上一会话的最近要点。遗留会话的提交在后台 goroutine
// 执行（同步 retain 含 LLM 提取，时长超过 Start 的超时；ctx 取消不影响提交），提交前其要点仍可经 Recall
// 的暂存匹配召回。引擎调用失败时返回错误（agent 侧按 Start 失败处理：新会话不带召回）。
func (s *Service) Start(ctx context.Context, req memory.SessionRequest) ([]memory.Item, error) {
	dir, err := s.nsDir(req.Namespace)
	if err != nil {
		return nil, err
	}
	if err := pathComponent("session_id", req.SessionID); err != nil {
		return nil, err
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.commitLeftovers(context.WithoutCancel(ctx), req.Namespace, req.SessionID, dir)
	}()

	now := s.now()
	var items []memory.Item
	seen := map[string]bool{}
	chars := 0
	add := func(it memory.Item) bool {
		if seen[it.Text] {
			return false
		}
		n := utf8.RuneCountInString(it.Text)
		if chars+n > BackgroundMaxChars {
			return false
		}
		seen[it.Text] = true
		chars += n
		items = append(items, it)
		return true
	}
	for _, m := range s.members {
		got, err := s.recall(ctx, req.Namespace, recallRequest{
			Query: m + "的身份、偏好与重要事实", Budget: "low", MaxTokens: 1024,
			PreferObservations: true, QueryTimestamp: now.Format(time.RFC3339),
		})
		if err != nil {
			return nil, err
		}
		n := 0
		for _, it := range got {
			if n >= BackgroundPerMember {
				break
			}
			if add(it) {
				n++
			}
		}
	}
	got, err := s.recall(ctx, req.Namespace, recallRequest{
		Query: "ARiA 答应过的事与待办的提醒", Budget: "low", MaxTokens: 1024,
		Tags: []string{CommitmentTag}, TagsMatch: "any", QueryTimestamp: now.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	for _, it := range got {
		if it.At.IsZero() || (!it.At.Before(now) && !it.At.After(now.Add(CommitmentWindow))) {
			add(it)
		}
	}
	items = append(items, s.recentPoints(dir, req.SessionID)...)
	return items, nil
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

// recentPoints 返回 current 以外最近一个会话的最后 RecentPoints 条要点，文本带「上一会话：」前缀。
// 已提交（.committed）与未提交（.jsonl）的文件均计入，以最后一批的暂存时刻为准，相同时取会话标识较大者；
// 未提交文件的条目 Source = staged。
func (s *Service) recentPoints(dir, current string) []memory.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var bestSID string
	var bestAt time.Time
	var best []batchRecord
	bestSource := memory.SourceCommitted
	for _, e := range entries {
		name := e.Name()
		var sid string
		source := memory.SourceCommitted
		switch {
		case e.IsDir():
			continue
		case strings.HasSuffix(name, stagedExt):
			sid, source = strings.TrimSuffix(name, stagedExt), memory.SourceStaged
		case strings.HasSuffix(name, committedExt):
			sid = strings.TrimSuffix(name, committedExt)
		default:
			continue
		}
		if sid == current {
			continue
		}
		batches, err := readBatches(filepath.Join(dir, name))
		if err != nil {
			s.log.Warn("hindsight: read session file failed", "path", name, "err", err)
			continue
		}
		if len(batches) == 0 {
			continue
		}
		at := batches[len(batches)-1].At
		if best == nil || at.After(bestAt) || (at.Equal(bestAt) && sid > bestSID) {
			bestSID, bestAt, best, bestSource = sid, at, batches, source
		}
	}
	var all []memory.Item
	for _, b := range best {
		for _, p := range b.Points {
			all = append(all, memory.Item{Text: previousPrefix + p.Text, Source: bestSource, SessionID: bestSID, At: b.At})
		}
	}
	if len(all) > RecentPoints {
		all = all[len(all)-RecentPoints:]
	}
	return all
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
	Tags          []string       `json:"tags"`
}

// recall 调用 Hindsight recall 并把结果映射为 memory.Item（Source = committed），同文本只保留首条。
func (s *Service) recall(ctx context.Context, ns string, rq recallRequest) ([]memory.Item, error) {
	if err := s.ensureBank(ctx, ns); err != nil {
		return nil, err
	}
	var resp recallResponse
	if err := s.do(ctx, http.MethodPost, apiPrefix+url.PathEscape(ns)+"/memories/recall", rq, &resp); err != nil {
		return nil, fmt.Errorf("hindsight: recall 失败: %w", err)
	}
	out := make([]memory.Item, 0, len(resp.Results))
	seen := map[string]bool{}
	for _, r := range resp.Results {
		text := strings.TrimSpace(r.Text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		it := memory.Item{Text: text, Source: memory.SourceCommitted}
		if sid, ok := r.Metadata["session_id"].(string); ok {
			it.SessionID = sid
		} else if r.DocumentID != nil {
			it.SessionID, _, _ = strings.Cut(*r.DocumentID, "#")
		}
		it.At = parseTime(r.OccurredStart)
		if it.At.IsZero() {
			it.At = parseTime(r.MentionedAt)
		}
		out = append(out, it)
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

// Recall 在 Hindsight 与本地未提交的暂存中检索：引擎结果最多 limit − reserve 条，暂存匹配最多 reserve 条
// （reserve = min(暂存匹配数, limit/2)），合并后按时间升序，无时间的条目在后。
func (s *Service) Recall(ctx context.Context, req memory.RecallRequest) ([]memory.Item, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultRecallLimit
	}
	dir, err := s.nsDir(req.Namespace)
	if err != nil {
		return nil, err
	}
	maxTokens := limit * 120
	if maxTokens < 512 {
		maxTokens = 512
	} else if maxTokens > 4096 {
		maxTokens = 4096
	}
	engine, err := s.recall(ctx, req.Namespace, recallRequest{
		Query: req.Query, Budget: "mid", MaxTokens: maxTokens, QueryTimestamp: s.now().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	staged := s.stagedMatches(dir, req.Query)
	reserve := len(staged)
	if reserve > limit/2 {
		reserve = limit / 2
	}
	if len(engine) > limit-reserve {
		engine = engine[:limit-reserve]
	}
	seen := map[string]bool{}
	out := make([]memory.Item, 0, limit)
	for _, it := range engine {
		seen[it.Text] = true
		out = append(out, it)
	}
	for _, it := range staged {
		if len(out) >= limit {
			break
		}
		if seen[it.Text] {
			continue
		}
		seen[it.Text] = true
		out = append(out, it)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type scored struct {
		it    memory.Item
		score int
	}
	var cands []scored
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, stagedExt) {
			continue
		}
		batches, err := readBatches(filepath.Join(dir, name))
		if err != nil {
			s.log.Warn("hindsight: read staged file failed", "path", name, "err", err)
			continue
		}
		sid := strings.TrimSuffix(name, stagedExt)
		for _, b := range batches {
			for _, p := range b.Points {
				lower := strings.ToLower(p.Text)
				n := 0
				for _, tk := range tokens {
					if strings.Contains(lower, tk) {
						n++
					}
				}
				if n > 0 {
					cands = append(cands, scored{memory.Item{Text: p.Text, Source: memory.SourceStaged, SessionID: sid, At: b.At}, n})
				}
			}
		}
	}
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
	for _, field := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		runes := []rune(field)
		var cjk []rune
		flush := func() {
			for i := 0; i+1 < len(cjk); i++ {
				add(string(cjk[i : i+2]))
			}
			cjk = cjk[:0]
		}
		var other []rune
		for _, r := range runes {
			if unicode.Is(unicode.Han, r) {
				if len(other) > 0 {
					add(string(other))
					other = other[:0]
				}
				cjk = append(cjk, r)
			} else {
				flush()
				other = append(other, r)
			}
		}
		flush()
		if len(other) > 0 {
			add(string(other))
		}
	}
	return out
}

// ---------- HTTP ----------

// ensureBank 首次使用某 namespace 时 PUT 建立同名 bank（重复 PUT 为更新，无副作用）。
func (s *Service) ensureBank(ctx context.Context, ns string) error {
	s.bankMu.Lock()
	defer s.bankMu.Unlock()
	if s.banks[ns] {
		return nil
	}
	if err := s.do(ctx, http.MethodPut, apiPrefix+url.PathEscape(ns), map[string]string{"name": ns}, nil); err != nil {
		return fmt.Errorf("hindsight: 建立 bank %q 失败: %w", ns, err)
	}
	s.banks[ns] = true
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
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: 响应解码失败: %w", method, path, err)
		}
	}
	return nil
}
