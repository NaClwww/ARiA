// Package agent 是 runtime 的总装（docs/03 §5）：Agent 是装配零件盒，
// Session 是一实例一个的长寿命容器——压缩记忆 + 飞轮 + 落盘订阅。
//
// 主线（v1）：Input(msg) → 窗口组装 → core.Run → 等本轮结算 → 返回。
// 尚未接入：ingress 成轮（随语音，03 §6）、小轮/抢话（03 §7）、投机（05 G0）。
//
// 会话切换：一个伴侣实例通常只建一个 Session（容器），从启动活到关闭；最近一轮结算后
// Config.IdleTimeout 内没有新输入时切换到新的会话标识（Scope.SessionID）并清空窗口，
// 新会话不带入上一会话的摘要与原文。
package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/runtime/artifact"
	"aria/runtime/memory"
	"aria/runtime/persist"
	"aria/runtime/toolkit"
	"aria/runtime/window"
)

var (
	// ErrSessionClosed 表示会话已关闭，不再接受新输入/排队。
	ErrSessionClosed = errors.New("agent: session closed")
	// ErrNoActiveRun 表示当前没有在途轮次，Queue 被拒（Queue 是轮间转向，不是发起轮次）。
	ErrNoActiveRun = errors.New("agent: no run in flight (Queue only injects into a running turn; use Input to start one)")
	// ErrStreamClosed 表示内部事件流意外中断（会话未关闭，但消费链断了）。
	ErrStreamClosed = errors.New("agent: event stream closed unexpectedly")
)

// DefaultCloseGrace 是 Close 等待落盘排空的默认上限。
const DefaultCloseGrace = 2 * time.Second

type Config struct {
	Provider provider.Provider
	Tools    []tool.Tool

	// Compressor 是间隙压缩实现；nil → window.KeepLast(window.DefaultKeepLast)。
	// 默认实现只保留最近若干条（静默丢弃更早内容）——要「记住更早的事」，
	// 显式接 window.ProviderCompressor 之类的摘要实现。
	Compressor window.Compressor

	// KeepRecentTurns 是压缩时保留原文的近轮数 K（window.SetKeepRecentTurns）。
	KeepRecentTurns int

	// Compact 是按上下文用量触发压缩的预留策略；窗口大小与 token 估算取自 Provider（见 CompactBudget）。
	Compact CompactBudget

	// IdleTimeout 是会话切换的无操作时长：最近一轮结算后 IdleTimeout 内没有新的 Input 时，
	// 当前会话结束（窗口清空，见 window.Reset；新会话不带入上一会话的摘要与原文），
	// 之后的输入使用新的 Scope.SessionID。0 = 不切换。
	IdleTimeout time.Duration

	// NewSessionID 生成会话切换后的会话标识，参数为切换时刻；
	// nil → SessionIDAt(NewSession 传入的 SessionID, 切换时刻)。
	NewSessionID func(now time.Time) string

	// Memory 是可选的记忆服务（docs/memory/options.md「记忆服务接口」）；nil → 不调用、不提取要点。
	// NewSession 与会话切换时通知会话开始（Start，结果写入窗口的召回位置），会话切换时先通知
	// 上一会话结束（End，结束时刻为该会话最近一轮的结算时刻）。压缩实现为 window.PointExtractor
	// （window.ProviderCompressor 已实现）时，会话内压缩与会话切换时提取要点并暂存（Stage）。
	// 调用经 memory.Client 的重试与超时，由每个 Session 的一个后台 goroutine 按投递顺序执行，
	// ctx 带该调用所属会话的 Scope；队列容量 MemoryQueueSize，队列满时丢弃该次调用并输出 Error 日志。
	// 调用使用 NewSession 传入的 Scope.Namespace。
	Memory memory.Service

	// Store 是可选的会话历史落盘（v1 只写不恢复）；nil → 不落盘。
	// 实现必须尊重 ctx 取消，否则关停时尾部事件可能写不完（见 CloseGrace）。
	Store persist.Store

	// CloseGrace 是 Close 等待落盘排空的上限；0 → DefaultCloseGrace。
	// 超时后取消落盘（尊重 ctx 的实现会立刻返回），并记一条 warn。
	CloseGrace time.Duration

	// Assembler 是 core 槽 1 的额外变换，作用在窗口组装结果之上（可空）。
	Assembler loop.Assembler
	Guard     loop.ToolGuard

	// SystemPrompt 是每轮置顶的 system 内容（人设/规则）。建议把
	// window.TagPolicyInstruction 一并写入——标签需要声明才有效（03 §5）。
	SystemPrompt string

	// ToolResultLimit 是单个工具结果的文本字符上限（rune）：超过则全文存入
	// Artifacts，只把「预览 + 引用」喂回模型（03 §1 artifact+ref stack）。
	// 0 → toolkit.DefaultLimit（4000）；负数 → 关闭截断。
	ToolResultLimit int

	// Artifacts 是超长工具结果的存放处；启用截断且为 nil 时用内存实现
	// （artifact.NewMemory(DefaultArtifactEntries)，FIFO 淘汰）。
	Artifacts artifact.Store

	MaxTurns    int
	ToolTimeout time.Duration

	// SubscribeBuffer 是每个内部订阅者的缓冲；0 → 256。
	SubscribeBuffer int

	Logger *slog.Logger
}

