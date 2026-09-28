// Package window 负责每轮上下文组装（docs/03 §5）：
//
//	每轮进 core 的历史 = 压缩记忆 + 未压缩近轮 + 新输入
//
// 压缩（一次 LLM 调用）不在组装快路径上：轮次结束后由 Settle 在间隙里异步做，
// 未就绪时下一轮继续用旧记忆——慢一点，但组装路径永远没有 LLM 调用。
package window

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"aria/pkg/ctxx"
	"aria/pkg/message"
)

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
	compressing bool
	pending     bool
	pendingCtx  context.Context // 触发待压缩批次的 Settle 的 ctx：下一轮压缩换用它的身份
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
func (w *Window) Assemble(input []message.Message) []message.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(w.memory) + len(w.inflight) + len(w.recent) + len(input)
	if w.system != "" {
		n++
	}
	out := make([]message.Message, 0, n)
	if w.system != "" {
		out = append(out, message.NewSystem(w.system))
	}
	out = append(out, cloneAll(w.memory)...)
	out = append(out, cloneAll(w.inflight)...)
	out = append(out, cloneAll(w.recent)...)
	out = append(out, cloneAll(input)...)
	return out
}

// Settle 在一轮结束后把本轮消息并入窗口并触发间隙压缩。
// 非阻塞：压缩在后台进行，下一轮组装不等待它（03 §5）。
func (w *Window) Settle(ctx context.Context, turn []message.Message) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.recent = append(w.recent, cloneAll(turn)...)
	if w.compressing {
		// 压缩在途：只标脏。当前这轮压完后会用最新内容再压一次，
		// 期间多次 Settle 被合并（避免并发压缩互相覆盖）。记下触发本批的
		// ctx——下一轮压缩的身份（Scope/凭据/参数）属于这批内容的主人，
		// 不能沿用上一轮的（2026-09-28 审查：B 的内容曾以 A 的 UserID 压缩）。
		w.pending = true
		if w.pendingCtx == nil && ctx != nil {
			w.pendingCtx = ctx
		}
		w.mu.Unlock()
		return
	}
	w.compressing = true
	// cancel 必须在持锁时注册：否则 Settle 返回后、压缩 goroutine 注册前
	// 到达的 Close 会看不到它，Close 将永久等待一个无法取消的压缩（审查 D2）。
	// compDone 同理：等它的是 Wait，注册晚了会漏掉这次压缩。
	cctx, cancel := context.WithCancel(ctxx.Detached(ctx))
	w.cancel = cancel
	w.compDone = make(chan struct{})
	w.mu.Unlock()
	go w.compress(cctx)
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
		mem, turn := cloneAll(w.memory), cloneAll(w.recent)
		w.recent, w.pending = nil, false
		w.inflight = turn
		comp := w.compressor // 持锁取出：SetCompressor 可能并发替换字段
		w.mu.Unlock()

		out, err := comp.Compress(ctx, mem, turn)
		if err == nil && len(out) == 0 {
			err = errors.New("window: compressor returned empty memory")
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
				mem = trimmed
			}
			w.memory = mem
		} else {
			w.memory = cloneAll(out) // 不持有 Compressor 的切片
		}
		w.inflight = nil
		if !w.pending || w.closed {
			w.compressing = false
			w.cancel = nil
			if w.compDone != nil {
				close(w.compDone)
				w.compDone = nil
			}
			w.mu.Unlock()
			return
		}
		w.mu.Unlock() // 待压缩批次在下一轮快照处换身份（见循环头）
	}
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
