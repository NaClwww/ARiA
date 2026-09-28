// Package loop 实现单线程飞轮（docs/02）：一次 Run 的全部可变状态归一个
// goroutine，飞轮状态零锁（入口与总线的同步原语是边界设施，不算状态锁）。
// 零磁盘 I/O：持久化 = runtime 订阅 durable 事件做事件溯源。
package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

// ---------- 公共词汇 ----------

type EndReason string

const (
	EndDone            EndReason = "done"
	EndInterrupted     EndReason = "interrupted"
	EndCancelled       EndReason = "cancelled" // parent 取消（关机），与 Interrupt（转向）相对
	EndBudgetExhausted EndReason = "budget_exhausted"
	EndMaxTurns        EndReason = "max_turns"
	EndError           EndReason = "error"
)

// DefaultMaxTurns 是 MaxTurns==0 时的默认闸（02 §2「默认宽松」）。
const DefaultMaxTurns = 100

var (
	ErrNoScope        = errors.New("loop: scope missing in ctx (fail-closed, 01 R4)")
	ErrAlreadyRunning = errors.New("loop: concurrent Run on same instance")
	// ErrNoActiveRun：Queue 只做轮间注入，运行未开始 / 已收敛 / 已返回时拒绝。
	ErrNoActiveRun = errors.New("loop: no active run (Queue only injects into a running turn)")
)

// State 是 Assembler 每轮收到的只读快照（core↔runtime 的分界类型，B1）。
// Assembler 不得修改其中的 Message；返回新切片以表达组装结果。
type State struct {
	Messages []message.Message // 当前 transcript（只读约定）
	Turn     int
	QueueLen int
}

// Assembler 是槽 1：窗口组装/记忆注入唯一入口。nil = 恒等透传。
type Assembler interface {
	Assemble(ctx context.Context, s State) []message.Message
}

type GuardAction uint8

const (
	GuardAllow GuardAction = iota
	GuardDeny
	GuardRewrite
)

// Decision 是 Guard 的决议。Rewrite 时替换调用执行（不再复检——改写者本身
// 就是受信的 Guard 链；需要复检由 ChainGuard 自行折叠，见 03 §3）。
type Decision struct {
	Action    GuardAction
	Reason    string    // Deny 时成为喂回模型的工具错误内容
	Rewritten tool.Call // Rewrite 时的替换调用（ID 缺省沿用原 ID）
}

// ToolGuard 是槽 2：审批/审计/改写。nil = 全放行。
type ToolGuard interface {
	Check(ctx context.Context, tc tool.Call) Decision
}

type Config struct {
	Provider provider.Provider
	Tools    []tool.Tool

	Assembler Assembler // nil = 恒等
	Guard     ToolGuard // nil = 全放行

	// MaxTurns：0 → DefaultMaxTurns；负数 = 无限制（loop until done）。
	// 注意：与 02 §2 草案「0 = 无限制」不同——无限制用负数表达，
	// 0 留给「未设置」走默认，避免埋下无闸飞轮。
	MaxTurns int

	// ToolTimeout 单工具执行上限；0 = 不限时。
	ToolTimeout time.Duration

	// MaxStreamRetries 是 ProviderError{Retryable} 且未产出任何内容前的
	// 退避重试次数（02 §7：流已半截不重试）；0 → 默认 3。
	MaxStreamRetries int
}

type RunResult struct {
	RunID     string
	EndReason EndReason
	Turns     int
	Usage     message.Usage // 累计
}

// ---------- Loop ----------

type Loop struct {
	cfg      Config
	tools    map[string]tool.Tool
	toolDefs []tool.Def
	bus      *bus

	mu      sync.Mutex // 只守入口边界（队列、运行标志），不守飞轮状态
	queue   []message.Message
	running bool
	// sealed：本轮已在收敛判定时封口，Queue 拒收。与 running 分开：running
	// 兼任「禁止并发 Run」的入口闸，要等 Run 返回才在 defer 清；而 Queue 的
	// 收口必须发生在收敛判定的同一临界区，否则消息会挤进「已判空、未返回」
	// 的缝隙（2026-09-28 审查修复）。
	sealed bool

	opCancel  atomic.Pointer[cancelBox] // 当前阻塞操作的 cancel（Interrupt 的靶点）
	interrupt atomic.Bool

	// 以下为飞轮状态：仅 Run goroutine 触碰。
	messages []message.Message
	turn     int
	runID    string
}

type cancelBox struct{ fn context.CancelFunc }

