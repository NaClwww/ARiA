// Package window 负责每轮上下文组装（docs/03 §5）：
//
//	每轮进 core 的历史 = 压缩记忆 + 未压缩近轮 + 新输入
//
// 压缩（一次 LLM 调用）不在组装快路径上：轮次结束后由 Settle 在间隙里异步做，
// 未就绪时下一轮继续用旧记忆——慢一点，但组装路径永远没有 LLM 调用。
//
// 压缩按上下文用量触发（见 Budget）：上下文剩余量低于预留量时，压缩触发时刻
// 近轮中除最近 K 轮以外的轮次；压缩进行中结算的轮次不计入 K，保留原文。
// 会话切换时由 Flush 触发：切换前结算的近轮全部压缩，不保留最近 K 轮。
package window

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"

	"aria/core/loop"
	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// Budget 是按上下文用量触发压缩的预算（单位 token）。
type Budget struct {
	// ContextWindow 是模型上下文窗口；≤ 0 时不按用量触发压缩，组装也不截断。
	ContextWindow int
	// MaxOutput 是单次请求的输出上限：组装结果超过 ContextWindow − MaxOutput 时，
	// 组装截断最早的原文（压缩进行中上下文继续增长的情形）。
	MaxOutput int
	// Reserve 是预留量：剩余量（ContextWindow − 上下文用量）低于该值时触发压缩。
	Reserve int
	// Count 估算消息的 token 数（无 usage 时与压缩完成后判定用量）；nil 时用 provider.EstimateTokens。
	Count func([]message.Message) int
}

func (b Budget) count(ms []message.Message) int {
	if b.Count != nil {
		return b.Count(ms)
	}
	return provider.EstimateTokens(ms)
}

const (
	// DefaultKeepLast 是无 Compressor 时的兜底保留条数。
	DefaultKeepLast = 40

	// defaultFallbackCap 是压缩持续失败时的原始消息上限（防止无界增长）。
	defaultFallbackCap = 4 * DefaultKeepLast
)

// Compressor 把「旧记忆 + 本轮消息」压成新记忆（03 §5 间隙压缩）。
//
// 契约：输出不得为空——空输出按失败处理（窗口退回未压缩形态，绝不静默清空）。
// 实现必须尊重 ctx 取消：窗口 Close 会取消在途压缩。
type Compressor interface {
	Compress(ctx context.Context, memory, turn []message.Message) ([]message.Message, error)
}

// Window 持有单个会话的上下文状态。用 New 构造。
type Window struct {
	compressor  Compressor
	log         *slog.Logger
	fallbackCap int
	system      string // 每轮置顶的 system 内容（可信文本；装配期设置）

	mu          sync.Mutex
	memory      []message.Message // 已压缩的记忆
	inflight    []message.Message // 正被压缩的消息：仍参与组装，压完才被替换
	recent      []message.Message // 尚未压缩的近轮（此前输入与产出）
	recentTurns []int             // recent 中各轮的消息条数，按结算顺序；按轮切分压缩范围用
	keepTurns   int               // 压缩时保留原文的近轮数 K，见 SetKeepRecentTurns
	budget      Budget            // 按用量触发压缩的预算，见 SetBudget
	used        int               // 最近一次判定得到的上下文用量（token）
	flushTurns  int               // recent 开头须全部压缩的轮数（Flush 设置；压缩取走后递减）
	compressing bool
	pendingCtx  context.Context // 触发待压缩批次的 Settle 的 ctx：下一轮压缩换用它的身份
	onCompress  func(context.Context, loop.WindowCompressedData)
	closed      bool
	cancel      context.CancelFunc // 在途压缩的取消（Close 用）
	compDone    chan struct{}      // 在途压缩的完成信号：结束即关闭（Wait 用）
}

