package assemble

import (
	"time"

	"aria/internal/config"
	"aria/runtime/agent"
)

// IdleTimeout 把 [session] idle_timeout_s 换算为 agent.Config.IdleTimeout（两个宿主的
// agent.Config 与配置重载共用）。
func IdleTimeout(c config.Session) time.Duration {
	return time.Duration(c.IdleTimeoutS) * time.Second
}

// SessionIDs 返回启动时刻 now 的会话标识与会话切换时的标识生成函数（agent.Config.NewSessionID），
// 二者均为 agent.SessionIDAt(c.ID, 会话开始时刻)。
func SessionIDs(c config.Session, now time.Time) (string, func(time.Time) string) {
	base := c.ID
	return agent.SessionIDAt(base, now), func(t time.Time) string { return agent.SessionIDAt(base, t) }
}