func New(cfg Config) (*Loop, error) {
	if cfg.Provider == nil {
		return nil, errors.New("loop: Provider is required")
	}
	l := &Loop{cfg: cfg, bus: newBus(), tools: make(map[string]tool.Tool, len(cfg.Tools))}
	for _, t := range cfg.Tools {
		d := t.Def()
		if !validToolName(d.Name) {
			return nil, fmt.Errorf("loop: invalid tool name %q（OpenAI 工具名规范：1-64 个 ASCII 字母/数字/_/-，部分网关严格校验）", d.Name)
		}
		if _, dup := l.tools[d.Name]; dup {
			return nil, fmt.Errorf("loop: duplicate tool name %q", d.Name)
		}
		l.tools[d.Name] = t
		l.toolDefs = append(l.toolDefs, cloneToolDef(d))
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.MaxStreamRetries == 0 {
		cfg.MaxStreamRetries = 3
	}
	l.cfg = cfg
	return l, nil
}

// ---------- 四个并发入口 ----------

// Run 阻塞执行一次完整运行；同实例禁止并发 Run。
func (l *Loop) Run(parent context.Context, input []message.Message) (RunResult, error) {
	if _, ok := ctxx.ScopeFrom(parent); !ok {
		return RunResult{}, ErrNoScope
	}
	if len(input) == 0 {
		return RunResult{}, errors.New("loop: empty input (a run starts from at least one message)")
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return RunResult{}, ErrAlreadyRunning
	}
	l.running = true
	l.sealed = false // 上一轮收敛时封的口，本轮入口重开
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
	}()

	runCtx, cancel := context.WithCancel(parent)
	defer cancel()
	l.interrupt.Store(false)
	l.runID = newID()
	// AgentStart 属于本轮第 0 轮。不复位会把上一轮结束时的计数值写进 durable
	// 记录（Loop 是复用的：一个 Session 一个 Loop），同一 run 内 turn 自相矛盾。
	l.turn = 0
	l.messages = cloneMessages(input)

	scope, _ := ctxx.ScopeFrom(parent)
	l.emit(KindAgentStart, AgentStartData{Scope: scope, InitialInput: cloneMessages(input)})

	var total message.Usage
	turns := 0
	for l.turn = 0; ; l.turn++ {
		if end, err := l.preFlight(parent); end != "" {
			return l.finish(end, turns, total, err)
		}
		l.drainQueue() // 错误路径滞留的入队消息在此排空（Queue 已拒空闲入队）
		// 轮间 Interrupt 且无新输入：直接收敛为 EndInterrupted。
		// sealQuiet 与 Queue 的入队同锁互斥：收敛判定即封口，不存在
		// 「已判空、未返回」间被入队挤入的缝隙。
		if l.interrupt.Load() && l.sealQuiet() {
			return l.finish(EndInterrupted, turns, total, nil)
		}
		turns++
		l.emit(KindTurnStart, TurnStartData{Turn: l.turn})

		msgs := l.assemble(runCtx)
		msg, usage, err := l.streamLLM(runCtx, msgs)
		total = total.Add(usage)
		if err != nil {
			if parent.Err() != nil {
				return l.finish(EndCancelled, turns, total, parent.Err())
			}
			return l.finish(EndError, turns, total, err)
		}
		if b := ctxx.BudgetFrom(parent); b != nil {
			b.Consume(usage)
		}
		l.messages = append(l.messages, msg)
		l.emit(KindMessageEnd, MessageEndData{Message: msg, Usage: usage})

		if len(msg.ToolCalls) > 0 {
			l.execCalls(runCtx, msg.ToolCalls)
		}
		injected := l.drainQueue()
		l.emit(KindTurnEnd, TurnEndData{Turn: l.turn, Usage: usage})

		// 恢复点先查 parent（02 §6）：Interrupt（转向）≠ parent 取消（关机）
		if perr := parent.Err(); perr != nil {
			return l.finish(EndCancelled, turns, total, perr)
		}
		if l.interrupt.Load() && l.sealQuiet() {
			return l.finish(EndInterrupted, turns, total, nil)
		}
		// 本轮无工具调用、也没有新注入的输入 → 自然收敛（sealQuiet 同上）
		if len(msg.ToolCalls) == 0 && injected == 0 && l.sealQuiet() {
			return l.finish(EndDone, turns, total, nil)
		}
		// 其余情况续轮：有工具结果待续 / 有新注入的输入待回答
	}
}

// Queue 往**正在进行的** Run 轮间注入 user 消息（steering）。运行未开始、
// 已收敛或已返回时拒绝 ErrNoActiveRun：空闲入队会被下一次 Run 在其新输入
// 之后排空，历史「后说的在前」且随记忆长留（06 §2）。「接受入队」与
// 「结束运行」在同一临界区判定，先到先得。
func (l *Loop) Queue(msg message.Message) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running || l.sealed {
		return ErrNoActiveRun
	}
	l.queue = append(l.queue, msg.Clone())
	return nil
}