// DefaultArtifactEntries 是默认内存 artifact 存储的条数上限（FIFO 淘汰）。
const DefaultArtifactEntries = 64

// MemoryQueueSize 是记忆服务调用队列的容量（见 Config.Memory）。
const MemoryQueueSize = 64

// extractRetryDelays 是会话切换时要点提取失败后的重试间隔，与 Stage、End 相同（memory.DefaultRetryDelays）。
var extractRetryDelays = memory.DefaultRetryDelays

// SessionIDAt 返回以 base 为前缀、以 t 为开始时刻的会话标识：
// <base>-<YYYYMMDD>-<hhmmss.mmm>（t 所在时区）。
func SessionIDAt(base string, t time.Time) string {
	return base + "-" + t.Format("20060102-150405.000")
}

// CompactBudget 的缺省值。
const (
	DefaultReserveRatio      = 0.10  // 预留量不低于窗口的 10%
	DefaultTurnReserveTokens = 16384 // 单轮输入预留（用户输入 + 工具结果），单位 token
	DefaultConcurrentTurns   = 1     // 压缩与 agent loop 并发期间额外预留的轮数
)

// CompactBudget 描述按上下文用量触发压缩的预留量（单位 token）。每次结算时按本轮
// 模型计算：
//
//	单轮 = 输出上限 + TurnReserveTokens
//	预留量 = max(ReserveRatio × 窗口, 单轮) + ConcurrentTurns × 单轮
//
// 窗口与缺省输出上限取自 Provider.Limits；ctx 的 Options.MaxTokens 非零时作为输出上限。
// 剩余量（窗口 − 上下文用量）低于预留量时触发压缩。Provider 报告窗口未知时不压缩。
type CompactBudget struct {
	ReserveRatio      float64 // 0 → DefaultReserveRatio
	TurnReserveTokens int     // 0 → DefaultTurnReserveTokens
	ConcurrentTurns   int     // 0 → DefaultConcurrentTurns；负数 = 不额外预留
}

func (c CompactBudget) withDefaults() CompactBudget {
	if c.ReserveRatio <= 0 {
		c.ReserveRatio = DefaultReserveRatio
	}
	if c.TurnReserveTokens <= 0 {
		c.TurnReserveTokens = DefaultTurnReserveTokens
	}
	switch {
	case c.ConcurrentTurns == 0:
		c.ConcurrentTurns = DefaultConcurrentTurns
	case c.ConcurrentTurns < 0:
		c.ConcurrentTurns = 0
	}
	return c
}

// windowBudget 按 lim 与本轮输出上限 maxTokens（0 = 用 lim.MaxOutput）计算窗口预算；
// count 为 token 估算。lim.ContextWindow ≤ 0 时返回零值（不压缩）。
func (c CompactBudget) windowBudget(lim provider.Limits, maxTokens int, count func([]message.Message) int) window.Budget {
	if lim.ContextWindow <= 0 {
		return window.Budget{}
	}
	c = c.withDefaults()
	out := maxTokens
	if out <= 0 {
		out = lim.MaxOutput
	}
	turn := out + c.TurnReserveTokens
	reserve := max(int(c.ReserveRatio*float64(lim.ContextWindow)), turn) + c.ConcurrentTurns*turn
	return window.Budget{ContextWindow: lim.ContextWindow, MaxOutput: out, Reserve: reserve, Count: count}
}

// Agent 是 Setup 的产物：纯零件盒，不持会话状态。
type Agent struct {
	cfg        Config
	compressor window.Compressor
	artifacts  artifact.Store
	toolLimit  int // 0 = 关闭截断
	buf        int
	grace      time.Duration
	log        *slog.Logger
}

func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agent: Provider is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	compressor := cfg.Compressor
	if compressor == nil {
		compressor = window.KeepLast(window.DefaultKeepLast)
	}
	buf := cfg.SubscribeBuffer
	if buf <= 0 {
		buf = 256
	}
	grace := cfg.CloseGrace
	if grace <= 0 {
		grace = DefaultCloseGrace
	}
	// 工具结果截断：负数关闭；启用时必须有存放处（否则宁可长也不丢）。
	toolLimit := cfg.ToolResultLimit
	if toolLimit == 0 {
		toolLimit = toolkit.DefaultLimit
	}
	store := cfg.Artifacts
	if toolLimit > 0 && store == nil {
		store = artifact.NewMemory(DefaultArtifactEntries)
	}
	if toolLimit < 0 {
		toolLimit = 0
	}
	return &Agent{
		cfg:        cfg,
		compressor: compressor,
		artifacts:  store,
		toolLimit:  toolLimit,
		buf:        buf,
		grace:      grace,
		log:        cfg.Logger,
	}, nil
}

