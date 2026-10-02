// Package window 负责每轮上下文组装（docs/03 §5）：
//
//	每轮进 core 的历史 = 会话开始时的召回 + 压缩记忆 + 未压缩近轮 + 新输入
//
// 压缩（一次 LLM 调用）不在组装快路径上：轮次结束后由 Settle 在间隙里异步做，
// 未就绪时下一轮继续用旧记忆——慢一点，但组装路径永远没有 LLM 调用。
//
// 压缩按上下文用量触发（见 Budget）：上下文剩余量低于预留量时，压缩触发时刻
// 近轮中除最近 K 轮以外的轮次；压缩进行中结算的轮次不计入 K，保留原文。
// 会话切换时由 Reset 清空记忆与近轮：新会话不带入上一会话的摘要与原文。
//
// 压缩实现同时实现 PointExtractor、且已由 SetOnPoints 注册回调时，压缩调用一并输出待写入长期记忆的
// 要点并交给该回调（docs/memory/options.md「写入与召回流程」）。
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
	"aria/runtime/memory"
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

// PointExtractor 是 Compressor 的可选扩展（docs/memory/options.md「要点的格式」）：
//   - CompressWithPoints：会话内压缩，一次调用同时输出新记忆与待写入长期记忆的要点；本批不需要写入时
//     points 为空；错误与空输出的处理与 Compress 相同；
//   - ExtractPoints：会话切换时对清除的原文只提取要点，不生成摘要；mem 为会话内的压缩摘要，只用于理解上下文。
//
// 两个方法的要点都只从 turn 提取，mem 只用于理解上下文；实现不得修改传入的消息切片。
type PointExtractor interface {
	CompressWithPoints(ctx context.Context, mem, turn []message.Message) (out []message.Message, points []memory.Point, err error)
	ExtractPoints(ctx context.Context, mem, turn []message.Message) ([]memory.Point, error)
}

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
	recalled    []message.Message // 会话开始时召回的长期记忆：整个会话内不变，不参与压缩，见 SetRecalled
	memory      []message.Message // 已压缩的记忆
	inflight    []message.Message // 正被压缩的消息：仍参与组装，压完才被替换
	recent      []message.Message // 尚未压缩的近轮（此前输入与产出）
	recentTurns []int             // recent 中各轮的消息条数，按结算顺序；按轮切分压缩范围用
	keepTurns   int               // 压缩时保留原文的近轮数 K，见 SetKeepRecentTurns
	budget      Budget            // 按用量触发压缩的预算，见 SetBudget
	used        int               // 最近一次判定得到的上下文用量（token）
	epoch       uint64            // Reset 次数：在途压缩据此判定结果是否仍属于当前窗口
	pointSeq    int               // 本次 Reset 以来已上报要点的批次数（SetOnPoints 回调的 seq）
	unextracted int               // memory 末尾未提取要点的原文条数：压缩失败回退时并入，下一批压缩时移回本批对话
	compressing bool
	pendingCtx  context.Context // 触发待压缩批次的 Settle 的 ctx：下一轮压缩换用它的身份
	onCompress  func(context.Context, loop.WindowCompressedData)
	onPoints    func(ctx context.Context, seq int, points []memory.Point)
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
// （值取自触发该批的 Settle，其 Scope 即该批内容所属的会话）。回调必须快、
// 不得再调窗口方法（会死锁）；nil 关闭。window_compressed 审计事件经此回调由 Session
// 写入 Store——窗口本身不认识事件总线。
func (w *Window) SetOnCompress(fn func(ctx context.Context, rep loop.WindowCompressedData)) {
	w.mu.Lock()
	w.onCompress = fn
	w.mu.Unlock()
}

// SetOnPoints 注册要点上报（装配期调用）：fn 非 nil 且压缩实现为 PointExtractor 时，压缩改用
// CompressWithPoints，某批压缩成功且输出的要点非空时调用 fn；seq 为本次 Reset 以来上报的批次序号（从 1 起），
// ctx 为该批压缩使用的 ctx。压缩期间窗口被 Reset 的批次不上报。fn 在持有窗口锁时同步执行，以保证
// 同一窗口的上报先于其后 Reset 返回：fn 只能做非阻塞操作（例如投递到队列），不得调用窗口方法或可能
// 等待窗口锁的方法。nil 关闭要点上报，压缩改回 Compress。
func (w *Window) SetOnPoints(fn func(ctx context.Context, seq int, points []memory.Point)) {
	w.mu.Lock()
	w.onPoints = fn
	w.mu.Unlock()
}