// Interrupt 取消当前 LLM 调用/工具执行，保留部分输出；队列有输入则续轮。
// 这是转向（steering），不是 parent 取消（关机）。
// A3 默认：中断发生在工具段时，未执行的调用补 [interrupted] 错误结果，
// 保证每个 tool_call 都有应答（否则重放历史会被 provider 拒收）。
func (l *Loop) Interrupt() {
	l.interrupt.Store(true)
	if box := l.opCancel.Load(); box != nil && box.fn != nil {
		box.fn()
	}
}

// Subscribe 返回事件通道与退订函数（总线纪律见 bus.go）。
func (l *Loop) Subscribe(buf int) (<-chan Event, func()) {
	return l.bus.add(buf)
}

// ---------- 飞轮内部（仅 Run goroutine 触碰） ----------

func (l *Loop) preFlight(parent context.Context) (EndReason, error) {
	if err := parent.Err(); err != nil {
		return EndCancelled, err
	}
	if l.cfg.MaxTurns >= 0 && l.turn >= l.cfg.MaxTurns {
		return EndMaxTurns, nil
	}
	if b := ctxx.BudgetFrom(parent); b != nil && b.Exceeded() {
		return EndBudgetExhausted, nil
	}
	return "", nil
}

// assemble 调槽 1；默认恒等透传 transcript。
func (l *Loop) assemble(ctx context.Context) []message.Message {
	if l.cfg.Assembler == nil {
		return cloneMessages(l.messages)
	}
	st := State{Messages: cloneMessages(l.messages), Turn: l.turn, QueueLen: l.queueLen()}

	out := l.cfg.Assembler.Assemble(ctx, st)
	if out == nil {
		return cloneMessages(l.messages)
	}
	return cloneMessages(out)
}

// streamLLM 消费 provider 流：翻译 Start/Update 事件；未产出任何内容前的
// Retryable 错误退避重试（02 §7）；取消时合成部分输出（provider 违约兜底）。
// 发送前经 message.ValidateToolPairing 校验 transcript——配对完整性是协议
// 硬要求，坏数据在这里响亮失败（EndError），不留给服务端报模糊的 400。
func (l *Loop) streamLLM(ctx context.Context, msgs []message.Message) (message.Message, message.Usage, error) {
	if err := message.ValidateToolPairing(msgs); err != nil {
		return message.Message{}, message.Usage{}, fmt.Errorf("invalid transcript: %w", err)
	}
	opts, _ := ctxx.OptionsFrom(ctx)
	req := provider.Request{Messages: cloneMessages(msgs), Tools: cloneToolDefs(l.toolDefs), Options: opts}

	msgID := newID()
	l.emit(KindMessageStart, MessageStartData{Role: message.RoleAssistant, MessageID: msgID})

	var text, thought strings.Builder
	received := false // 已产出任何内容——有内容则不重试（避免重复计费/内容）

attemptLoop:
	for attempt := 0; ; attempt++ {
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		l.opCancel.Store(&cancelBox{fn: attemptCancel})
		// 注册后再复查：Interrupt 落在「检查通过→注册 cancel」之间的空档时，
		// 标志已置位而 cancel 未注册；先注册再检查可保证该窗口内的 Interrupt
		// 必被此处捕获（此后到达的 Interrupt 则命中已注册的 cancel）。
		if l.interrupt.Load() {
			attemptCancel()
			l.opCancel.Store(&cancelBox{})
			return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
		}
		evCh, err := l.cfg.Provider.Stream(attemptCtx, cloneRequest(req))
		if err != nil {
			cancelled := attemptCtx.Err() != nil
			attemptCancel()
			l.opCancel.Store(&cancelBox{})
			if cancelled {
				return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
			}
			if provider.IsRetryable(err) && attempt < l.cfg.MaxStreamRetries && l.waitRetry(ctx, attempt) {
				continue
			}
			if l.interrupt.Load() {
				return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
			}
			return message.Message{}, message.Usage{}, fmt.Errorf("provider stream: %w", err)
		}
		for ev := range evCh {
			if l.interrupt.Load() && attemptCtx.Err() == nil {
				// 流中到达的 Interrupt：立即取消 attempt，provider 按义务回
				// Interrupted complete；若违约，随后的 channel 关闭路径兜底
				attemptCancel()
			}
			switch e := ev.(type) {
			case provider.PartDelta:
				received = true
				text.WriteString(e.Text)
				l.emit(KindMessageUpdate, MessageUpdateData{MessageID: msgID, TextDelta: e.Text})
			case provider.ThoughtDelta:
				received = true
				thought.WriteString(e.Text)
				l.emit(KindMessageUpdate, MessageUpdateData{MessageID: msgID, ThoughtDelta: e.Text})
			case provider.MessageComplete:
				attemptCancel()
				l.opCancel.Store(&cancelBox{})
				m := e.Message.Clone()
				m.ID = msgID // loop 持有 canonical ID，事件与历史一致
				m.Interrupted = e.Interrupted
				return m, e.Usage, nil
			case provider.ErrorEvent:
				cancelled := attemptCtx.Err() != nil
				attemptCancel()
				l.opCancel.Store(&cancelBox{})
				if cancelled {
					return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
				}
				if e.Retryable && !received && attempt < l.cfg.MaxStreamRetries && l.waitRetry(ctx, attempt) {
					continue attemptLoop
				}
				if l.interrupt.Load() {
					return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
				}
				return message.Message{}, message.Usage{}, fmt.Errorf("provider: %w", e.Err)
			}
		}
		cancelled := attemptCtx.Err() != nil
		attemptCancel()
		l.opCancel.Store(&cancelBox{})
		if cancelled {
			return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
		}
		return message.Message{}, message.Usage{},
			errors.New("provider violated contract: stream ended without MessageComplete")
	}
}