// buildTools 组装本会话的工具表（registry 全局包装，03 §3 挂点 5）：
// 启用截断时注册 artifact.open 读取工具，并对其他工具套上结果截断装饰器。
// 读取工具自身不截断——否则「读大块」永远读不完。
func (a *Agent) buildTools() []tool.Tool {
	tools := append([]tool.Tool(nil), a.cfg.Tools...)
	if a.toolLimit <= 0 || a.artifacts == nil {
		return tools
	}
	hasOpen := false
	for _, t := range tools {
		if t.Def().Name == artifact.OpenToolName {
			hasOpen = true
			break
		}
	}
	if !hasOpen {
		tools = append(tools, artifact.OpenTool(a.artifacts))
	} else {
		a.log.Warn("agent: tool name already taken, auto artifact reader skipped",
			"name", artifact.OpenToolName)
	}
	wrap := toolkit.Truncate(toolkit.TruncateConfig{Store: a.artifacts, Limit: a.toolLimit})
	for i, t := range tools {
		if t.Def().Name == artifact.OpenToolName {
			continue
		}
		tools[i] = wrap(t)
	}
	return tools
}

// NewSession 建一个长寿命会话。scope 必填（01 R4 fail-closed）：
// SessionID 是首个会话的标识（不可被单次输入改写；会话切换后改用 Config.NewSessionID
// 生成的标识，见 Config.IdleTimeout），UserID 是初始说话人；
// 单次 Input 的 ctx 可以用自己的 UserID 覆盖它（03 §6：当次 Input 的
// UserID = 说话人，多人共享一个 Session）。
func (a *Agent) NewSession(scope ctxx.Scope) (*Session, error) {
	if !scope.Valid() {
		return nil, loop.ErrNoScope
	}
	l, err := loop.New(loop.Config{
		Provider:    a.cfg.Provider,
		Tools:       a.buildTools(),
		Assembler:   a.cfg.Assembler,
		Guard:       a.cfg.Guard,
		MaxTurns:    a.cfg.MaxTurns,
		ToolTimeout: a.cfg.ToolTimeout,
	})
	if err != nil {
		return nil, err
	}

	// 会话长活 ctx：携带身份与 Trace。它也是本会话所有 run ctx 的生存期锚点
	// （01 §1.2：run ctx 的父级是 session，不是触发请求）。
	base, _ := ctxx.EnsureTrace(context.Background())
	base, cancel := context.WithCancel(ctxx.WithScope(base, scope))
	win := window.New(a.compressor, a.log)
	win.SetSystem(a.cfg.SystemPrompt)
	win.SetKeepRecentTurns(a.cfg.KeepRecentTurns)
	s := &Session{
		loop:    l,
		win:     win,
		scope:   scope,
		ctx:     base,
		cancel:  cancel,
		closed:  make(chan struct{}),
		dead:    make(chan struct{}),
		grace:   a.grace,
		log:     a.log,
		store:   a.cfg.Store,
		prov:    a.cfg.Provider,
		compact: a.cfg.Compact,
		idle:    a.cfg.IdleTimeout,
		newID:   a.cfg.NewSessionID,
	}
	if s.newID == nil {
		base := scope.SessionID
		s.newID = func(t time.Time) string { return SessionIDAt(base, t) }
	}
	// 压缩观测 → durable 事件直写 store（压缩发生在 run 之间，不经飞轮总线；
	// jsonl 单行锁串行化两路写入）。失败只响亮记日志——压缩本身已落地，
	// 审计写失败不该连坐会话。
	if a.cfg.Store != nil {
		win.SetOnCompress(func(cctx context.Context, rep loop.WindowCompressedData) {
			// 记在该批内容所属的会话下：会话切换后才完成的压缩仍属切换前的会话。
			sid := s.SessionID()
			if sc, ok := ctxx.ScopeFrom(cctx); ok {
				sid = sc.SessionID
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ev := loop.Event{Kind: loop.KindWindowCompressed, At: time.Now(), Data: rep}
			if err := s.store.Append(ctx, sid, ev); err != nil {
				s.log.Error("agent: window_compressed append failed", "session", sid, "err", err)
			}
		})
	}

	// 窗口订阅：durable 消息事件进本轮缓冲，AgentEnd 结算进窗口（03 §5）。
	ch, unsub := l.Subscribe(a.buf)
	s.unsubs = append(s.unsubs, unsub)
	s.wg.Add(1)
	go s.consumeWindow(ch)

	// 可选落盘：自己一条订阅 + 单 goroutine 顺序写（03 §1 存储分账）。
	// 落盘用独立 ctx——会话 ctx 在 Close 早期就被取消（用于终止在途轮），
	// 落盘还要把总线里已排队的事件排空，不能跟着一起死。
	if a.cfg.Store != nil {
		rec, err := persist.New(a.cfg.Store, a.log)
		if err != nil {
			s.Close()
			return nil, err
		}
		pch, punsub := l.Subscribe(a.buf)
		s.unsubs = append(s.unsubs, punsub)
		pbase, _ := ctxx.EnsureTrace(context.Background())
		pctx, pcancel := context.WithCancel(ctxx.WithScope(pbase, scope))
		s.persistDone = make(chan struct{})
		s.persistCancel = pcancel
		go func() {
			defer close(s.persistDone)
			// 消费者退出即摘除订阅：死订阅者会继续被克隆事件、积压到总线
			// 上限才断开（bus overflowCap），并让「durable 不丢」静默失效。
			defer punsub()
			err := rec.Consume(pctx, scope.SessionID, pch)
			switch {
			case err != nil && pctx.Err() == nil:
				s.log.Error("agent: persist stopped", "session", s.SessionID(), "err", err)
				s.fail(err)
			case err == nil && !s.isClosed():
				// 会话没关但流断了：总线判定订阅者停滞并把我们断开，
				// durable 承诺已破，必须让宿主看见（不能静默）。
				s.log.Error("agent: persist stream closed unexpectedly", "session", s.SessionID())
				s.fail(ErrStreamClosed)
			}
		}()
	}
	if a.cfg.Memory != nil {
		s.startMemory(a.cfg.Memory, scope)
	}
	return s, nil
}

// startMemory 启动记忆服务调用的后台 goroutine，注册要点上报，并通知首个会话开始。
func (s *Session) startMemory(svc memory.Service, scope ctxx.Scope) {
	s.mem = memory.NewClient(svc, s.log)
	s.memJobs = make(chan memJob, MemoryQueueSize)
	ctx, cancel := context.WithCancel(context.Background())
	s.memCancel = cancel
	s.memDone = make(chan struct{})
	go s.memLoop(ctx)
	s.win.SetOnPoints(func(bctx context.Context, seq int, points []memory.Point) {
		// 持窗口锁执行：只做非阻塞投递（见 window.SetOnPoints）。批次所属的会话取自该批压缩的 ctx。
		sc, ok := ctxx.ScopeFrom(bctx)
		if !ok {
			s.log.Warn("agent: points without scope dropped", "points", len(points))
			return
		}
		s.enqueueMemory(s.memStage(sc, seq, points))
	})
	s.enqueueMemory(s.memStart(scope, time.Now()))
}

// Session 是一个会话的全部运行时状态。并发安全：Input 之间互斥（03 §5）。
type Session struct {
	loop   *loop.Loop
	win    *window.Window
	scope  ctxx.Scope // SessionID 在会话切换时改写，读写持 mu（见 currentScope）
	ctx    context.Context
	cancel context.CancelFunc
	closed chan struct{}
	dead   chan struct{} // 内部消费链意外中断（事件流断开等）
	once   sync.Once
	deadOn sync.Once
	grace  time.Duration
	unsubs []func()
	wg     sync.WaitGroup
	log    *slog.Logger

	persistDone   chan struct{}
	persistCancel context.CancelFunc
	store         persist.Store // 压缩事件直写用；主 durable 流仍走总线订阅

	runMu sync.Mutex // 一 Session 同时只跑一轮

	prov provider.Provider // 窗口预算的限额与 token 估算来源

	mu        sync.Mutex
	inputBuf  []message.Message // 本轮的触发输入（Input 写入，结算时消费）
	turnBuf   []message.Message // 本轮产出（消息事件累积）
	turnCtx   context.Context   // 本轮 run ctx（压缩继承其身份）
	lastUsage message.Usage     // 本轮最后一次 LLM 调用的 usage（结算时判定上下文用量）
	compact   CompactBudget     // 压缩预留策略，见 SetCompactBudget
	noWindow  bool              // 已记录过「窗口未知」告警

	idle         time.Duration          // 会话切换的无操作时长，见 Config.IdleTimeout
	newID        func(time.Time) string // 会话切换后的会话标识生成
	idleTimer    *time.Timer            // 无操作计时：每轮结算时启动，Input 开始时停止
	idleGen      uint64                 // 计时代号：每次启动或停止递增，到期回调据此判定是否作废
	sessionTurns int                    // 当前会话已结算的轮数
	lastCtx      context.Context        // 当前会话最近一轮的 run ctx（会话切换时提取要点使用其值）
	lastActive   time.Time              // 当前会话最近一轮的结算时刻（End 的结束时刻）

	mem       *memory.Client     // 记忆服务调用（含重试与超时）；nil = 未配置
	memJobs   chan memJob        // 记忆服务调用队列，由 memLoop 按投递顺序执行
	memCancel context.CancelFunc // 停止 memLoop（Close 用）
	memDone   chan struct{}      // memLoop 退出信号
	runEnd    chan struct{}      // 本轮已结算的信号
	err       error              // 首个后台错误
}

// Input 阻塞跑完一轮：组装 → core.Run → 等本轮结算进窗口 → 返回。
//
// 返回即保证本轮已进入窗口（会话关闭或消费链断裂除外）：不存在「提前返回
// 留下未结算轮次」的路径——否则下一次 Input 会覆盖未结算的缓冲，导致整轮
// 历史永久丢失（审查发现的 P0）。
//
// 宿主 ctx 的值（Scope/Credentials/Budget/Options/Trace）随本轮进入 core 与压缩；
// 宿主取消或会话关闭都会终止本轮（见 runContext）。
func (s *Session) Input(ctx context.Context, msg message.Message) (loop.RunResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if err := s.checkOpen(); err != nil {
		return loop.RunResult{}, err
	}
	s.mu.Lock()
	s.stopIdleLocked() // 有新输入：当前会话继续，无操作计时作废
	s.mu.Unlock()

	runCtx, cancel := s.runContext(ctx)
	defer cancel()
	done := s.beginTurn(runCtx, msg)

	input := s.win.Assemble([]message.Message{msg})
	res, err := s.loop.Run(runCtx, input)

	select {
	case <-done:
	case <-s.closed:
		// 关停：不再保证已结算（窗口即将废弃，durable 已由落盘链负责）
	case <-s.dead:
		// 消费链断裂：停止等待（本轮自身的结果与错误照常返回，
		// 后台故障经 Err() 暴露；后续 Input 会被 checkOpen 拒绝）。
	}
	return res, err
}

// Queue 把消息注入**正在进行的** Run 的轮间（core steering），事件记为
// UserMessageInjected；空闲或本轮已判定结束时拒绝并返回 ErrNoActiveRun
// （宿主改用 Input 起一轮）。会话已关闭时返回 ErrSessionClosed。
//
// 为什么空闲要拒绝（不只是洁癖）：空闲入队的消息会在**下一次** Run 的入口被排空，
// 而窗口结算按「本轮输入 + 本轮产出」记序，于是它落到那条新输入**之后**——历史
// 变成「后说的在前」，与 06 §2「多插头谁先来谁先进」相悖，且错误顺序会随记忆
// 长期留着。
//
// 判据在 core 的运行标志（与收敛判定同锁原子），Run 一返回即拒绝。旧实现用
// runMu.TryLock 推断在途——模型循环结束后的历史结算等待期 Input 仍持锁，
// 此窗口内 Queue 误收，消息滞留到下一次 Input 且排在其输入之后
// （2026-09-28 审查修复）。
func (s *Session) Queue(msg message.Message) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if err := s.loop.Queue(msg); err != nil {
		return ErrNoActiveRun
	}
	return nil
}