// New 构造窗口；compressor 为 nil 时退化为 KeepLast(DefaultKeepLast)。
func New(compressor Compressor, log *slog.Logger) *Window {
	if compressor == nil {
		compressor = KeepLast(DefaultKeepLast)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Window{compressor: compressor, log: log, fallbackCap: defaultFallbackCap}
}

// SetSystem 设置每轮置顶的 system 内容（宿主人设/规则/标签声明；装配期调用，
// 之后不应再改）。只有它是「指令位」——不可信内容一律不进 system（03 §5）。
func (w *Window) SetSystem(text string) {
	w.mu.Lock()
	w.system = text
	w.mu.Unlock()
}

// SetOnCompress 注册压缩观测（装配期调用）：每次间隙压缩结束后（成功或
// 失败）在压缩 goroutine 上同步回调，报告规模与结果；ctx 为该批压缩使用的 ctx
// （值取自触发该批的 Settle 或 Flush，其 Scope 即该批内容所属的会话）。回调必须快、
// 不得再调窗口方法（会死锁）；nil 关闭。window_compressed 审计事件经此回调由 Session
// 写入 Store——窗口本身不认识事件总线。
func (w *Window) SetOnCompress(fn func(ctx context.Context, rep loop.WindowCompressedData)) {
	w.mu.Lock()
	w.onCompress = fn
	w.mu.Unlock()
}

// SetKeepRecentTurns 设置压缩时保留原文的近轮数 K（负数按 0 处理；运行期调用时自下一次压缩起生效）。
// 压缩取触发时刻近轮中除最近 K 轮以外的轮次；近轮不超过 K 轮时全部压缩。
// 压缩以轮为单位切分，一轮内的工具调用与结果不会被拆开。
func (w *Window) SetKeepRecentTurns(k int) {
	if k < 0 {
		k = 0
	}
	w.mu.Lock()
	w.keepTurns = k
	w.mu.Unlock()
}

// SetBudget 设置按上下文用量触发压缩的预算；自下一次 Settle 起生效。
// 未设置（ContextWindow ≤ 0）时窗口不压缩。
func (w *Window) SetBudget(b Budget) {
	w.mu.Lock()
	w.budget = b
	w.mu.Unlock()
}

// dueLocked 报告是否满足按用量压缩的触发条件（调用方持有 w.mu）：
// 有近轮可压缩，且剩余量（ContextWindow − used）低于 Reserve。
func (w *Window) dueLocked() bool {
	b := w.budget
	return b.ContextWindow > 0 && len(w.recentTurns) > 0 && b.ContextWindow-w.used < b.Reserve
}

// cutTurnsLocked 返回下一批压缩从 recent 开头取走的轮数（调用方持有 w.mu）：
// 满足用量触发条件时取除最近 K 轮以外的轮次（近轮不超过 K 轮时全部取走），
// 再与 Flush 要求的轮数取较大值；两个条件均不成立时为 0。
func (w *Window) cutTurnsLocked() int {
	n := 0
	if w.dueLocked() {
		n = len(w.recentTurns) - w.keepTurns
		if n <= 0 {
			n = len(w.recentTurns)
		}
	}
	return min(max(n, w.flushTurns), len(w.recentTurns))
}

// estimateLocked 按 budget.Count 估算当前上下文（system、记忆、在途与近轮）的 token 数（调用方持有 w.mu）。
func (w *Window) estimateLocked() int {
	ms := make([]message.Message, 0, len(w.memory)+len(w.inflight)+len(w.recent)+1)
	if w.system != "" {
		ms = append(ms, message.NewSystem(w.system))
	}
	ms = append(ms, w.memory...)
	ms = append(ms, w.inflight...)
	ms = append(ms, w.recent...)
	return w.budget.count(ms)
}

// SetCompressor 热替换压缩策略（03 §5 组装层的零件位；配置点菜 / 网页面板）。
//
// 语义：**在途压缩用旧实现跑完**——不打断、结果照常落下（它可能已经调了
// provider，中途换掉只会白花钱且丢结果）；下一个轮间隙自然用新实现。
// nil 视为「不压缩」，退化为 KeepLast(DefaultKeepLast)（与 New 一致）。
func (w *Window) SetCompressor(c Compressor) {
	if c == nil {
		c = KeepLast(DefaultKeepLast)
	}
	w.mu.Lock()
	w.compressor = c
	w.mu.Unlock()
}

// Assemble 产出本轮进 core 的完整历史。全程深拷贝——窗口不把内部切片
// 借给 core 或宿主（01 §2 消息所有权纪律）。
//
// 顺序固定（03 §5 分层规范，稳定前缀利于 provider 缓存）：
//
//	[system] → [<memory>] → 近轮 → 新输入
//
// inflight（正被压缩的消息）也参与组装：压缩在途时用户又说了话，
// 那些消息必须仍在上下文里，否则会短暂失忆。
//
// 设置了 Budget 时，组装结果的估算超过 ContextWindow − MaxOutput 则从最早的原文
// （inflight 与近轮）开始略去，直至不超过；只影响本次组装结果，窗口状态不变。
func (w *Window) Assemble(input []message.Message) []message.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := make([]message.Message, 0, len(w.memory)+1)
	if w.system != "" {
		head = append(head, message.NewSystem(w.system))
	}
	head = append(head, cloneAll(w.memory)...)
	raw := append(cloneAll(w.inflight), cloneAll(w.recent)...)
	tail := cloneAll(input)
	raw = w.fitLocked(head, raw, tail)

	out := make([]message.Message, 0, len(head)+len(raw)+len(tail))
	out = append(out, head...)
	out = append(out, raw...)
	return append(out, tail...)
}