// execCalls 顺序执行工具调用（02 §2）。每个调用的操作 cancel 在入口
// 注册、随后复查 interrupt——与 streamLLM 同一纪律，保证「检查通过→
// 注册 cancel」空档内到达的 Interrupt 必被捕获。
func (l *Loop) execCalls(ctx context.Context, calls []message.ToolCall) {
	for _, tc := range calls {
		callCtx, callCancel := context.WithCancel(ctx)
		l.opCancel.Store(&cancelBox{fn: callCancel})
		if l.interrupt.Load() {
			callCancel()
			l.opCancel.Store(&cancelBox{})
			res := aborted(tc.ID)
			l.messages = append(l.messages, res.ToMessage())
			l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res})
			continue
		}

		d := Decision{Action: GuardAllow}
		if l.cfg.Guard != nil {
			d = l.cfg.Guard.Check(callCtx, tc.Clone())
			if callCtx.Err() != nil {
				callCancel()
				l.opCancel.Store(&cancelBox{})
				res := aborted(tc.ID)
				l.messages = append(l.messages, res.ToMessage())
				l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res})
				continue
			}
		}

		effective := tc.Clone()
		if d.Action == GuardRewrite {
			effective = d.Rewritten.Clone()
			effective.ID = tc.ID
			d.Rewritten = effective
		}
		if l.cfg.Guard != nil {
			var rewritten *tool.Call
			if d.Action == GuardRewrite {
				r := effective.Clone()
				rewritten = &r
			}
			l.emit(KindToolGuardDecision, ToolGuardDecisionData{Call: tc, Action: d.Action, Reason: d.Reason, Rewritten: rewritten})
		}

		switch d.Action {
		case GuardDeny:
			callCancel()
			l.opCancel.Store(&cancelBox{})
			res := message.ToolResult{CallID: tc.ID, IsError: true,
				Blocks: []message.Block{message.TextBlock{Text: "denied: " + d.Reason}}}
			l.messages = append(l.messages, res.ToMessage())
			l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res, Denied: true})
			continue
		case GuardRewrite:
			tc = effective
		}

		l.emit(KindToolExecStart, ToolExecStartData{Call: tc})
		res := l.execTool(callCtx, tc)
		callCancel()
		l.opCancel.Store(&cancelBox{})
		l.messages = append(l.messages, res.ToMessage())
		l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res})
	}
}

// execTool 执行单个工具。cancel 已由 execCalls 入口注册（覆盖 Guard+Exec
// 全程），这里只叠加可选的单工具超时，不再自装 opCancel。
func (l *Loop) execTool(ctx context.Context, tc message.ToolCall) message.ToolResult {
	t, ok := l.tools[tc.Name]
	if !ok {
		return message.ToolResult{CallID: tc.ID, IsError: true,
			Blocks: []message.Block{message.TextBlock{Text: "unknown tool: " + tc.Name}}}
	}
	tctx := ctx
	if l.cfg.ToolTimeout > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeout(ctx, l.cfg.ToolTimeout)
		defer cancel()
	}

	res := t.Exec(tctx, tc.Clone()).Clone()
	if res.CallID == "" {
		res.CallID = tc.ID
	}
	if tctx.Err() != nil && !res.IsError {
		res.IsError = true
		res.Blocks = append(res.Blocks, message.TextBlock{Text: "[tool interrupted]"})
	}
	return res
}

