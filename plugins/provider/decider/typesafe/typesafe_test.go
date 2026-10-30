package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/image/draw"

	"aria/core/provider"
	"aria/pkg/ctxx"
	"aria/pkg/message"
)

var qAddressed = provider.Question{Name: "addressed", Kind: provider.KindNoul, Instructions: "`items[0]` 是在对 ARiA 说话"}

func serve(t *testing.T, cfg Config, h http.HandlerFunc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL + "/" // 末尾斜杠应被去掉
	return New(cfg)
}

func TestRequestShape(t *testing.T) {
	var got map[string]any
	var r0 *http.Request
	a := serve(t, Config{APIKey: "cfg-key", LongContext: true}, func(w http.ResponseWriter, r *http.Request) {
		r0 = r
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		w.Header().Set(RequestIDHeader, "req-7")
		io.WriteString(w, `{"model": "jev-1.14", "answers": {"addressed": {"type": "noul", "noul": 0.2}},
		  "usage": {"input_tokens": 12, "output_tokens": 3}}`)
	})
	ctx := ctxx.WithCredentials(context.Background(), ctxx.Credentials{DefaultCredName: "ctx-key"})
	ctx = ctxx.WithScope(ctx, ctxx.Scope{SessionID: "home-1"})
	d, err := a.Decide(ctx, provider.DecideInput{State: "妈妈：ARiA 在吗"}, []provider.Question{qAddressed})
	if err != nil {
		t.Fatal(err)
	}
	if r0.URL.Path != Path || r0.Header.Get("Authorization") != "Bearer ctx-key" || r0.Header.Get(LongContextHeader) != "true" {
		t.Fatalf("request %s %v", r0.URL.Path, r0.Header)
	}
	// 官方端点只收 model / state / questions：不带 OpenRouter 的 provider / session_id
	if got["model"] != DefaultModel || got["state"] != "妈妈：ARiA 在吗" || len(got) != 3 {
		t.Fatalf("body %v", got)
	}
	// 请求 ID 取响应头；没有花费
	if d.ID != "req-7" || d.Model != "jev-1.14" || d.Usage.In != 12 || d.Usage.Cost != 0 || d.Answers["addressed"].P != 0.2 {
		t.Fatalf("decision %+v", d)
	}
}

func TestNoLongContextByDefault(t *testing.T) {
	a := serve(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(LongContextHeader) != "" {
			t.Error("long context header sent by default")
		}
		io.WriteString(w, `{"model": "m", "answers": {}, "usage": {"input_tokens": 1, "output_tokens": 0}}`)
	})
	d, err := a.Decide(context.Background(), provider.DecideInput{State: "s"}, []provider.Question{qAddressed})
	if err != nil || len(d.Missing) != 1 {
		t.Fatalf("d %+v err %v", d, err)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		status    int
		header    map[string]string
		body      string
		retryable bool
		msg       string
		after     time.Duration
	}{
		{400, nil, `{"detail": "Box questions are not supported by this model"}`, false, "Box questions are not supported by this model", 0},
		{422, nil, `{"detail": [{"loc": ["body", "questions", "level", "score", "criteria"], "msg": "Field required", "type": "missing"},
		  {"loc": ["body", "model"], "msg": "Input should be a valid string", "type": "string_type"}]}`,
			false, "questions.level.score.criteria: Field required; model: Input should be a valid string", 0},
		{403, nil, `{"detail": {"message": "Invalid API key"}}`, false, "Invalid API key", 0},
		{429, map[string]string{"retry-after-ms": "800"}, `{"detail": "Rate limit exceeded"}`, true, "Rate limit exceeded", 800 * time.Millisecond},
		{500, nil, `Internal Server Error`, true, "Internal Server Error", 0},
	}
	for _, c := range cases {
		a := serve(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
			for k, v := range c.header {
				w.Header().Set(k, v)
			}
			w.Header().Set(RequestIDHeader, "req-e")
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		})
		_, err := a.Decide(context.Background(), provider.DecideInput{State: "s"}, []provider.Question{qAddressed})
		var e *APIError
		if !errors.As(err, &e) || e.Status != c.status || e.Message != c.msg || e.RequestID != "req-e" || e.RetryAfter != c.after {
			t.Errorf("%d: err %v", c.status, err)
			continue
		}
		if provider.IsRetryable(err) != c.retryable {
			t.Errorf("%d: retryable %v", c.status, provider.IsRetryable(err))
		}
	}
}