// fitLocked 在组装结果超过硬上限（ContextWindow − MaxOutput）时略去 raw 开头的消息（调用方持有 w.mu）。
// 略去后开头若是失去配对调用的工具结果，一并略去。head 与 tail 本身超限时不再处理，由 provider 报错。
func (w *Window) fitLocked(head, raw, tail []message.Message) []message.Message {
	b := w.budget
	limit := b.ContextWindow - b.MaxOutput
	if b.ContextWindow <= 0 || limit <= 0 || len(raw) == 0 {
		return raw
	}
	fixed := b.count(head) + b.count(tail)
	sizes := make([]int, len(raw))
	total := fixed
	for i := range raw {
		sizes[i] = b.count(raw[i : i+1])
		total += sizes[i]
	}
	if total <= limit {
		return raw
	}
	drop := 0
	for drop < len(raw) && (total > limit || raw[drop].Role == message.RoleTool) {
		total -= sizes[drop]
		drop++
	}
	w.log.Warn("window: assembled context over limit, oldest raw messages omitted",
		"omitted", drop, "estimated_tokens", total, "limit", limit)
	return raw[drop:]
}

// Settle 在一轮结束后把本轮消息并入窗口，上下文用量达到触发条件时启动间隙压缩。
// used 是本轮结束时的上下文用量（最后一次请求的输入与输出 token 之和）；
// ≤ 0 表示没有 usage，按 Budget.Count 估算。
// 非阻塞：压缩在后台进行，下一轮组装不等待它（03 §5）。
func (w *Window) Settle(ctx context.Context, turn []message.Message, used int) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.recent = append(w.recent, cloneAll(turn)...)
	w.recentTurns = append(w.recentTurns, len(turn))
	if w.compressing {
		// 压缩在途：本轮不计入在途压缩的保留量 K，原文保留；压缩完成后按完成时的
		// 估算用量重新判定。记下触发本批的 ctx——下一次压缩的身份（Scope/凭据/参数）
		// 属于这批内容的主人，不能沿用上一轮的（2026-09-28 审查：B 的内容曾以 A 的 UserID 压缩）。
		if w.pendingCtx == nil && ctx != nil {
			w.pendingCtx = ctx
		}
		w.mu.Unlock()
		return
	}
	switch {
	case w.budget.ContextWindow <= 0:
		w.used = 0
	case used > 0:
		w.used = used
	default:
		w.used = w.estimateLocked()
	}
	if w.cutTurnsLocked() == 0 { // 剩余量不低于预留量且无待执行的 Flush：继续以原文参与组装
		w.mu.Unlock()
		return
	}
	w.startLocked(ctx)
	w.mu.Unlock()
}

// Flush 把调用时刻的近轮全部压缩进记忆，不保留最近 K 轮，与上下文用量无关；用于会话切换。
// 非阻塞：与 Settle 相同在后台压缩。压缩在途时记下要求的轮数，在途批次结束后接着压缩；
// Flush 之后结算的轮次不在要求之内，保留原文。压缩失败时该批按失败回退并入记忆，要求中
// 尚未取走的轮数保留，由下一次 Settle 重新发起。
// ctx 的值（Scope/凭据/参数）用于这批压缩，应取自被压缩内容所属的会话。
func (w *Window) Flush(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || len(w.recentTurns) == 0 {
		return
	}
	w.flushTurns = len(w.recentTurns)
	if w.compressing {
		if w.pendingCtx == nil && ctx != nil {
			w.pendingCtx = ctx
		}
		return
	}
	w.startLocked(ctx)
}