// Interrupt 取消当前 LLM/工具/Guard 等待，保留部分输出（02 §6 转向语义）。
func (s *Session) Interrupt() { s.loop.Interrupt() }

// Subscribe 订阅本会话事件流：volatile 增量给渲染（TTS 边收边播），
// durable 供审计与其他消费者；cancel 退订并关闭 channel。
func (s *Session) Subscribe(buf int) (<-chan loop.Event, func()) { return s.loop.Subscribe(buf) }

// Artifacts 暴露超长工具结果的存放处（宿主可自行读回或预置内容）。
func (a *Agent) Artifacts() artifact.Store { return a.artifacts }

// History 返回窗口当前内容的只读快照：记忆（会话开始时的召回、压缩记忆与在途）与近轮。
// v1 无窗口命令（03 §5）：宿主只能看，不能据此改窗口。
func (s *Session) History() (memory, recent []message.Message) { return s.win.Snapshot() }

// SetCompressor 热替换压缩策略（03 §5 组装层的零件位）：在途压缩用旧实现跑完、
// 结果照常落下，下一个轮间隙用新的。网页面板改策略后由宿主调它——不重启、
// 不丢在途结果。
func (s *Session) SetCompressor(c window.Compressor) { s.win.SetCompressor(c) }

// SetKeepRecentTurns 热替换压缩时保留原文的近轮数 K（window.SetKeepRecentTurns），与 SetCompressor
// 同属压缩策略，配置重载时两者一并更新。
func (s *Session) SetKeepRecentTurns(k int) { s.win.SetKeepRecentTurns(k) }

