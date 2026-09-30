package gowild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// GatewayConfig 是 asr-gateway 输入插头的装配参数（EPYC VM 常驻网关：
// FSMN-VAD 流式判停 + 整句 ASR + CAM++ 声纹，见 Gowild-HE/asr_backend）。
// 与 ASRConfig（backend SSE）同构交付「一句说完整的话 + 谁说的」，成轮
// 判定同样信任服务端 VAD 的 final；差别在音频路径：本插头自己拉音箱
// mic 流转发给网关（backend 不再经手 ASR），闭耳也从 backend /asr/mute
// 换成网关 WS 的 reset + 本地停转（同样在源头丢流）。
type GatewayConfig struct {
	// Base 是 asr-gateway 根地址（如 ws://172.16.53.179:8210；
	// http:// 前缀自动换 ws://）。必填。
	Base string
	// MicURL 是音箱麦克风流地址（launcher /api/voice/mic/stream，
	// 16k 单声道 PCM16LE 裸流）。必填——插头自己当拉流方。
	MicURL string
	// Speaker 是默认说话人（网关未认主时的回落值）。必填。
	Speaker string
	// OnFinal 收「一句说完整的话」。必填。
	OnFinal func(text, speaker string)
	// OnSpeechStart 每条 speech_start 一调（可选）：整句 ASR 没有
	// partial，收听信号用 VAD 开口事件（原 OnPartial 的角色）。
	OnSpeechStart func()
}

// Gateway 拉音箱 mic 流 → 转发 asr-gateway WS → 消费 final 事件。mic 流
// 与 WS 任一断开都拆掉整条会话重建（指数退避，连上过即重置）；闭耳期间
// mic 在本地丢弃 + 发 reset 清网关在途话语——自回声 final 不产生。
type Gateway struct {
	cfg    GatewayConfig
	log    *slog.Logger
	dialer websocket.Dialer
	hc     *http.Client
	muted  atomic.Bool

	mu  sync.Mutex // 会话字段的读写
	ws  *websocket.Conn
	wmu sync.Mutex // WS 写串行：转发循环与 SetMuted 的 reset 竞争
}

