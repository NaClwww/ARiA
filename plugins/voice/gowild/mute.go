package gowild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MicMute 是 backend 的闭耳开关（POST /asr/mute）：闸门忙（生成/放音回合
// 中）时让麦克风流在源头就被丢弃——自回声 final 根本不会产生（比客户端
// 事后忽略干净），顺带省掉回合中白做的流式解码。下发失败只降级回
// 「闸门侧忽略」（Input 已有防线），不拖垮语音链路。
type MicMute struct {
	base string
	log  *slog.Logger
	hc   *http.Client

	mu       sync.Mutex
	lastSent *bool // 最近一次成功下发的状态；nil = 尚未同步（首次必发，兼作崩溃残留的修复）
	dead     bool  // backend 无此端点（旧版）：永久放弃
}

func NewMicMute(base string, log *slog.Logger) *MicMute {
	if log == nil {
		log = slog.Default()
	}
	return &MicMute{
		base: strings.TrimSuffix(base, "/"),
		log:  log,
		hc:   &http.Client{Timeout: 3 * time.Second},
	}
}

// Set 下发闭耳状态。幂等：与最近成功值相同则不发；404 视为 backend 过旧、
// 此后永久静默；其它失败不记成功值，下次状态变化再试。
func (m *MicMute) Set(on bool) error {
	m.mu.Lock()
	skip := m.dead || (m.lastSent != nil && *m.lastSent == on)
	m.mu.Unlock()
	if skip {
		return nil
	}

	body, _ := json.Marshal(map[string]bool{"muted": on})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.base+"/asr/mute", bytes.NewReader(body))
	if err != nil {
		return err // 不可达：固定 URL 构造
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		m.log.Warn("micmute: 下发失败（降级为闸门侧忽略）", "muted", on, "err", err)
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		m.mu.Lock()
		m.lastSent = &on
		m.mu.Unlock()
		return nil
	case http.StatusNotFound:
		m.mu.Lock()
		m.dead = true
		m.mu.Unlock()
		m.log.Info("micmute: backend 无 /asr/mute（旧版），闭耳停用——回退闸门侧忽略")
		return nil
	default:
		err = fmt.Errorf("POST /asr/mute: status %d", resp.StatusCode)
		m.log.Warn("micmute: 端点异常（降级为闸门侧忽略）", "muted", on, "err", err)
		return err
	}
}

// MuteSink 是闭耳水槽：闸门忙 → Set(true) 丢 mic，闲 → Set(false) 开耳。
// MicMute（backend /asr/mute）与 Gateway（WS reset + 本地停转）各是其实现。
type MuteSink interface {
	Set(on bool) error
}

// FollowGate 把闸门迁移接到闭耳水槽上：忙 → 闭耳；闲 → 先等 unmuteHold
// （放掉设备在途的尾巴 chunk）再确认仍闲才开耳。下发由内部 goroutine
// 异步执行——Observe 回调的契约是不做慢活。启动时若闸门闲，先补一发开
// 耳：宿主若曾在回合中崩溃，水槽会残留在闭耳态，这里负责修复。返回退订函数。
func FollowGate(gate GateState, mute MuteSink, unmuteHold time.Duration) (cancel func()) {
	if gate == nil || mute == nil {
		return func() {}
	}
	b := &muteBridge{
		gate: gate,
		mute: mute,
		hold: unmuteHold,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	un := gate.Observe(func(active bool) {
		b.mu.Lock()
		b.active = active
		b.mu.Unlock()
		select { // 唤醒可塌缩成一次：状态每轮重读，不丢语义
		case b.wake <- struct{}{}:
		default:
		}
	})
	// 对齐订阅前的现值：注册后发生的迁移已走回调，这里覆盖为最新的闸门真值。
	b.mu.Lock()
	b.active = gate.Active()
	b.mu.Unlock()
	go b.run()
	var once sync.Once
	return func() {
		once.Do(func() {
			un()
			close(b.done)
		})
	}
}

// muteBridge 是迁移→闭耳的桥：吃闸门回调（快），慢活（下发）在自己的
// goroutine 里做。hold 期间闸门再次变忙则作废本次开耳。
type muteBridge struct {
	gate GateState
	mute MuteSink
	hold time.Duration
	wake chan struct{}
	done chan struct{}

	mu     sync.Mutex
	active bool
}

func (b *muteBridge) run() {
	for {
		b.mu.Lock()
		active := b.active
		b.mu.Unlock()
		if active {
			b.mute.Set(true)
		} else {
			t := time.NewTimer(b.hold)
			select {
			case <-b.done:
				t.Stop()
				return
			case <-b.wake: // hold 期间又忙了：作废开耳，立即重看状态
				t.Stop()
				continue
			case <-t.C:
				if !b.gate.Active() { // 复查：期间可能又忙了（唤醒塌缩的兜底）
					b.mute.Set(false)
				}
			}
		}
		select {
		case <-b.done:
			return
		case <-b.wake:
		}
	}
}
