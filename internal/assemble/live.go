package assemble

import (
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"aria/internal/config"
	"aria/runtime/agent"
)

// BuildFunc 是宿主的「配置 → agent.Config」装配，启动与热更新共用同一份（参照 pi 的
// _buildRuntime）。note 描述本次装配（如 provider/模型），记入 reconfigured 事件。
// Memory / Store 也由 Build 给出（宿主用 MemoryCache / StoreCache 缓存），变更在下一次会话切换时
// 生效（Session.Reconfigure）；NewSessionID 由宿主在启动时填入。
type BuildFunc func(cfg config.Config) (ac agent.Config, note string, err error)

// Live 把配置变更应用到运行中的会话（docs/06 §3），两个宿主共用。宿主把 Apply 注册为
// Manager.OnChange：不管变更来自面板 Set、/reload 还是文件监听，都走这一处。
//
// 做法（参照 pi 的 reload）：配置有变化就用 Build 按新配置整份重新装配，交给
// Session.Reconfigure——会话、上下文窗口、事件订阅不动，装配换新；Provider 等昂贵部件
// 由 Build 内部缓存（ProviderCache）。装配失败保持原配置。
//
// 记忆服务与落盘的变更在下一次会话切换（闲置超时或 /new）时生效；监听地址、[host] 与会话标识类
// 配置只在启动时生效，变更只记「需重启」日志。
type Live struct {
	Session *agent.Session
	Build   BuildFunc
	Log     *slog.Logger

	mu   sync.Mutex
	last config.Config
}

// NewLive 以启动时的生效配置 cur 为比对基准。
func NewLive(sess *agent.Session, build BuildFunc, cur config.Config, log *slog.Logger) *Live {
	if log == nil {
		log = slog.Default()
	}
	return &Live{Session: sess, Build: build, Log: log, last: cur}
}

// Apply 按 cfg 重新装配会话；与上次应用值相同则不做任何事。并发调用串行化。
func (l *Live) Apply(cfg config.Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := l.last
	if reflect.DeepEqual(prev, cfg) {
		return
	}
	l.last = cfg

	for _, sec := range restartOnly(prev, cfg) {
		l.Log.Warn("配置热更新：该段变更需重启生效", "section", sec)
	}
	if err := l.apply(cfg); err != nil {
		l.Log.Error("配置热更新：重新装配失败，保持原配置", "err", err)
	}
}

func (l *Live) apply(cfg config.Config) error {
	ac, note, err := l.Build(cfg)
	if err != nil {
		return err
	}
	if err := l.Session.Reconfigure(ac, note); err != nil {
		return fmt.Errorf("reconfigure: %w", err)
	}
	l.Log.Info("配置热更新：已重新装配，下一轮生效", "note", note)
	return nil
}

// restartOnly 返回有变化、但只在启动时生效的配置段名。
func restartOnly(prev, cur config.Config) []string {
	var out []string
	add := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	add("host", prev.Host, cur.Host)
	add("server", prev.Server, cur.Server)
	// [session] 里 idle_timeout_s / recall_wait_ms 热生效；会话标识、默认说话人、命名空间不热换。
	ps, cs := prev.Session, cur.Session
	ps.IdleTimeoutS, cs.IdleTimeoutS = 0, 0
	ps.RecallWaitMs, cs.RecallWaitMs = 0, 0
	add("session", ps, cs)
	return out
}