// startLocked 标记压缩在途并启动压缩 goroutine（调用方持有 w.mu）。
// cancel 与 compDone 必须在持锁时注册：否则调用方返回后、压缩 goroutine 注册前到达的
// Close 看不到 cancel，将永久等待一个无法取消的压缩（审查 D2）；Wait 同理会漏掉本批压缩。
func (w *Window) startLocked(ctx context.Context) {
	w.compressing = true
	cctx, cancel := context.WithCancel(ctxx.Detached(ctx))
	w.cancel = cancel
	w.compDone = make(chan struct{})
	go w.compress(cctx)
}

// finishLocked 结束压缩循环：清除在途标记、待压缩批次的 ctx 与取消函数，并唤醒 Wait（调用方持有 w.mu）。
func (w *Window) finishLocked() {
	w.pendingCtx = nil
	w.compressing = false
	w.cancel = nil
	if w.compDone != nil {
		close(w.compDone)
		w.compDone = nil
	}
}

func (w *Window) compress(ctx context.Context) {
	for {
		w.mu.Lock()
		// 取快照前先接住待压缩批次的身份：压缩在途时有新 Settle 进来，本批
		// 内容含新说话人的轮次，身份（Scope/凭据/参数）换成触发它的 ctx——
		// 不能沿用上一轮的（2026-09-28 审查：B 的内容曾以 A 的 UserID 压缩）。
		// 放在快照同一临界区：新 Settle 与快照的先后在此定序，无论谁先，
		// 压缩用的都是「本批最后到达内容」的 ctx。换身份即重挂取消——Close
		// 必须能取消「正在跑」的这一轮；被合并轮的取消已无人使用，顺手释放。
		if w.pendingCtx != nil {
			ctx = w.pendingCtx
			w.pendingCtx = nil
			if prev := w.cancel; prev != nil {
				prev()
			}
			cctx, cancel := context.WithCancel(ctxx.Detached(ctx))
			ctx, w.cancel = cctx, cancel
		}
		// 取走的轮数见 cutTurnsLocked；为 0 表示两个触发条件均已不成立
		// （例如压缩进行中 SetBudget 更换了预算），结束循环。
		cutTurns := w.cutTurnsLocked()
		if cutTurns == 0 {
			w.finishLocked()
			w.mu.Unlock()
			return
		}
		w.flushTurns = max(w.flushTurns-cutTurns, 0)
		cut := 0
		for _, n := range w.recentTurns[:cutTurns] {
			cut += n
		}
		mem, turn := cloneAll(w.memory), cloneAll(w.recent[:cut])
		w.recent = slices.Clone(w.recent[cut:]) // recent 只由窗口持有：浅拷贝即可释放旧底层数组
		w.recentTurns = slices.Clone(w.recentTurns[cutTurns:])
		w.inflight = turn
		comp := w.compressor // 持锁取出：SetCompressor 可能并发替换字段
		onCompress := w.onCompress
		w.mu.Unlock()

		out, err := comp.Compress(ctx, mem, turn)
		if err == nil && len(out) == 0 {
			err = errors.New("window: compressor returned empty memory")
		}
		report := loop.WindowCompressedData{
			InMessages:  len(mem) + len(turn),
			InChars:     charsOf(mem) + charsOf(turn),
			OutMessages: len(out),
			OutChars:    charsOf(out),
		}

		w.mu.Lock()
		if err != nil {
			// 压缩失败不丢内容：退回未压缩形态继续累积，下一轮照常组装。
			// 原始消息超过 fallbackCap 才裁剪（防止压缩持续失败导致无界增长）。
			w.log.Warn("window: compress failed, keeping raw messages", "err", err)
			mem = append(mem, turn...)
			if n := w.fallbackCap; n > 0 && len(mem) > n {
				trimmed := trimKeepLast(mem, n)
				w.log.Warn("window: fallback trimmed", "dropped", len(mem)-len(trimmed), "cap", n)
				report.FallbackDropped = len(mem) - len(trimmed)
				mem = trimmed
			}
			w.memory = mem
			report.Err = err.Error()
			report.OutMessages, report.OutChars = len(mem), charsOf(mem)
		} else {
			w.memory = cloneAll(out) // 不持有 Compressor 的切片
		}
		w.inflight = nil
		// 成功：按压缩后的内容重新估算用量，仍满足触发条件（近轮仍过长、压缩期间又有新结算，
		// 或 Flush 要求的轮数未取完）时继续压缩。失败：结束，由下一次 Settle 重新判定，
		// 不在此处连续重试。结束时待压缩批次的 ctx 一并清除，下一次由触发压缩的 Settle 或 Flush 提供 ctx。
		if err == nil && w.budget.ContextWindow > 0 {
			w.used = w.estimateLocked()
		}
		done := w.closed || err != nil || w.cutTurnsLocked() == 0
		if done {
			w.finishLocked()
		}
		w.mu.Unlock()
		if onCompress != nil {
			onCompress(ctx, report) // 锁外回调：见 SetOnCompress 的纪律
		}
		if done {
			return
		}
	}
}

