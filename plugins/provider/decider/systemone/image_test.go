package systemone

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"golang.org/x/image/draw"

	"aria/core/provider"
	"aria/pkg/message"
)

func encoded(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{200, 80, 40, 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func size(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

func TestPrepareImage(t *testing.T) {
	cases := []struct {
		name         string
		format       string
		w, h         int
		passthrough  bool
		wantW, wantH int
	}{
		{"jpeg in range", "jpeg", 640, 480, true, 640, 480},
		{"png in range", "png", 300, 1568, true, 300, 1568},
		{"too large", "png", 1600, 1000, false, 1568, 980},
		{"too small", "jpeg", 100, 50, false, 560, 280},
		{"extreme aspect padded", "png", 2000, 50, false, 1568, 280},
		{"gif converted", "gif", 400, 300, false, 400, 300},
	}
	for _, c := range cases {
		src := encoded(t, c.format, c.w, c.h)
		out, format, err := PrepareImage(src)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.passthrough != bytes.Equal(out, src) {
			t.Errorf("%s: passthrough = %v", c.name, !c.passthrough)
		}
		w, h, got := size(t, out)
		if got != format || w != c.wantW || h != c.wantH {
			t.Errorf("%s: %dx%d %s (reported %s), want %dx%d", c.name, w, h, got, format, c.wantW, c.wantH)
		}
		if !c.passthrough && format != "jpeg" {
			t.Errorf("%s: re-encoded as %s", c.name, format)
		}
	}
	if _, _, err := PrepareImage([]byte("not an image")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestFitSize(t *testing.T) {
	for _, c := range []struct{ sw, sh, w, h, cw, ch int }{
		{4000, 3000, 1568, 1176, 1568, 1176},
		{200, 200, 280, 280, 280, 280},
		{100, 4000, 39, 1568, 280, 1568},
	} {
		w, h, cw, ch := fitSize(c.sw, c.sh)
		if w != c.w || h != c.h || cw != c.cw || ch != c.ch {
			t.Errorf("fitSize(%d,%d) = %d %d %d %d", c.sw, c.sh, w, h, cw, ch)
		}
	}
}

func TestAnimatedPNG(t *testing.T) {
	if !animatedPNG([]byte("\x89PNG....IHDR....acTL....IDAT")) || animatedPNG([]byte("\x89PNG....IHDR....IDAT....acTL")) {
		t.Fatal("acTL detection")
	}
}

func TestPrepareImages(t *testing.T) {
	jpg := encoded(t, "jpeg", 300, 300)
	got, err := PrepareImages(map[string]message.ImageBlock{"img-b": {Data: jpg}, "img-a": {Data: jpg}})
	if err != nil || len(got) != 2 || got[0].Ref != "img-a" || got[1].MIME != "image/jpeg" {
		t.Fatalf("got %+v err %v", got, err)
	}
	_, err = PrepareImages(map[string]message.ImageBlock{"img-x": {Data: []byte("nope")}})
	if !errors.Is(err, provider.ErrInvalidInput) {
		t.Fatalf("err %v", err)
	}
}