// SetCompactBudget 热替换压缩预留策略（CompactBudget），自下一次结算起生效。
func (s *Session) SetCompactBudget(c CompactBudget) {
	s.mu.Lock()
	s.compact = c
	s.mu.Unlock()
}

// SetIdleTimeout 热替换会话切换的无操作时长（见 Config.IdleTimeout）：当前会话有已结算的轮次时
// 自调用时刻起按新时长重新计时；d ≤ 0 停止计时，不再切换。
func (s *Session) SetIdleTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle = d
	s.armIdleLocked()
}

// SessionID 返回当前会话标识（会话切换后为新标识）。
func (s *Session) SessionID() string { return s.currentScope().SessionID }

// WaitCompress 等待在途压缩结束（测试与关停观察用）。
func (s *Session) WaitCompress() { s.win.Wait() }

// Err 返回首个后台错误（落盘失败、事件流意外断开等）；nil = 一切正常。
// 后台故障不中断对话（对话继续在窗口里推进），但必须能被宿主看见。
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close 关闭会话，幂等。顺序与保证：
//
//  1. 关 closed —— 新输入立即失败，在途 Input 不再死等
//  2. Interrupt  —— 尽力让在途轮优雅收敛为部分输出
//  3. 取消会话 ctx —— 硬保证：在途 run ctx 随之取消（Interrupt 可能被
//     core.Run 入口重置，不能作为终止保证；core 的 interrupt 是转向语义）
//  4. 等在途 Input 退出
//  5. 退订 —— 总线把已排队事件投递完再关闭 channel
//  6. 有界等落盘排空（CloseGrace），超时则取消落盘并 warn
//  7. 等内部消费者收尾、取消并等在途压缩
//  8. 停止记忆服务调用（Config.Memory），队列中尚未执行的调用丢弃
func (s *Session) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.mu.Lock()
		s.stopIdleLocked()
		s.mu.Unlock()
		s.loop.Interrupt()
		s.cancel()

		s.runMu.Lock()
		s.runMu.Unlock()

		for _, u := range s.unsubs {
			u()
		}
		if s.persistDone != nil {
			select {
			case <-s.persistDone:
			case <-time.After(s.grace):
				s.log.Warn("agent: persist drain timed out; tail events may be unsaved",
					"session", s.SessionID(), "grace", s.grace)
				s.persistCancel()
			}
		}
		s.wg.Wait()
		s.win.Close()
		if s.memDone != nil {
			// 关闭缺口搁置（docs/memory/options.md 已定第 9 项）：队列中尚未执行的调用丢弃。
			s.memCancel()
			<-s.memDone
			if n := len(s.memJobs); n > 0 {
				s.log.Warn("agent: memory calls dropped on close", "session", s.SessionID(), "pending", n)
			}
		}
	})
	return nil
}