func NewGateway(cfg GatewayConfig, log *slog.Logger) (*Gateway, error) {
	base := normalizeGatewayBase(cfg.Base)
	if base == "" {
		return nil, errors.New("gowild.NewGateway: Base 必填（asr-gateway 根地址）")
	}
	cfg.Base = base
	if strings.TrimSpace(cfg.MicURL) == "" {
		return nil, errors.New("gowild.NewGateway: MicURL 必填（音箱 /api/voice/mic/stream；需 --device）")
	}
	if cfg.Speaker == "" {
		return nil, errors.New("gowild.NewGateway: Speaker 必填（插头不认主，交付时的说话人名）")
	}
	if cfg.OnFinal == nil {
		return nil, errors.New("gowild.NewGateway: OnFinal 必填（final 的交付口）")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		cfg: cfg,
		log: log,
		hc: &http.Client{Transport: &http.Transport{
			// mic 裸流长连接不能整体超时，只约束建连速度。
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
	}, nil
}

// normalizeGatewayBase 容错 http(s):// 前缀（flag 手输常态）并去尾斜杠。
func normalizeGatewayBase(base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if s, ok := strings.CutPrefix(base, "http://"); ok {
		return "ws://" + s
	}
	if s, ok := strings.CutPrefix(base, "https://"); ok {
		return "wss://" + s
	}
	return base
}

// SetMuted 实现 FollowGate 的闭耳水槽。true：发 reset 清网关在途话语
// （VAD cache 一并丢弃）+ 本地停转 mic；false：恢复转发。幂等——reset
// 由 Swap 检出「闲→忙」沿才发。
func (g *Gateway) Set(on bool) error {
	if g.muted.Swap(on) == on {
		return nil
	}
	if on {
		g.mu.Lock()
		ws := g.ws
		g.mu.Unlock()
		if ws == nil {
			return nil // 会话未建：本地 muted 位已挡住转发
		}
		g.wmu.Lock()
		defer g.wmu.Unlock()
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"cmd":"reset"}`))
	}
	return nil
}

// Run 阻塞到 ctx 取消；宿主自行起 goroutine（app.Service）。
func (g *Gateway) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for ctx.Err() == nil {
		connected, err := g.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		g.log.Warn("asr-gateway: 会话断开，重建中", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session 建一条「WS 事件 + mic 转发」的完整会话，任一侧断开即返回；
// connected 报告是否两头都接通过（用于退避重置）。
func (g *Gateway) session(ctx context.Context) (connected bool, err error) {
	ws, _, err := g.dialer.DialContext(ctx, g.cfg.Base+"/v1/asr/ws?fmt=s16", nil)
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", g.cfg.Base, err)
	}
	defer func() {
		g.mu.Lock()
		if g.ws == ws {
			g.ws = nil
		}
		g.mu.Unlock()
		ws.Close()
	}()
	g.mu.Lock()
	g.ws = ws
	g.mu.Unlock()

	// 事件读者独占 ReadMessage；错误带回转发循环侧收尾。
	readErr := make(chan error, 1)
	go func() { readErr <- g.readEvents(ws) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.MicURL, nil)
	if err != nil {
		return true, err // 不可达：URL 由 NewGateway 校验非空
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return true, fmt.Errorf("mic stream %s: %w", g.cfg.MicURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("GET mic stream: status %d", resp.StatusCode)
	}
	g.log.Info("asr-gateway: mic → 网关已接通", "mic", g.cfg.MicURL, "gateway", g.cfg.Base)
	connected = true

	// 转发循环：mic 裸流原样进 binary 帧（网关侧自行按 2 字节对齐缓冲）。
	buf := make([]byte, 8*1024) // ~256ms @16k s16：小帧低延迟
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 && !g.muted.Load() {
			g.wmu.Lock()
			werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			g.wmu.Unlock()
			if werr != nil {
				return true, werr
			}
		} // muted：读掉不转——源头丢流；reset 已清网关在途话语
		if rerr != nil {
			return true, rerr
		}
		if rerr := readErrNonBlocking(readErr); rerr != nil {
			return true, rerr
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
	}
}

// readErrNonBlocking 非阻塞看一眼读者是否已退出（nil = 还在读）。
func readErrNonBlocking(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	default:
		return nil
	}
}

// readEvents 消费网关 JSON 事件到连接断开。闭耳瞬间在途的 final 由
// muted 检查兜底丢弃（reset 之后网关不该再发，发了也是残留）。
func (g *Gateway) readEvents(ws *websocket.Conn) error {
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ev gwEvent
		if json.Unmarshal(data, &ev) != nil {
			continue // 解析失败的事件按噪音丢弃，不断流
		}
		switch ev.Type {
		case "speech_start":
			if g.cfg.OnSpeechStart != nil && !g.muted.Load() {
				g.cfg.OnSpeechStart()
			}
		case "final":
			if text := strings.TrimSpace(ev.Text); text != "" && !g.muted.Load() {
				g.cfg.OnFinal(text, g.speaker(ev))
			}
		}
	}
}

// gwEvent 是网关 WS 事件载荷。final 带认主字段（speaker_id/
// speaker_status），语义与 backend /asr/events 一致。
type gwEvent struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	SpeakerID     string `json:"speaker_id"`
	SpeakerStatus string `json:"speaker_status"`
}

// speaker 与 backend SSE 插头同一纪律：matched 且带 id 信网关，否则回落
// 装配默认。阈值策略在网关（matched 就是它判过的）。
func (g *Gateway) speaker(ev gwEvent) string {
	if ev.SpeakerStatus == "matched" {
		if id := strings.TrimSpace(ev.SpeakerID); id != "" {
			return id
		}
	}
	return g.cfg.Speaker
}
