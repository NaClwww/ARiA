// Package ctxx 是全项目唯一合法的 ctx 值存取点（docs/01 §1）。
// 私有 key 结构体，禁止裸 context.WithValue；Scope 缺失 fail-closed（R4）。
package ctxx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"

	"aria/pkg/message"
)

// ---------- Scope ----------

// Scope 是记忆隔离与计费聚合的锚点。Valid 要求 SessionID 非空——
// run ctx 的父级是 session（01 §1.2），无 session 的运行一律拒绝。
type Scope struct {
	UserID    string
	SessionID string
	AgentID   string
	Namespace string
}

func (s Scope) Valid() bool { return s.SessionID != "" }

type scopeKey struct{}

func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom 返回 false 即 Scope 缺失，调用方必须拒绝执行（R4 fail-closed）。
func ScopeFrom(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	return s, ok && s.Valid()
}

// ---------- Trace ----------

type Trace struct{ TraceID, SpanID string }

type traceKey struct{}

func WithTrace(ctx context.Context, t Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

func TraceFrom(ctx context.Context) (Trace, bool) {
	t, ok := ctx.Value(traceKey{}).(Trace)
	return t, ok
}

// EnsureTrace 在触发层缺失 Trace 时自动生成（01 §1.1 的兜底）。
func EnsureTrace(ctx context.Context) (context.Context, Trace) {
	if t, ok := TraceFrom(ctx); ok {
		return ctx, t
	}
	t := Trace{TraceID: newID(), SpanID: newID()}
	return WithTrace(ctx, t), t
}

// ---------- Credentials ----------

// Credentials 按 provider 名存 key/token；只负责传播，存储归宿主（05 B3）。
type Credentials map[string]string

type credKey struct{}

// WithCredentials 存副本，派生不可改写（R2 精神：值不可变）。
func WithCredentials(ctx context.Context, c Credentials) context.Context {
	cp := make(Credentials, len(c))
	for k, v := range c {
		cp[k] = v
	}
	return context.WithValue(ctx, credKey{}, cp)
}

func CredentialFrom(ctx context.Context, provider string) (string, bool) {
	c, ok := ctx.Value(credKey{}).(Credentials)
	if !ok {
		return "", false
	}
	v, ok := c[provider]
	return v, ok
}

// ---------- Budget ----------

// Budget 以指针入 ctx：派生共享同一实例，原子扣减全局可见（派生不复制，01 §1.4）。
// 费用以微单位整型存储以支持原子操作；单写者飞轮下原子性是防御性的。
type Budget struct {
	MaxTokens    int64 // 0 = 不限
	MaxCostMicro int64 // 0 = 不限
	usedTokens   atomic.Int64
	usedCost     atomic.Int64
}

func NewBudget(maxTokens int64, maxCost float64) *Budget {
	return &Budget{MaxTokens: maxTokens, MaxCostMicro: int64(maxCost * 1e6)}
}

// Consume 在 MessageComplete 后调用（02 §7）。
func (b *Budget) Consume(u message.Usage) {
	b.usedTokens.Add(int64(u.In + u.Out))
	b.usedCost.Add(int64(u.Cost * 1e6))
}

func (b *Budget) Exceeded() bool {
	if b.MaxTokens > 0 && b.usedTokens.Load() >= b.MaxTokens {
		return true
	}
	if b.MaxCostMicro > 0 && b.usedCost.Load() >= b.MaxCostMicro {
		return true
	}
	return false
}

func (b *Budget) Used() (tokens int64, costMicro int64) {
	return b.usedTokens.Load(), b.usedCost.Load()
}

type budgetKey struct{}

func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

// BudgetFrom 返回 nil 表示不限额。
func BudgetFrom(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}

// ---------- Options ----------

// Options 承载 model 覆盖、采样参数等；业务层一次性写入，运行中只读。
type Options struct {
	Model       string
	Temperature *float64
	MaxTokens   int
	Stop        []string
}

type optionsKey struct{}

func WithOptions(ctx context.Context, o Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, o)
}

func OptionsFrom(ctx context.Context) (Options, bool) {
	o, ok := ctx.Value(optionsKey{}).(Options)
	return o, ok
}

// ---------- 分离任务 ----------

// Detached 供异步落盘等分离任务使用：保留值、脱离取消（01 §1.2）。
// 生命周期收口（关机等待）归 runtime 调度（G1 草稿）。
func Detached(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ctxx: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