// ---------- 内部 ----------

func (s *Session) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// checkOpen 报告会话是否还能接受新输入：已关闭 → ErrSessionClosed；
// 消费链已断（窗口再也不会结算）→ ErrStreamClosed，不能继续无声降级。
func (s *Session) checkOpen() error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	select {
	case <-s.dead:
		return ErrStreamClosed
	default:
		return nil
	}
}

// fail 记录首个后台错误并唤醒等待者（dead 只关一次）。
func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.deadOn.Do(func() { close(s.dead) })
}

// runContext 组装本轮 run ctx：值全部来自宿主 ctx（Scope/Credentials/Budget/
// Options/Trace 因此照常可用），生存期同时挂在会话长活 ctx 上——会话关闭能
// 终止本轮（01 §1.2）。Scope 的 SessionID 由会话锚定，UserID 允许当次覆盖
// （03 §6：当次 Input 的 UserID = 说话人）。
func (s *Session) runContext(host context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(host)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctxx.WithScope(ctx, mergeScope(s.currentScope(), ctx)), func() {
		stop()
		cancel()
	}
}

func mergeScope(session ctxx.Scope, ctx context.Context) ctxx.Scope {
	out := session
	// 宿主 ctx 的 Scope 可只带 UserID（无 SessionID，Valid 为 false），按原始值读取覆盖字段；
	// ctx 中无 Scope 时得到零值，以下判定均不生效。
	host, _ := ctxx.ScopeOverrideFrom(ctx)
	if host.UserID != "" {
		out.UserID = host.UserID
	}
	if host.AgentID != "" {
		out.AgentID = host.AgentID
	}
	return out
}

func (s *Session) beginTurn(ctx context.Context, msg message.Message) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 深拷贝：inputBuf 存活到 settle，不能持有调用方的切片底层（01 §2）。
	s.inputBuf = []message.Message{msg.Clone()}
	s.turnBuf = nil
	s.turnCtx = ctx
	s.lastUsage = message.Usage{}
	s.runEnd = make(chan struct{})
	return s.runEnd
}

func (s *Session) consumeWindow(ch <-chan loop.Event) {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				if !s.isClosed() {
					// 会话没关但事件流断了：窗口再也不会结算，
					// 必须唤醒在途 Input 并让宿主看见（否则永久等待）。
					s.log.Error("agent: window stream closed unexpectedly", "session", s.SessionID())
					s.fail(ErrStreamClosed)
				}
				return
			}
			s.handleEvent(ev)
		}
	}
}

// handleEvent 只认「进历史的 durable 消息」与本轮结束：消息取数统一经
// loop.HistoryMessage（「哪些事件进历史」的权威定义，记忆 Pump 共用）；
// AgentEnd → 结算本轮进窗口。
func (s *Session) handleEvent(ev loop.Event) {
	if d, ok := ev.Data.(loop.MessageEndData); ok {
		s.mu.Lock()
		s.lastUsage = d.Usage
		s.mu.Unlock()
	}
	if m, ok := loop.HistoryMessage(ev); ok {
		s.appendTurn(m)
		return
	}
	if _, ok := ev.Data.(loop.AgentEndData); ok {
		s.settle()
	}
}

func (s *Session) appendTurn(m message.Message) {
	s.mu.Lock()
	s.turnBuf = append(s.turnBuf, m)
	s.mu.Unlock()
}

