package gowild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lightState 是灯的三态词汇：待机 / 收听（用户开口中）/ 思考生成（整轮忙）。
type lightState int

const (
	lightIdle lightState = iota
	lightListening
	lightThinking
)

func (s lightState) String() string {
	switch s {
	case lightListening:
		return "listening"
	case lightThinking:
		return "thinking"
	default:
		return "idle"
	}
}

// partialWindow 判定「正在收听」的心跳新鲜度窗口：流式 ASR 说话期间数百
// 毫秒一发 partial，1.5s 没有下一发即认为用户已停口（final 还在路上，或
// VAD 判成噪音根本不会有 final）。窗口必须显著小于 ASR 断线退避间隔，
// 否则断流会让灯挂在绿色上。
const partialWindow = 1500 * time.Millisecond

// LightConfig 是状态灯的装配参数。
type LightConfig struct {
	// Base 是 launcher 根地址（POST <Base>/api/light 的 color 模式只驱
	// 12/15/18 三色通道，不碰 15 颗暖白舞台环）。必填。
	Base string
	// Colors 形如 "202020,00a000,2050ff"（idle,listening,thinking）。
	Colors string
}

// Light 把会话状态映射到机身 RGB 指示灯，订阅式驱动（无轮询）：
//
//	闸门迁移（Observe）──┐
//	ASR partial 心跳 ────┼──→ poke → 求值：thinking > listening > idle
//	心跳衰减定时器 ──────┘
//
// 求值只在目标态与已下发态不同才 POST（失败退避 5s）。事件源是宿主闸门
// 与心跳两路信号——「她正忙着说」不用各组件各自轮询。LightConfig.Colors
// 缺省暗白/绿/蓝。
type Light struct {
	base   string
	colors [3][3]int // 按 lightState 索引的 rgb
	hc     *http.Client
	log    *slog.Logger
	gate   GateState

	// poke 是求值信号（cap 1：并发信号合并成一次求值，状态不变无害）。
	poke chan struct{}
	quit chan struct{}
	done chan struct{}

	// heartbeat 由 ASR 插头的 goroutine 写、灯循环读；decay 定时器同样
	// 只由 Heartbeat 调用方触达。applied/failWant/retryNotBefore 闭锁在
	// 灯循环 goroutine。
	mu        sync.Mutex
	heartbeat time.Time
	decay     *time.Timer

	applied        lightState // 已成功下发的态；构造为 -1：首拍强制下发
	failWant       lightState // 上次失败的目标态（退避期内同目标不重试）
	retryNotBefore time.Time
}

func NewLight(cfg LightConfig, gate GateState, log *slog.Logger) (*Light, error) {
	if strings.TrimSuffix(cfg.Base, "/") == "" {
		return nil, errors.New("gowild.NewLight: Base 必填（launcher 根地址）")
	}
	if gate == nil {
		return nil, errors.New("gowild.NewLight: Gate 必填（状态灯订阅闸门迁移）")
	}
	colors, err := ParseLightColors(orDefaultLight(cfg.Colors, "202020,00a000,2050ff"))
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	l := &Light{
		base:   strings.TrimSuffix(cfg.Base, "/"),
		colors: colors,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
		log:     log,
		gate:    gate,
		poke:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		applied: lightState(-1),
	}
	gate.Observe(func(bool) { l.signal() })
	go l.loop()
	l.signal() // 初始求值：把灯收到已知的待机态（设备可能停在任意旧状态）
	return l, nil
}

// Heartbeat 报一次「mic 听到了人声」（ASR partial）。播放期间设备自己的
// 回声也会进来——无妨，闸门激活时 thinking 优先级更高；回声留下的新鲜
// 心跳最多让灯在回答结束后多绿 1.5s，随后自然回落。
func (l *Light) Heartbeat() {
	l.mu.Lock()
	l.heartbeat = time.Now()
	if l.decay == nil {
		l.decay = time.AfterFunc(partialWindow, l.signal)
	} else {
		l.decay.Reset(partialWindow)
	}
	l.mu.Unlock()
	l.signal()
}

// SettleIdle 退出前把灯收回待机（尽力而为；灯是音箱的资产，不该留在
// thinking 上过夜）。停循环后同步下发，applied 的 confinement 仍单线程。
func (l *Light) SettleIdle() {
	select {
	case <-l.quit:
		return // 已收过
	default:
	}
	close(l.quit)
	l.mu.Lock()
	if l.decay != nil {
		l.decay.Stop()
	}
	l.mu.Unlock()
	<-l.done
	if err := l.apply(lightIdle); err != nil {
		l.log.Warn("light: 退出置待机失败", "err", err)
	}
}

func (l *Light) loop() {
	defer close(l.done)
	for {
		select {
		case <-l.quit:
			return
		case <-l.poke:
			l.evaluate()
		}
	}
}

// evaluate 求值一次：thinking > listening > idle。只在目标态与已下发态
// 不同才下发；失败退避 5s（同目标态），设备不可达时不刷错误日志。
func (l *Light) evaluate() {
	l.mu.Lock()
	hb := l.heartbeat
	l.mu.Unlock()

	want := lightIdle
	if l.gate.Active() {
		want = lightThinking
	} else if time.Since(hb) < partialWindow {
		want = lightListening
	}
	if want == l.applied {
		return
	}
	if want == l.failWant && time.Now().Before(l.retryNotBefore) {
		return
	}
	if err := l.apply(want); err != nil {
		l.log.Warn("light: 下发失败", "state", want, "err", err)
		l.failWant, l.retryNotBefore = want, time.Now().Add(5*time.Second)
		return
	}
	l.applied = want
	l.failWant, l.retryNotBefore = lightIdle, time.Time{}
	l.log.Info("light: "+want.String(), "rgb", l.colors[want])
}

// apply 下发一档颜色（同步、2s 超时；灯循环单线程调用）。color 模式的
// brightness 缺省 255 = rgb 原值直发，明暗直接编进颜色里。
func (l *Light) apply(s lightState) error {
	body := fmt.Sprintf(`{"mode":"color","rgb":[%d,%d,%d]}`,
		l.colors[s][0], l.colors[s][1], l.colors[s][2])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/api/light", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /api/light: status %d", resp.StatusCode)
	}
	return nil
}

// signal 投一枚求值信号（满则丢弃：已有待处理信号，合并无害）。
func (l *Light) signal() {
	select {
	case l.poke <- struct{}{}:
	default:
	}
}

// ParseLightColors 解析 "rrggbb,rrggbb,rrggbb"（idle,listening,thinking）。
func ParseLightColors(s string) ([3][3]int, error) {
	var out [3][3]int
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return out, errors.New("需要三个逗号分隔的 hex 颜色（idle,listening,thinking），如 \"202020,00a000,2050ff\"")
	}
	for i, p := range parts {
		p = strings.TrimPrefix(strings.TrimSpace(p), "#")
		if len(p) != 6 {
			return out, fmt.Errorf("颜色 %d（%q）须为 rrggbb 六位 hex", i, p)
		}
		for j := 0; j < 3; j++ {
			v, err := strconv.ParseUint(p[j*2:j*2+2], 16, 8)
			if err != nil {
				return out, fmt.Errorf("颜色 %d（%q）解析失败：%w", i, p, err)
			}
			out[i][j] = int(v)
		}
	}
	return out, nil
}

func orDefaultLight(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