// SetRecalled 设置会话开始时召回的长期记忆（RecallMessage 等），组装时位于 system 之后、
// 压缩记忆之前；整个会话内不变，不参与压缩，Reset 时清除。
func (w *Window) SetRecalled(msgs []message.Message) {
	w.mu.Lock()
	w.recalled = cloneAll(msgs)
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

// cutTurnsLocked 返回下一批压缩从 recent 开头取走的轮数（调用方持有 w.mu）：满足触发条件时
// 取除最近 K 轮以外的轮次，近轮不超过 K 轮时全部取走；不满足时为 0。
func (w *Window) cutTurnsLocked() int {
	if !w.dueLocked() {
		return 0
	}
	n := len(w.recentTurns) - w.keepTurns
	if n <= 0 {
		n = len(w.recentTurns)
	}
	return n
}

// estimateLocked 按 budget.Count 估算当前上下文（system、召回、记忆、在途与近轮）的 token 数（调用方持有 w.mu）。
func (w *Window) estimateLocked() int {
	ms := make([]message.Message, 0, len(w.recalled)+len(w.memory)+len(w.inflight)+len(w.recent)+1)
	if w.system != "" {
		ms = append(ms, message.NewSystem(w.system))
	}
	ms = append(ms, w.recalled...)
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
//	[system] → [<memory> 会话开始时的召回] → [<memory> 压缩记忆] → 近轮 → 新输入
//
// inflight（正被压缩的消息）也参与组装：压缩在途时用户又说了话，
// 那些消息必须仍在上下文里，否则会短暂失忆。
//
// 设置了 Budget 时，组装结果的估算超过 ContextWindow − MaxOutput 则从最早的原文
// （inflight 与近轮）开始略去，直至不超过；只影响本次组装结果，窗口状态不变。
func (w *Window) Assemble(input []message.Message) []message.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := make([]message.Message, 0, len(w.recalled)+len(w.memory)+1)
	if w.system != "" {
		head = append(head, message.NewSystem(w.system))
	}
	head = append(head, cloneAll(w.recalled)...)
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
	if w.cutTurnsLocked() == 0 { // 剩余量不低于预留量：继续以原文参与组装
		w.mu.Unlock()
		return
	}
	w.startLocked(ctx)
	w.mu.Unlock()
}

// Cleared 是 Reset 清除的内容。
type Cleared struct {
	Memory []message.Message // 压缩摘要（不含会话开始时的召回与压缩失败回退时并入的原文）
	Raw    []message.Message // 原文，按时间先后：压缩失败回退时并入记忆的原文、在途、近轮
	Seq    int               // 清除前最后上报的要点批次序号；0 = 未上报过
}

// Reset 清空召回、记忆、在途与近轮（会话切换：新会话不带入上一会话的摘要与原文），返回被清除的内容
// 供调用方另行处理（例如以摘要为上下文对原文提取要点，以 Seq+1 暂存）。在途压缩被取消，其结果与失败
// 回退一律丢弃，待压缩批次的 ctx 一并清除；Reset 之后结算的轮次照常参与组装与压缩。
func (w *Window) Reset() Cleared {
	w.mu.Lock()
	defer w.mu.Unlock()
	summary, unextracted := w.splitMemoryLocked()
	raw := append(cloneAll(unextracted), cloneAll(w.inflight)...)
	c := Cleared{
		Memory: cloneAll(summary),
		Raw:    append(raw, cloneAll(w.recent)...),
		Seq:    w.pointSeq,
	}
	w.recalled, w.memory, w.inflight, w.recent, w.recentTurns = nil, nil, nil, nil, nil
	w.unextracted = 0
	w.used = 0
	w.epoch++
	w.pointSeq = 0
	w.pendingCtx = nil // 上一会话轮次的 ctx：不得用于其后结算的轮次
	if w.cancel != nil {
		w.cancel()
	}
	return c
}

// splitMemoryLocked 把 memory 分为摘要与末尾未提取要点的原文（见 unextracted；调用方持有 w.mu）。
func (w *Window) splitMemoryLocked() (summary, unextracted []message.Message) {
	n := len(w.memory) - w.unextracted
	return w.memory[:n], w.memory[n:]
}

// ExtractsPoints 报告当前压缩实现是否为 PointExtractor。
func (w *Window) ExtractsPoints() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.compressor.(PointExtractor)
	return ok
}