// charsOf 统计消息中文本与思考块的总字符数（rune）——报告里的信息量粗估。
func charsOf(ms []message.Message) int {
	n := 0
	for _, m := range ms {
		for _, b := range m.Blocks {
			switch v := b.(type) {
			case message.TextBlock:
				n += len([]rune(v.Text))
			case message.ThoughtBlock:
				n += len([]rune(v.Text))
			}
		}
	}
	return n
}

// Snapshot 返回记忆（含在途）与近轮的副本（调试与测试用）。
func (w *Window) Snapshot() (memory, recent []message.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	mem := cloneAll(w.memory)
	mem = append(mem, cloneAll(w.inflight)...)
	return mem, cloneAll(w.recent)
}

// Wait 等待在途压缩结束，可并发调用、也可与 Settle 并发（宿主监控/关停观察）。
//
// 不用 sync.WaitGroup：Settle 里的 Add 与这里的 Wait 交错会触发
// 「WaitGroup is reused before previous Wait has returned」panic（进程级崩溃）。
// compDone 每次压缩新建、结束即关闭，等待者拿到 nil 就说明没有在途压缩。
// 注意：压缩实现若无视 ctx，Wait/Close 会被拖住。
func (w *Window) Wait() {
	for {
		w.mu.Lock()
		done := w.compDone
		w.mu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

// Close 取消并等待在途压缩，之后不再接受 Settle。
func (w *Window) Close() {
	w.mu.Lock()
	w.closed = true
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.Wait()
}

// ---------- 兜底压缩器 ----------

// KeepLast 是不调 LLM 的压缩：只保留最近 n 条消息（工具调用组不拆散时
// 可能略多），更早的丢弃（静默）。它保证窗口有界，是测试与无 Provider
// 场景的确定性默认；需要「记住更早内容」的宿主应显式接 ProviderCompressor
// 之类的摘要实现。
type KeepLast int

func (k KeepLast) Compress(_ context.Context, memory, turn []message.Message) ([]message.Message, error) {
	all := append(cloneAll(memory), cloneAll(turn)...)
	return trimKeepLast(all, int(k)), nil
}

// trimKeepLast 取 all 的末尾约 n 条，且不拆散工具调用组：切点若落在
// tool 结果上，向后扩到配对的 assistant（整组保留）。n 是预算不是硬上限，
// 超出一组无伤有界性。序列自身的合法性不在此处修补——坏数据原样通过，
// 由发送边界的 message.ValidateToolPairing 响亮报错（带位置与 ID）。
func trimKeepLast(all []message.Message, n int) []message.Message {
	if n <= 0 || len(all) <= n {
		return all
	}
	start := len(all) - n
	for start > 0 && all[start].Role == message.RoleTool {
		start--
	}
	return all[start:]
}

func cloneAll(in []message.Message) []message.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]message.Message, len(in))
	for i, m := range in {
		out[i] = m.Clone()
	}
	return out
}