// drainQueue 把队列输入注入为 user 消息，返回注入条数。
// 注入即「转向已交付」：interrupt 标记随之清除，下一轮正常回答新输入。
func (l *Loop) drainQueue() int {
	n := 0
	for {
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.mu.Unlock()
			if n > 0 {
				l.interrupt.Store(false)
			}
			return n
		}
		m := l.queue[0]
		l.queue = l.queue[1:]
		l.mu.Unlock()

		if m.ID == "" {
			m.ID = newID()
		}
		if m.Role == "" {
			m.Role = message.RoleUser
		}
		l.messages = append(l.messages, m)
		l.emit(KindUserMessageInjected, UserMessageInjectedData{Message: m})
		n++
	}
}

func (l *Loop) queueLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue)
}

// validToolName 校验工具名符合 OpenAI 工具名规范（1-64 个 ASCII 字母/
// 数字/下划线/连字符）。规范是 wire 协议的一部分，部分网关严格校验、
// 违规直接 400——在装配期响亮失败，好过发出去吃一条含糊的服务端报错。
func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// sealQuiet 在队列无积压时原子地封箱本轮入队口，返回 false 表示有积压。
// 收敛路径（自然收敛 / Interrupt 收敛）必须经它同时完成「看空」与「封口」：
// 若分两步，Queue 会挤进「已看空、未返回」的窗口，消息滞留到下一次 Run
// 并排在其新输入之后（时序颠倒，2026-09-28 审查修复）。
func (l *Loop) sealQuiet() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) > 0 {
		return false
	}
	l.sealed = true
	return true
}

// partialAssistant 在 provider 违约（取消却无 MessageComplete）时兜底合成部分输出。
func (l *Loop) partialAssistant(msgID string, text, thought *strings.Builder) message.Message {
	m := message.Message{ID: msgID, Role: message.RoleAssistant, Interrupted: true}
	if thought.Len() > 0 {
		m.Blocks = append(m.Blocks, message.ThoughtBlock{Text: thought.String()})
	}
	if text.Len() > 0 {
		m.Blocks = append(m.Blocks, message.TextBlock{Text: text.String()})
	}
	return m
}

func (l *Loop) finish(end EndReason, turns int, total message.Usage, err error) (RunResult, error) {
	res := RunResult{RunID: l.runID, EndReason: end, Turns: turns, Usage: total}
	l.emit(KindAgentEnd, AgentEndData{Result: res, Err: err})
	if err != nil {
		return res, err
	}
	return res, nil
}

func (l *Loop) emit(kind Kind, data any) {
	l.bus.emit(Event{Kind: kind, RunID: l.runID, Turn: l.turn, At: time.Now(), Data: data})
}

// ---------- 杂项 ----------

// waitRetry 等待重试退避；期间 parent 取消或 Interrupt 到达则提前放弃
// （false）。轮询 25ms：Interrupt 可能在两个 attempt 之间的空槽到达，
// 此时不持有 opCancel，只能靠标志位唤醒。
func (l *Loop) waitRetry(ctx context.Context, attempt int) bool {
	d := time.Duration(50<<min(attempt, 4)) * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return !l.interrupt.Load()
		case <-tick.C:
			if l.interrupt.Load() {
				return false
			}
		}
	}
}

func aborted(callID string) message.ToolResult {
	return message.ToolResult{CallID: callID, IsError: true,
		Blocks: []message.Block{message.TextBlock{Text: "[interrupted]"}}}
}

func cloneMessages(in []message.Message) []message.Message {
	if in == nil {
		return nil
	}
	out := make([]message.Message, len(in))
	for i := range in {
		out[i] = in[i].Clone()
	}
	return out
}

func cloneToolDef(d tool.Def) tool.Def {
	d.Parameters = append([]byte(nil), d.Parameters...)
	return d
}

func cloneToolDefs(in []tool.Def) []tool.Def {
	out := make([]tool.Def, len(in))
	for i := range in {
		out[i] = cloneToolDef(in[i])
	}
	return out
}

func cloneRequest(req provider.Request) provider.Request {
	req.Messages = cloneMessages(req.Messages)
	req.Tools = cloneToolDefs(req.Tools)
	req.Options.Stop = slices.Clone(req.Options.Stop)
	if req.Options.Temperature != nil {
		v := *req.Options.Temperature
		req.Options.Temperature = &v
	}
	return req
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("loop: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