func (s *Session) settle() {
	s.mu.Lock()
	turn := make([]message.Message, 0, len(s.inputBuf)+len(s.turnBuf))
	turn = append(turn, s.inputBuf...)
	turn = append(turn, s.turnBuf...)
	ctx := s.turnCtx
	s.inputBuf, s.turnBuf, s.turnCtx = nil, nil, nil
	usage := s.lastUsage
	s.lastUsage = message.Usage{}
	compact := s.compact
	done := s.runEnd
	s.runEnd = nil
	s.mu.Unlock()

	if ctx == nil {
		ctx = ctxx.WithScope(s.ctx, s.currentScope()) // 没有触发 ctx 时使用会话 ctx 与当前会话身份
	}
	s.win.SetBudget(s.budget(ctx, compact))
	used := 0
	if usage.In > 0 {
		used = usage.In + usage.Out // 本轮最后一次请求的规模，即下一轮组装中已有部分的规模
	}
	s.win.Settle(ctx, turn, used)
	s.mu.Lock()
	s.lastCtx, s.lastActive = ctx, time.Now()
	s.sessionTurns++
	s.armIdleLocked()
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// currentScope 返回会话 Scope 的副本（SessionID 为当前会话标识）。
func (s *Session) currentScope() ctxx.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope
}

// stopIdleLocked 停止无操作计时，并使已到期、尚未执行的回调作废（调用方持有 s.mu）。
func (s *Session) stopIdleLocked() {
	s.idleGen++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// armIdleLocked 按 s.idle 重新启动无操作计时（调用方持有 s.mu）；会话已关闭、未设置时长
// 或当前会话没有已结算的轮次时只停止计时。
func (s *Session) armIdleLocked() {
	s.stopIdleLocked()
	if s.idle <= 0 || s.sessionTurns == 0 || s.isClosed() {
		return
	}
	gen := s.idleGen
	s.idleTimer = time.AfterFunc(s.idle, func() { s.onIdle(gen) })
}

// onIdle 是无操作计时到期的回调。持 runMu 与 Input 互斥：在途轮次结束后才执行，期间开始的
// Input 已使计时代号改变，回调随之作废。代号未变且当前会话有已结算的轮次时切换会话：
// 改用新的 SessionID，并清空窗口（window.Reset，新会话不带入上一会话的摘要与原文）；
// 配置了记忆服务时依次投递：清除原文的要点提取与暂存（压缩实现为 window.PointExtractor 时）、
// End（上一会话）、Start（新会话）。
// 切换由 s.log 输出一条 Info 日志 agent: session switched，dropped_messages 为清除的原文条数。
func (s *Session) onIdle(gen uint64) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.isClosed() {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if gen != s.idleGen || s.sessionTurns == 0 {
		s.mu.Unlock()
		return
	}
	prevScope, lastCtx, lastActive := s.scope, s.lastCtx, s.lastActive
	prev := prevScope.SessionID
	next := s.newID(now)
	if next == "" || next == prev {
		s.log.Warn("agent: NewSessionID returned an empty or unchanged id, SessionIDAt used",
			"session", prev, "got", next)
		next = SessionIDAt(prev, now)
	}
	s.scope.SessionID = next
	nextScope := s.scope
	s.sessionTurns = 0
	s.lastCtx, s.lastActive = nil, time.Time{}
	s.idleTimer = nil
	idle := s.idle
	s.mu.Unlock()

	cleared := s.win.Reset()
	dropped := len(cleared.Raw)
	if s.mem != nil {
		// Reset 返回前，上一会话已完成批次的要点已投递（window.SetOnPoints）；清除的原文以会话内摘要
		// 为上下文提取要点，以下一个序号暂存；End 排在两者之后。
		if len(cleared.Raw) > 0 && s.win.ExtractsPoints() {
			// 身份与会话内批次一致：取最近一轮 run ctx 的 Scope（含该轮说话人的 UserID）。
			batchScope := prevScope
			if lastCtx == nil {
				lastCtx = ctxx.WithScope(s.ctx, prevScope)
			} else if sc, ok := ctxx.ScopeFrom(lastCtx); ok && sc.SessionID == prev {
				batchScope = sc
			}
			s.enqueueMemory(s.memExtract(lastCtx, batchScope, cleared.Seq+1, cleared.Memory, cleared.Raw))
		}
		if lastActive.IsZero() {
			lastActive = now
		}
		s.enqueueMemory(s.memEnd(prevScope, lastActive))
		s.enqueueMemory(s.memStart(nextScope, now))
	}
	s.log.Info("agent: session switched", "session", prev, "next", next, "idle", idle, "dropped_messages", dropped)
}

// memJob 是一次记忆服务调用：memLoop 以带 scope 的 ctx 执行 run；kind 与 scope.SessionID 用于日志。
type memJob struct {
	kind  string // stage、extract、end、start
	scope ctxx.Scope
	run   func(ctx context.Context)
}

// memStage 返回一批要点的暂存（Stage），batch_id 在此生成。
func (s *Session) memStage(sc ctxx.Scope, seq int, points []memory.Point) memJob {
	req := memory.StageRequest{
		Namespace: sc.Namespace, SessionID: sc.SessionID, BatchID: ctxx.NewTrace().TraceID, Seq: seq, Points: points,
	}
	return memJob{kind: "stage", scope: sc, run: func(ctx context.Context) { _ = s.mem.Stage(ctx, req) }}
}

// memExtract 返回会话切换时对清除原文的要点提取：提取使用 vctx 的值（该会话最近一轮的 run ctx），
// 生存期随执行时的 ctx（Close 时取消）；mem（会话内摘要）只用于理解上下文，要点从 msgs 提取，非空时
// 以 seq 暂存到 sc 所指的会话。提取失败后按 extractRetryDelays 重试 3 次（间隔 1 s / 2 s / 4 s），期间
// 队列中其后的调用（End、Start）等待；仍失败时由 s.log 输出 Error 日志
// agent: session-end point extraction failed, points dropped，不暂存。
func (s *Session) memExtract(vctx context.Context, sc ctxx.Scope, seq int, mem, msgs []message.Message) memJob {
	return memJob{kind: "extract", scope: sc, run: func(ctx context.Context) {
		ectx, cancel := context.WithCancel(ctxx.Detached(vctx))
		stop := context.AfterFunc(ctx, cancel)
		var points []memory.Point
		err := memory.Retry(ctx, extractRetryDelays, func(context.Context) error {
			var err error
			points, err = s.win.ExtractPoints(ectx, mem, msgs)
			return err
		})
		stop()
		cancel()
		if err != nil {
			s.log.Error("agent: session-end point extraction failed, points dropped",
				"session", sc.SessionID, "messages", len(msgs), "err", err)
			return
		}
		if len(points) > 0 {
			s.memStage(sc, seq, points).run(ctx)
		}
	}}
}

// memEnd 返回会话结束的通知（End），at 为该会话的结束时刻。
func (s *Session) memEnd(sc ctxx.Scope, at time.Time) memJob {
	req := memory.SessionRequest{Namespace: sc.Namespace, SessionID: sc.SessionID, At: at}
	return memJob{kind: "end", scope: sc, run: func(ctx context.Context) { _ = s.mem.End(ctx, req) }}
}

// memStart 返回会话开始的通知（Start），成功时召回结果写入窗口（injectRecall）。
func (s *Session) memStart(sc ctxx.Scope, at time.Time) memJob {
	req := memory.SessionRequest{Namespace: sc.Namespace, SessionID: sc.SessionID, At: at}
	return memJob{kind: "start", scope: sc, run: func(ctx context.Context) {
		if items, err := s.mem.Start(ctx, req); err == nil {
			s.injectRecall(req.SessionID, items)
		}
	}}
}

// enqueueMemory 非阻塞地投递一次记忆服务调用；队列满时丢弃并由 s.log 输出 Error 日志
// agent: memory queue full, call dropped。可在持窗口锁时调用（不获取会话锁）。
func (s *Session) enqueueMemory(j memJob) {
	select {
	case s.memJobs <- j:
	default:
		s.log.Error("agent: memory queue full, call dropped", "kind", j.kind, "session", j.scope.SessionID)
	}
}

// memLoop 按投递顺序执行记忆服务调用，直至 ctx 取消（Close）；每次调用的 ctx 带该调用所属会话的 Scope。
// 失败的日志由 memory.Client 输出。
func (s *Session) memLoop(ctx context.Context) {
	defer close(s.memDone)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.memJobs:
			j.run(ctxx.WithScope(ctx, j.scope))
		}
	}
}