// ExtractPoints 用当前压缩实现对 msgs 只提取要点（PointExtractor.ExtractPoints），mem 只用于理解上下文；
// 不改变窗口状态，mem 与 msgs 不复制、直接交给压缩实现；压缩实现不是 PointExtractor 时返回 nil。
func (w *Window) ExtractPoints(ctx context.Context, mem, msgs []message.Message) ([]memory.Point, error) {
	w.mu.Lock()
	comp := w.compressor
	w.mu.Unlock()
	pe, ok := comp.(PointExtractor)
	if !ok {
		return nil, nil
	}
	return pe.ExtractPoints(ctx, mem, msgs)
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
		// 取走的轮数见 cutTurnsLocked；为 0 表示触发条件已不成立
		// （例如压缩进行中 SetBudget 更换了预算），结束循环。
		cutTurns := w.cutTurnsLocked()
		if cutTurns == 0 {
			w.finishLocked()
			w.mu.Unlock()
			return
		}
		epoch := w.epoch
		cut := 0
		for _, n := range w.recentTurns[:cutTurns] {
			cut += n
		}
		// 压缩失败回退时并入 memory 的原文未曾提取要点，移回本批对话：要点只从本批对话提取。
		summary, unextracted := w.splitMemoryLocked()
		turn := append(cloneAll(unextracted), cloneAll(w.recent[:cut])...)
		w.memory, w.unextracted = summary, 0
		mem := cloneAll(summary)
		w.recent = slices.Clone(w.recent[cut:]) // recent 只由窗口持有：浅拷贝即可释放旧底层数组
		w.recentTurns = slices.Clone(w.recentTurns[cutTurns:])
		w.inflight = turn
		comp := w.compressor // 持锁取出：SetCompressor 可能并发替换字段
		onCompress := w.onCompress
		extract := w.onPoints != nil // 注册了要点上报时才提取要点（SetOnPoints）
		w.mu.Unlock()

		var out []message.Message
		var points []memory.Point
		var err error
		if pe, ok := comp.(PointExtractor); ok && extract {
			out, points, err = pe.CompressWithPoints(ctx, mem, turn)
		} else {
			out, err = comp.Compress(ctx, mem, turn)
		}
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
		switch {
		case w.epoch != epoch:
			// 压缩期间窗口已 Reset：本批属于已结束的会话，结果与失败回退一律丢弃。
			report.Err = "window reset during compression, result discarded"
			report.OutMessages, report.OutChars = 0, 0
		case err != nil:
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
			w.memory, w.unextracted = mem, min(len(turn), len(mem)) // 本批原文未提取要点，见 unextracted
			report.Err = err.Error()
			report.OutMessages, report.OutChars = len(mem), charsOf(mem)
		default:
			w.memory = cloneAll(out) // 不持有 Compressor 的切片
			if len(points) > 0 && w.onPoints != nil {
				w.pointSeq++
				w.onPoints(ctx, w.pointSeq, slices.Clone(points)) // 持锁回调：见 SetOnPoints 的约束
			}
		}
		w.inflight = nil
		// 成功：按压缩后的内容重新估算用量，仍满足触发条件（近轮仍过长，或压缩期间又有新结算）
		// 时继续压缩。失败：结束，由下一次 Settle 重新判定，不在此处连续重试。
		// 结束时待压缩批次的 ctx 一并清除，下一次由触发压缩的 Settle 提供 ctx。
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

// Snapshot 返回记忆（含会话开始时的召回与在途）与近轮的副本（调试与测试用）。
func (w *Window) Snapshot() (memory, recent []message.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	mem := cloneAll(w.recalled)
	mem = append(mem, cloneAll(w.memory)...)
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

func (k KeepLast) Compress(_ context.Context, mem, turn []message.Message) ([]message.Message, error) {
	all := append(cloneAll(mem), cloneAll(turn)...)
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
