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
	cfg   Config
	tools map[string]tool.Tool
	bus   *bus

	mu      sync.Mutex // 只守入口边界（队列、运行标志），不守飞轮状态
	queue   []message.Message
	running bool

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
		if d.Name == "" {
			return nil, errors.New("loop: tool with empty name")
		}
		if _, dup := l.tools[d.Name]; dup {
			return nil, fmt.Errorf("loop: duplicate tool name %q", d.Name)
		}
		l.tools[d.Name] = t
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
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return RunResult{}, ErrAlreadyRunning
	}
	l.running = true
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
	l.messages = slices.Clone(input)

	scope, _ := ctxx.ScopeFrom(parent)
	l.emit(KindAgentStart, AgentStartData{Scope: scope, InitialInput: input})

	var total message.Usage
	for l.turn = 0; ; l.turn++ {
		if end, err := l.preFlight(parent); end != "" {
			return l.finish(end, total, err)
		}
		l.drainQueue() // Run 前积压 / 轮间竞态到达的 steering 输入并入本轮
		// 轮间 Interrupt 且无新输入：直接收敛为 EndInterrupted
		if l.interrupt.Load() && l.queueLen() == 0 {
			return l.finish(EndInterrupted, total, nil)
		}
		l.emit(KindTurnStart, TurnStartData{Turn: l.turn})

		msgs := l.assemble(runCtx)
		msg, usage, err := l.streamLLM(runCtx, msgs)
		total = total.Add(usage)
		if err != nil {
			if parent.Err() != nil {
				return l.finish(EndCancelled, total, parent.Err())
			}
			return l.finish(EndError, total, err)
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
			return l.finish(EndCancelled, total, perr)
		}
		if l.interrupt.Load() && l.queueLen() == 0 {
			return l.finish(EndInterrupted, total, nil)
		}
		// 本轮无工具调用、也没有新注入的输入 → 自然收敛
		if len(msg.ToolCalls) == 0 && injected == 0 && l.queueLen() == 0 {
			return l.finish(EndDone, total, nil)
		}
		// 其余情况续轮：有工具结果待续 / 有新注入的输入待回答
	}
}

// Queue 任意时刻入队，轮间注入为 user 消息。
func (l *Loop) Queue(msg message.Message) {
	l.mu.Lock()
	l.queue = append(l.queue, msg)
	l.mu.Unlock()
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
		return l.messages
	}
	st := State{Messages: slices.Clone(l.messages), Turn: l.turn, QueueLen: l.queueLen()}
	out := l.cfg.Assembler.Assemble(ctx, st)
	if out == nil {
		return l.messages
	}
	return out
}

// streamLLM 消费 provider 流：翻译 Start/Update 事件；未产出任何内容前的
// Retryable 错误退避重试（02 §7）；取消时合成部分输出（provider 违约兜底）。
func (l *Loop) streamLLM(ctx context.Context, msgs []message.Message) (message.Message, message.Usage, error) {
	tools := make([]tool.Def, 0, len(l.tools))
	for _, t := range l.tools {
		tools = append(tools, t.Def())
	}
	opts, _ := ctxx.OptionsFrom(ctx)
	req := provider.Request{Messages: msgs, Tools: tools, Options: opts}

	msgID := newID()
	l.emit(KindMessageStart, MessageStartData{Role: message.RoleAssistant, MessageID: msgID})

	opCtx, opCancel := context.WithCancel(ctx)
	l.opCancel.Store(&cancelBox{fn: opCancel})
	defer func() {
		l.opCancel.Store(&cancelBox{})
		opCancel()
	}()

	var text, thought strings.Builder
	received := false // 已产出任何内容——有内容则不重试（避免重复计费/内容）

attemptLoop:
	for attempt := 0; ; attempt++ {
		evCh, err := l.cfg.Provider.Stream(opCtx, req)
		if err != nil {
			if opCtx.Err() != nil {
				return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
			}
			if provider.IsRetryable(err) && attempt < l.cfg.MaxStreamRetries && backoff(opCtx, attempt) == nil {
				continue
			}
			return message.Message{}, message.Usage{}, fmt.Errorf("provider stream: %w", err)
		}
		for ev := range evCh {
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
				m := e.Message
				m.ID = msgID // loop 持有 canonical ID，事件与历史一致
				m.Interrupted = e.Interrupted
				return m, e.Usage, nil
			case provider.ErrorEvent:
				if e.Retryable && !received && attempt < l.cfg.MaxStreamRetries && backoff(opCtx, attempt) == nil {
					continue attemptLoop
				}
				return message.Message{}, message.Usage{}, fmt.Errorf("provider: %w", e.Err)
			}
		}
		if opCtx.Err() != nil {
			return l.partialAssistant(msgID, &text, &thought), message.Usage{}, nil
		}
		return message.Message{}, message.Usage{},
			errors.New("provider violated contract: stream ended without MessageComplete")
	}
}

// execCalls 顺序执行工具调用（02 §2）。
func (l *Loop) execCalls(ctx context.Context, calls []message.ToolCall) {
	for _, tc := range calls {
		if l.interrupt.Load() {
			res := aborted(tc.ID)
			l.messages = append(l.messages, res.ToMessage())
			l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res})
			continue
		}

		d := Decision{Action: GuardAllow}
		if l.cfg.Guard != nil {
			d = l.cfg.Guard.Check(ctx, tc)
			l.emit(KindToolGuardDecision, ToolGuardDecisionData{Call: tc, Action: d.Action, Reason: d.Reason})
		}
		switch d.Action {
		case GuardDeny:
			res := message.ToolResult{CallID: tc.ID, IsError: true,
				Blocks: []message.Block{message.TextBlock{Text: "denied: " + d.Reason}}}
			l.messages = append(l.messages, res.ToMessage())
			l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res, Denied: true})
			continue
		case GuardRewrite:
			if d.Rewritten.ID == "" {
				d.Rewritten.ID = tc.ID
			}
			tc = d.Rewritten
		}

		l.emit(KindToolExecStart, ToolExecStartData{Call: tc})
		res := l.execTool(ctx, tc)
		l.messages = append(l.messages, res.ToMessage())
		l.emit(KindToolExecEnd, ToolExecEndData{Call: tc, Result: res})
	}
}

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
	opCtx, opCancel := context.WithCancel(tctx)
	l.opCancel.Store(&cancelBox{fn: opCancel})
	defer func() {
		l.opCancel.Store(&cancelBox{})
		opCancel()
	}()

	res := t.Exec(opCtx, tc)
	if res.CallID == "" {
		res.CallID = tc.ID
	}
	if opCtx.Err() != nil && !res.IsError {
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

func (l *Loop) finish(end EndReason, total message.Usage, err error) (RunResult, error) {
	res := RunResult{RunID: l.runID, EndReason: end, Turns: l.turn + 1, Usage: total}
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

func backoff(ctx context.Context, attempt int) error {
	d := time.Duration(50<<min(attempt, 4)) * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func aborted(callID string) message.ToolResult {
	return message.ToolResult{CallID: callID, IsError: true,
		Blocks: []message.Block{message.TextBlock{Text: "[interrupted]"}}}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("loop: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
