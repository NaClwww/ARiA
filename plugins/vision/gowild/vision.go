// Package gowild 是 Gowild-HE 设备的视觉插件：摄像头当前帧注入组装链
// （docs/discussions/2026-09-11-runtime-layer.md §1「当前照片」，2026-09-28
// 拍板 B 形态）：每次 LLM 调用前现拉一帧，追加在组装结果最底部（agent
// 槽 1 的额外变换位，作用在窗口组装结果之上）。不进窗口历史、不落盘——
// 窗口/压缩/record 对图片零感知，每个请求恰好一张「此刻」的帧，旧帧永不
// 重发。失败（超时/非 200/非图）安静跳过：画面要么是此刻的，要么没有，
// 不注入任何替代物。
package gowild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"aria/core/loop"
	"aria/pkg/message"
)

// FrameTimeout 是单帧抓取硬顶。组装是同步缝（docs/03 §3 红线：同步缝内
// 禁止长阻塞）——局域网热帧几十到两三百毫秒，2s 顶只防设备失联时连接
// 挂死卡住轮次。
const FrameTimeout = 2 * time.Second

// Config 是视觉插件的装配参数。
type Config struct {
	// Base 是 launcher 根地址（GET <Base>/api/camera/frame 单帧 JPEG，
	// 按需开摄像头）。必填。
	Base string
	// Note 是注入消息里的画面说明文字（模型据此理解这张图是什么）。
	// 空 = 「（摄像头当前画面）」。
	Note string
}

// Vision 实现 loop.Assembler：窗口组装结果透传，尾部追加「当前画面」
// 消息（user 角色：文本说明 + ImageBlock(Data)）。
type Vision struct {
	cfg     Config
	log     *slog.Logger
	hc      *http.Client
	timeout time.Duration // 单帧硬顶；测试可收紧
}

func New(cfg Config, log *slog.Logger) (*Vision, error) {
	if strings.TrimSuffix(cfg.Base, "/") == "" {
		return nil, errors.New("vision.New: Base 必填（launcher 根地址）")
	}
	if cfg.Note == "" {
		cfg.Note = "（摄像头当前画面）"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Vision{
		cfg:     cfg,
		log:     log,
		timeout: FrameTimeout,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
		}},
	}, nil
}

// Assemble 透传组装结果并追加当前帧。抓取失败返回原切片（不注入）。
// 每次 LLM 调用都会经过这里——工具循环中的后续调用拿到的同样是最新帧。
func (v *Vision) Assemble(ctx context.Context, s loop.State) []message.Message {
	jpeg, err := v.frame(ctx)
	if err != nil {
		v.log.Warn("vision: 当前帧获取失败（本轮不注入）", "err", err)
		return s.Messages
	}
	out := make([]message.Message, len(s.Messages), len(s.Messages)+1)
	copy(out, s.Messages)
	out = append(out, message.Message{
		Role: message.RoleUser,
		Blocks: []message.Block{
			message.TextBlock{Text: v.cfg.Note},
			message.ImageBlock{Data: jpeg, MIME: "image/jpeg"},
		},
	})
	return out
}

func (v *Vision) frame(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(v.cfg.Base, "/")+"/api/camera/frame", nil)
	if err != nil {
		return nil, err // 不可达：固定 URL 构造
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/camera/frame: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		return nil, fmt.Errorf("frame Content-Type %q 不是图片", ct)
	}
	jpeg, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(jpeg) == 0 {
		return nil, errors.New("frame body 为空")
	}
	return jpeg, nil
}
