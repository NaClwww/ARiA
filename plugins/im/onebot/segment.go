package onebot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aria/pkg/message"
)

// TextSegments 把一段文本包成消息段（空串返回 nil）。
func TextSegments(s string) []Segment {
	if s == "" {
		return nil
	}
	return []Segment{Text(s)}
}

// Text 是文本段。
func Text(s string) Segment { return Segment{Type: "text", Data: map[string]any{"text": s}} }

// At 是 @某人 段（群聊）。
func At(userID int64) Segment {
	return Segment{Type: "at", Data: map[string]any{"qq": strconv.FormatInt(userID, 10)}}
}

// Reply 是引用回复段（引用 messageID 那条消息），按惯例放在消息最前。
func Reply(messageID int64) Segment {
	return Segment{Type: "reply", Data: map[string]any{"id": strconv.FormatInt(messageID, 10)}}
}

// Mentions 报告消息段里是否 @ 了 userID（@全体成员不算）。
func Mentions(segs []Segment, userID int64) bool {
	for _, s := range segs {
		if s.Type == "at" && s.Int("qq") == userID && userID != 0 {
			return true
		}
	}
	return false
}

// ReplyTo 返回引用回复段引用的消息 ID；没有引用为 0。
func ReplyTo(segs []Segment) int64 {
	for _, s := range segs {
		if s.Type == "reply" {
			return s.Int("id")
		}
	}
	return 0
}

// PlainText 拼接全部文本段（不含 at / 图片等）。
func PlainText(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Type == "text" {
			b.WriteString(s.Str("text"))
		}
	}
	return b.String()
}

// ImageFetcher 下载图片：返回内容与 MIME。
type ImageFetcher func(url string) (data []byte, mime string, err error)

// ToBlocks 把消息段转成规范消息块：相邻文本合并为一个 TextBlock；at 写成「@QQ号」
// （@全体成员写成「@全体成员」）；image 经 fetch 下载为 ImageBlock，fetch 为 nil 或下载失败时
// 写成文字「[图片]」；face 写成「[表情]」；reply 不产出内容（引用关系由宿主处理）；
// 其余类型写成「[类型]」。
func ToBlocks(segs []Segment, fetch ImageFetcher) []message.Block {
	var out []message.Block
	var text strings.Builder
	flush := func() {
		if t := text.String(); strings.TrimSpace(t) != "" {
			out = append(out, message.TextBlock{Text: t})
		}
		text.Reset()
	}
	for _, s := range segs {
		switch s.Type {
		case "text":
			text.WriteString(s.Str("text"))
		case "at":
			if q := s.Str("qq"); q == "all" {
				text.WriteString("@全体成员")
			} else {
				text.WriteString("@" + q)
			}
		case "reply":
		case "face":
			text.WriteString("[表情]")
		case "image":
			url := s.Str("url")
			if fetch == nil || url == "" {
				text.WriteString("[图片]")
				continue
			}
			data, mime, err := fetch(url)
			if err != nil {
				text.WriteString("[图片]")
				continue
			}
			flush()
			out = append(out, message.ImageBlock{Data: data, MIME: mime})
		default:
			text.WriteString("[" + s.Type + "]")
		}
	}
	flush()
	return out
}

// DefaultImageLimit 是 HTTPImageFetcher 的单张图片大小上限（字节）。
const DefaultImageLimit = 8 << 20

// HTTPImageFetcher 返回经 HTTP GET 下载图片的 ImageFetcher（QQ 图片段的 url 是腾讯图床直链）：
// 单次下载以 timeout 为上限，超过 limit 字节（≤0 → DefaultImageLimit）报错；MIME 取响应头，
// 不是 image/* 时按内容嗅探，嗅探结果仍不是图片则报错。hc 为 nil 时用 http.DefaultClient。
func HTTPImageFetcher(hc *http.Client, timeout time.Duration, limit int64) ImageFetcher {
	if hc == nil {
		hc = http.DefaultClient
	}
	if limit <= 0 {
		limit = DefaultImageLimit
	}
	return func(url string) ([]byte, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("onebot: fetch image: HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, "", err
		}
		if int64(len(data)) > limit {
			return nil, "", fmt.Errorf("onebot: fetch image: larger than %d bytes", limit)
		}
		mime := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(mime, "image/") {
			mime = http.DetectContentType(data)
		}
		if !strings.HasPrefix(mime, "image/") {
			return nil, "", fmt.Errorf("onebot: fetch image: not an image (%s)", mime)
		}
		if i := strings.IndexByte(mime, ';'); i > 0 {
			mime = mime[:i]
		}
		return data, mime, nil
	}
}
