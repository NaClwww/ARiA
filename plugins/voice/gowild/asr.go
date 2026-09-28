package gowild

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ASRConfig 是 ASR 输入插头的装配参数（06 §2 插头契约：交付「一句说完整
// 的话 + 谁说的」，成轮判定由插头自理——这里 = 信任服务端 VAD 的 final）。
type ASRConfig struct {
	// Base 是 backend 根地址（订阅 <Base>/asr/events）。必填。
	Base string
	// Speaker 是默认说话人（backend 未认主时的回落值）。必填——认主信
	// final 的 speaker_status/speaker_id 字段：matched 且带 id 用之，
	// 未匹配/旧版 backend 缺字段即回落本值。
	Speaker string
	// OnFinal 收「一句说完整的话」。必填——收不到交付的插头没有意义。
	OnFinal func(text, speaker string)
	// OnPartial 每条非空 partial 一调（可选）：只表示「mic 听到人声」，
	// 成轮与否由 final 决定。宿主拿去驱状态灯。
	OnPartial func()
}

// ASR 订阅 backend 的 ASR 事件流。断线指数退避重连（服务重启/网络抖动
// 属常态）；Run 阻塞到 ctx 取消，宿主自行起 goroutine。
type ASR struct {
	cfg ASRConfig
	log *slog.Logger
	hc  *http.Client
}

func NewASR(cfg ASRConfig, log *slog.Logger) (*ASR, error) {
	if strings.TrimSuffix(cfg.Base, "/") == "" {
		return nil, errors.New("gowild.NewASR: Base 必填（backend 根地址）")
	}
	if cfg.Speaker == "" {
		return nil, errors.New("gowild.NewASR: Speaker 必填（插头不认主，交付时的说话人名）")
	}
	if cfg.OnFinal == nil {
		return nil, errors.New("gowild.NewASR: OnFinal 必填（final 的交付口）")
	}
	if log == nil {
		log = slog.Default()
	}
	return &ASR{
		cfg: cfg,
		log: log,
		hc: &http.Client{Transport: &http.Transport{
			// SSE 长连接不能设整体超时，只约束建连速度：连不上要快点进退避。
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		}},
	}, nil
}

func (a *ASR) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for ctx.Err() == nil {
		connected, err := a.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second // 连上过就重置退避
		}
		a.log.Warn("asr: 事件流断开，重连中", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// consume 阻塞消费一条 SSE 连接直到断开；connected 报告是否成功建立过
// 连接（用于退避重置）。
func (a *ASR) consume(ctx context.Context) (connected bool, err error) {
	url := strings.TrimSuffix(a.cfg.Base, "/") + "/asr/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := a.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /asr/events: status %d", resp.StatusCode)
	}
	a.log.Info("asr: 已连接", "url", url)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// SSE 帧：`data: {json}`；`: keepalive` / 空行跳过。backend 每事件
		// 单行 JSON，不存在多行 data 聚合。
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev asrEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue // 解析失败的行按噪音丢弃，不断流
		}
		if ev.Type == "partial" {
			if strings.TrimSpace(ev.Text) != "" && a.cfg.OnPartial != nil {
				a.cfg.OnPartial() // 成轮与否由 final 决定；partial 只说明 mic 听到了人声
			}
			continue
		}
		if ev.Type != "final" {
			continue
		}
		if text := strings.TrimSpace(ev.Text); text != "" {
			a.cfg.OnFinal(text, a.speaker(ev)) // 空白判废已 trim，交付也 trim——「一句完整的话」不带边缘空白
		}
	}
	if serr := sc.Err(); serr != nil {
		return true, serr
	}
	return true, errors.New("sse stream closed by server")
}

// asrEvent 是 /asr/events 的载荷。final 可带 backend 认主字段
// （speaker_id/speaker_status）；旧版缺字段为零值，自然走回落。
type asrEvent struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	SpeakerID     string `json:"speaker_id"`
	SpeakerStatus string `json:"speaker_status"`
}

// speaker 解析「谁说的」：backend 判 matched 且带 id → 信 backend；否则
// （未匹配 / 空 id / 旧版无字段）回落装配时的默认说话人。阈值策略在
// backend（matched 就是它判过的），客户端不二次设阈。
func (a *ASR) speaker(ev asrEvent) string {
	if ev.SpeakerStatus == "matched" {
		if id := strings.TrimSpace(ev.SpeakerID); id != "" {
			return id
		}
	}
	return a.cfg.Speaker
}