// 实网：设置 TYPESAFE_API_KEY 时运行（TYPESAFE_MODEL 可换模型）。样本与 openrouter 的实网测试相同，便于对比。
func TestLiveTypeSafe(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY not set; skipping live test")
	}
	a := New(Config{APIKey: key, Model: os.Getenv("TYPESAFE_MODEL")})
	state := map[string]any{
		"assistant": "ARiA，家里的语音助手",
		"items": []map[string]string{
			{"from": "妈妈", "text": "ARiA，明天会下雨吗？"},
			{"from": "爸爸", "text": "你把那个碗递给我一下"},
		},
	}
	qs := []provider.Question{
		{Name: "addressed_0", Kind: provider.KindNoul, Instructions: "`items[0]` 是在对 ARiA 说话、或在找她"},
		{Name: "addressed_1", Kind: provider.KindNoul, Instructions: "`items[1]` 是在对 ARiA 说话、或在找她"},
		{Name: "level_0", Kind: provider.KindScore, Instructions: "`items[0]` 需要 ARiA 处理的紧迫程度",
			Criteria: []provider.Criterion{{Name: "不需要"}, {Name: "可以稍后"}, {Name: "现在"}, {Name: "紧急"}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := a.Decide(ctx, provider.DecideInput{State: state}, qs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s id=%s usage=%+v missing=%v", d.Model, d.ID, d.Usage, d.Missing)
	for _, q := range qs {
		t.Logf("%s: %+v", q.Name, d.Answers[q.Name])
	}
	if len(d.Missing) != 0 {
		t.Fatalf("missing %v", d.Missing)
	}
	if d.Answers["addressed_0"].P <= d.Answers["addressed_1"].P {
		t.Errorf("「ARiA，明天会下雨吗」应比「把碗递给我」更像在叫她")
	}
}

func testJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 带图片：danger-zone 头 + multipart（request 段的 assets 用 part 指向文件段）。
func TestImagesMultipart(t *testing.T) {
	door := provider.NewImageRef()
	data := testJPEG(t, 400, 300, color.White)
	var parts = map[string][]byte{}
	var partTypes = map[string]string{}
	a := serve(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ImagesHeader) != "true" {
			t.Error("images header missing")
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v (%s)", err, r.Header.Get("Content-Type"))
			return
		}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			parts[p.FormName()] = b
			partTypes[p.FormName()] = p.Header.Get("Content-Type")
		}
		io.WriteString(w, `{"model": "jev-1.14.0", "answers": {"addressed": {"type": "noul", "noul": 0.7}}, "usage": {"input_tokens": 300, "output_tokens": 1}}`)
	})
	in := provider.DecideInput{
		State:  map[string]any{"camera": door, "text": "妈妈：ARiA 你看"},
		Images: map[string]message.ImageBlock{door: {Data: data, MIME: "image/jpeg"}},
	}
	d, err := a.Decide(context.Background(), in, []provider.Question{qAddressed})
	if err != nil || d.Answers["addressed"].P != 0.7 {
		t.Fatalf("d %+v err %v", d, err)
	}
	var req struct {
		State  map[string]string
		Assets map[string]map[string]string
	}
	if err := json.Unmarshal(parts["request"], &req); err != nil {
		t.Fatalf("request part: %v", err)
	}
	if partTypes["request"] != "application/json" || req.State["camera"] != door {
		t.Fatalf("request part %s %s", partTypes["request"], parts["request"])
	}
	if as := req.Assets[door]; as["type"] != "image" || as["part"] != "asset-0" {
		t.Fatalf("assets %v", req.Assets)
	}
	if !bytes.Equal(parts["asset-0"], data) || partTypes["asset-0"] != "image/jpeg" { // 尺寸合规：原样透传
		t.Fatalf("file part %d bytes, type %s", len(parts["asset-0"]), partTypes["asset-0"])
	}
}

// 无图片时仍是纯 JSON、不带 images 头。
func TestNoImagesPlainJSON(t *testing.T) {
	a := serve(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ImagesHeader) != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		io.WriteString(w, `{"model": "m", "answers": {}, "usage": {"input_tokens": 1, "output_tokens": 0}}`)
	})
	if _, err := a.Decide(context.Background(), provider.DecideInput{State: "s"}, []provider.Question{qAddressed}); err != nil {
		t.Fatal(err)
	}
}

// 实网（图片）：设置 TYPESAFE_API_KEY 时运行。一次请求两张纯色图，问哪张是红色。
func TestLiveTypeSafeImage(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY not set; skipping live test")
	}
	a := New(Config{APIKey: key, Model: os.Getenv("TYPESAFE_MODEL")})
	left, right := provider.NewImageRef(), provider.NewImageRef()
	in := provider.DecideInput{
		State: map[string]any{"left": left, "right": right},
		Images: map[string]message.ImageBlock{
			left:  {Data: testJPEG(t, 320, 320, color.RGBA{220, 30, 30, 255})},
			right: {Data: testJPEG(t, 320, 320, color.RGBA{30, 60, 220, 255})},
		},
	}
	qs := []provider.Question{
		{Name: "left_red", Kind: provider.KindNoul, Instructions: "`left` 这张图整体是红色的"},
		{Name: "right_red", Kind: provider.KindNoul, Instructions: "`right` 这张图整体是红色的"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	d, err := a.Decide(ctx, in, qs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s id=%s usage=%+v missing=%v took=%v", d.Model, d.ID, d.Usage, d.Missing, time.Since(start))
	for _, q := range qs {
		t.Logf("%s: %+v", q.Name, d.Answers[q.Name])
	}
	if d.Answers["left_red"].P <= d.Answers["right_red"].P {
		t.Errorf("红图应比蓝图更像红色")
	}
}
