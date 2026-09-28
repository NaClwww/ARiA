package gowild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// deviceClient 把对设备播放端点的所有请求串成单连接 FIFO：一条隧道连接
// （MaxConnsPerHost=1）+ 单 worker 逐个执行，设备侧到达序 = 发出序。这是
// 「对话太快 TTS 被打烂」的根治——此前新旧会话的 POST 各自并发，旧轮次
// 没死透的 chunk 会混进新会话、旧 end 会掐断新会话的输入。
type deviceClient struct {
	base string
	log  *slog.Logger
	ops  chan deviceOp
	hc   *http.Client
}

// deviceOp 是隧道里的一次请求；ctx 取消即放弃执行（本条已死）。
// done 为 nil 表示 fire-and-forget（stop 用，不阻塞事件循环）。
type deviceOp struct {
	ctx    context.Context
	method string
	path   string
	body   []byte
	ct     string
	done   chan opResult
}

type opResult struct {
	body []byte
	err  error
}

func newDeviceClient(base string, log *slog.Logger) *deviceClient {
	c := &deviceClient{
		base: strings.TrimSuffix(base, "/"),
		log:  log,
		ops:  make(chan deviceOp, 64),
		hc: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxConnsPerHost:       1, // 单连接：FIFO 发出序才能等于到达序
			MaxIdleConnsPerHost:   1,
			ResponseHeaderTimeout: 10 * time.Second,
		}},
	}
	go c.worker()
	return c
}

func (c *deviceClient) worker() {
	for op := range c.ops {
		if op.ctx != nil && op.ctx.Err() != nil {
			if op.done != nil {
				op.done <- opResult{err: op.ctx.Err()}
			}
			continue
		}
		body, err := c.do(op)
		if op.done != nil {
			op.done <- opResult{body: body, err: err}
		}
	}
}

func (c *deviceClient) do(op deviceOp) ([]byte, error) {
	ctx := op.ctx
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}
	method := op.method
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if len(op.body) > 0 {
		reader = bytes.NewReader(op.body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+op.path, reader)
	if err != nil {
		return nil, err
	}
	if op.ct != "" {
		req.Header.Set("Content-Type", op.ct)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device %s: status %d", op.path, resp.StatusCode)
	}
	return body, nil
}

// call 同步提交（等结果与响应体）；返回的 error 供背压与失败判定。
func (c *deviceClient) call(ctx context.Context, method, path string, body []byte, ct string) ([]byte, error) {
	op := deviceOp{ctx: ctx, method: method, path: path, body: body, ct: ct, done: make(chan opResult, 1)}
	select {
	case c.ops <- op:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-op.done:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fireAndForget 异步提交（stop 用）：绝不阻塞驱动事件循环。
func (c *deviceClient) fireAndForget(path string) {
	select {
	case c.ops <- deviceOp{path: path}:
	default:
		c.log.Warn("tts: 设备请求隧道已满，丢弃", "path", path)
	}
}

// deviceSink 把 PCM 送到 launcher 的播放端点（三段式；NanoHTTPD 不收
// chunked 请求体是三段式的原因）。所有请求走 deviceClient 的单连接 FIFO，
// 顺序有保证。dead 是会话栅栏：stop() 先置死再投递 stop 操作，之后本会话
// 的任何请求（包括正卡在背压上的 chunk）一律本地丢弃——旧会话永远碰不到
// 新会话。采样率/声道来自流首 WAV 头（自适应）。
type deviceSink struct {
	cl       *deviceClient
	ctx      context.Context // 本条消息的播放 ctx（p.stop 先 cancel 再 stop）
	mu       sync.Mutex
	dead     bool
	rate     int
	channels int
}

func (d *deviceSink) begin() error {
	body := fmt.Sprintf(`{"rate":%d,"channels":%d}`, d.rate, d.channels)
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/begin", []byte(body), "application/json")
	return err
}

func (d *deviceSink) write(pcm []byte) error {
	d.mu.Lock()
	dead := d.dead
	d.mu.Unlock()
	if dead {
		return errors.New("sink stopped")
	}
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/chunk", pcm, "application/octet-stream")
	return err
}

// end 结束输入（设备端排空收尾）。会话已死则静默成功——旧的 end 决不能
// 落到新会话上把人家的输入掐断。
func (d *deviceSink) end() error {
	d.mu.Lock()
	dead := d.dead
	d.mu.Unlock()
	if dead {
		return nil
	}
	_, err := d.cl.call(d.ctx, http.MethodPost, "/api/voice/play/end", nil, "")
	return err
}

// waitDrain 轮询设备直到会话排空关闭（open:false）——设备侧 EOF 后还要
// 把已入队音频放完才收口，这里等的就是那段尾巴。闸门（灯）跟着这个函数
// 走：提前放行=灯提前灭+输入提前开。查询失败容忍连续 3 次（偶发抖动不该
// 提前收口），超时/会话已死仍一律放行：闸门宁可早开也不能卡死输入。
func (d *deviceSink) waitDrain() {
	deadline := time.Now().Add(20 * time.Second)
	fails := 0
	for time.Now().Before(deadline) {
		d.mu.Lock()
		dead := d.dead
		d.mu.Unlock()
		if dead {
			return
		}
		body, err := d.cl.call(d.ctx, http.MethodGet, "/api/voice/play", nil, "")
		if err != nil {
			fails++
			if fails >= 3 {
				return // 连续失败：设备/隧道真不可用，别死等
			}
		} else {
			fails = 0
			var st struct {
				Open bool `json:"open"`
			}
			if json.Unmarshal(body, &st) != nil || !st.Open {
				return
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// stop 立即置死 + 异步投递 /play/stop。置死先于投递：置死前入队的旧 chunk
// 在 FIFO 里位于 stop 之前，落进旧会话后即被 stop 清掉；置死后的请求本地
// 丢弃，永远到不了设备。
func (d *deviceSink) stop() {
	d.mu.Lock()
	d.dead = true
	d.mu.Unlock()
	d.cl.fireAndForget("/api/voice/play/stop")
}
