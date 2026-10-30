package systemone

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"sort"

	_ "image/gif" // 解码器注册：GIF 取第一帧
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"aria/core/provider"
	"aria/pkg/message"
)

// 图片约束（TypeSafe 文档 docs.typesafe.ai/images；OpenRouter 上 Jev 的下限是 256，取更严的 280）：
// 只收 PNG / JPEG 静态图，宽高各 280~1568 px，超出范围直接拒收而不是缩放；EXIF 方向不生效。
const (
	MinImageSide = 280
	MaxImageSide = 1568
	jpegQuality  = 90
)

// Image 是预处理后可上传的图片。
type Image struct {
	Ref    string
	Data   []byte
	Format string // "png" | "jpeg"
	MIME   string
}

// PrepareImages 按引用串排序（请求内容确定）逐张预处理；任一张失败整体失败（端点也是这样）。
func PrepareImages(images map[string]message.ImageBlock) ([]Image, error) {
	refs := make([]string, 0, len(images))
	for ref := range images {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]Image, 0, len(refs))
	for _, ref := range refs {
		data, format, err := PrepareImage(images[ref].Data)
		if err != nil {
			return nil, fmt.Errorf("%w: 图片 %s: %v", provider.ErrInvalidInput, ref, err)
		}
		out = append(out, Image{Ref: ref, Data: data, Format: format, MIME: "image/" + format})
	}
	return out, nil
}

// PrepareImage 把图片整理成端点收的样子：已是 PNG / JPEG 静态图且尺寸在范围内的原样返回；
// 否则（GIF / WebP、动图 PNG、过大或过小）解码后缩放到范围内（长边 ≤ 1568，短边 ≥ 280；
// 长宽比极端、两条做不到时按长边缩放再把短边补白边），透明部分垫白，编码为 baseline JPEG。
// EXIF 方向不处理：调用方给已转正的像素（摄像头帧本就如此）。
func PrepareImage(data []byte) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("无法识别的图片: %v", err)
	}
	if (format == "jpeg" || format == "png" && !animatedPNG(data)) && inRange(cfg.Width) && inRange(cfg.Height) {
		return data, format, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("解码失败: %v", err)
	}
	b := src.Bounds()
	w, h, cw, ch := fitSize(b.Dx(), b.Dy())
	dst := image.NewRGBA(image.Rect(0, 0, cw, ch))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	off := image.Pt((cw-w)/2, (ch-h)/2)
	draw.CatmullRom.Scale(dst, image.Rectangle{Min: off, Max: off.Add(image.Pt(w, h))}, src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("编码失败: %v", err)
	}
	return buf.Bytes(), "jpeg", nil
}

func inRange(n int) bool { return n >= MinImageSide && n <= MaxImageSide }

// fitSize 返回缩放后的图像尺寸 (w, h) 与画布尺寸 (cw, ch)；画布大于图像的部分补白边。
func fitSize(sw, sh int) (w, h, cw, ch int) {
	long, short := float64(max(sw, sh)), float64(min(sw, sh))
	s := 1.0
	if long > MaxImageSide {
		s = MaxImageSide / long
	}
	if short*s < MinImageSide {
		s = MinImageSide / short
	}
	if long*s > MaxImageSide { // 长宽比极端：长边优先，短边补边
		s = MaxImageSide / long
	}
	w = max(1, int(math.Round(float64(sw)*s)))
	h = max(1, int(math.Round(float64(sh)*s)))
	return w, h, max(w, MinImageSide), max(h, MinImageSide)
}

// animatedPNG 报告 PNG 是否带 APNG 动画控制块（acTL 在首个 IDAT 之前）——端点拒收动图，
// 标准库只解第一帧，重新编码即可。
func animatedPNG(data []byte) bool {
	actl := bytes.Index(data, []byte("acTL"))
	idat := bytes.Index(data, []byte("IDAT"))
	return actl >= 0 && (idat < 0 || actl < idat)
}