// injectRecall 把 Start 返回的召回结果写入窗口的召回位置（window.SetRecalled）；sessionID 已不是
// 当前会话时丢弃。持会话锁调用窗口方法（锁顺序：会话锁在前、窗口锁在后）。
func (s *Session) injectRecall(sessionID string, items []memory.Item) {
	if len(items) == 0 {
		return
	}
	msg := window.RecallMessage(renderRecall(items))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope.SessionID != sessionID {
		return
	}
	s.win.SetRecalled([]message.Message{msg})
}

// renderRecall 把召回条目渲染为逐行文本：有发生时间的条目带本地时间前缀，暂存条目标注「未合并」。
func renderRecall(items []memory.Item) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		if !it.At.IsZero() {
			b.WriteString("[" + it.At.Local().Format("2006-01-02 15:04") + "] ")
		}
		b.WriteString(it.Text)
		if it.Source == memory.SourceStaged {
			b.WriteString("（未合并）")
		}
	}
	return b.String()
}

// budget 按本轮 ctx 的模型与输出上限计算窗口预算。Provider 报告窗口未知时返回零值
// （不压缩），并在本会话首次出现时记一条 Warn 日志。
func (s *Session) budget(ctx context.Context, compact CompactBudget) window.Budget {
	opts, _ := ctxx.OptionsFrom(ctx)
	model := opts.Model
	lim := s.prov.Limits(model)
	if lim.ContextWindow <= 0 {
		s.mu.Lock()
		warn := !s.noWindow
		s.noWindow = true
		s.mu.Unlock()
		if warn {
			s.log.Warn("agent: model context window unknown, usage-based compaction disabled",
				"session", s.SessionID(), "model", model)
		}
		return window.Budget{}
	}
	count := func(ms []message.Message) int { return provider.CountTokens(s.prov, model, ms) }
	return compact.windowBudget(lim, opts.MaxTokens, count)
}
