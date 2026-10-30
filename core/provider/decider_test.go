package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"aria/pkg/message"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var (
	qAddressed = Question{Name: "addressed", Kind: KindNoul, Instructions: "`items[0]` 是在对 ARiA 说话"}
	qRoute     = Question{Name: "route", Kind: KindChoice, Instructions: "该交给谁",
		Criteria: []Criterion{{Name: "aria"}, {Name: "human"}, {Name: "none"}}}
	qLevel = Question{Name: "level", Kind: KindScore, Instructions: "有多重要",
		Criteria: []Criterion{{Name: "低"}, {Name: "中"}, {Name: "高"}, {Name: "紧急"}}}
)

func TestValidateQuestions(t *testing.T) {
	cases := []struct {
		name string
		qs   []Question
		caps DeciderCaps
		want error
	}{
		{"ok", []Question{qAddressed, qRoute, qLevel}, DeciderCaps{}, nil},
		{"empty", nil, DeciderCaps{}, ErrInvalidQuestion},
		{"dup name", []Question{qAddressed, qAddressed}, DeciderCaps{}, ErrInvalidQuestion},
		{"unknown kind", []Question{{Name: "x", Kind: "box"}}, DeciderCaps{}, ErrInvalidQuestion},
		{"choice no options", []Question{{Name: "x", Kind: KindChoice}}, DeciderCaps{}, ErrInvalidQuestion},
		{"dup option", []Question{{Name: "x", Kind: KindScore, Criteria: []Criterion{{Name: "a"}, {Name: "a"}}}}, DeciderCaps{}, ErrInvalidQuestion},
		{"score too many", []Question{{Name: "x", Kind: KindScore, Criteria: levels(11)}}, DeciderCaps{}, ErrInvalidQuestion},
		{"noul criteria ok", []Question{{Name: "x", Kind: KindNoul, Criteria: []Criterion{{Name: "true"}, {Name: "false"}}}}, DeciderCaps{}, nil},
		{"noul criteria bad", []Question{{Name: "x", Kind: KindNoul, Criteria: []Criterion{{Name: "yes"}, {Name: "no"}}}}, DeciderCaps{}, ErrInvalidQuestion},
		{"kind unsupported", []Question{qAddressed, qLevel}, DeciderCaps{Kinds: []QuestionKind{KindNoul, KindChoice}}, ErrUnsupportedQuestion},
		{"options over caps", []Question{qRoute}, DeciderCaps{MaxOptions: 2}, ErrUnsupportedQuestion},
		{"levels over caps", []Question{qLevel}, DeciderCaps{MaxLevels: 3}, ErrUnsupportedQuestion},
	}
	for _, c := range cases {
		err := ValidateQuestions(c.qs, c.caps)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func levels(n int) []Criterion {
	cs := make([]Criterion, n)
	for i := range cs {
		cs[i] = Criterion{Name: strings.Repeat("l", i+1)}
	}
	return cs
}

func TestNormalizeNoul(t *testing.T) {
	a, ok := NormalizeAnswer(qAddressed, Answer{P: 0.9})
	if !ok || a.Kind != KindNoul || a.P != 0.9 || !near(a.Confidence, 0.8) || a.Probs != nil {
		t.Fatalf("got %+v ok=%v", a, ok)
	}
	for _, p := range []float64{-0.1, 1.1, math.NaN()} {
		if _, ok := NormalizeAnswer(qAddressed, Answer{P: p}); ok {
			t.Errorf("P=%v accepted", p)
		}
	}
	if _, ok := NormalizeAnswer(qAddressed, Answer{Kind: KindChoice, P: 0.5}); ok {
		t.Error("kind mismatch accepted")
	}
}

func TestNormalizeChoice(t *testing.T) {
	// 缺的选项补 0；和不为 1 时归一；confidence = (pmax-1/n)/(1-1/n)
	a, ok := NormalizeAnswer(qRoute, Answer{Probs: map[string]float64{"aria": 0.6, "human": 0.2}})
	if !ok || a.Choice != "aria" {
		t.Fatalf("got %+v ok=%v", a, ok)
	}
	if !near(a.Probs["aria"], 0.75) || !near(a.Probs["none"], 0) || len(a.Probs) != 3 {
		t.Fatalf("probs %v", a.Probs)
	}
	if want := (0.75 - 1.0/3) / (1 - 1.0/3); !near(a.Confidence, want) {
		t.Fatalf("confidence %v want %v", a.Confidence, want)
	}
	// 并列取 criteria 靠前者；均匀分布 confidence 为 0
	a, _ = NormalizeAnswer(qRoute, Answer{Probs: map[string]float64{"none": 1, "human": 1, "aria": 1}})
	if a.Choice != "aria" || !near(a.Confidence, 0) {
		t.Fatalf("uniform: %+v", a)
	}
	for name, probs := range map[string]map[string]float64{
		"unknown key": {"aria": 0.5, "robot": 0.5},
		"all zero":    {"aria": 0},
		"negative":    {"aria": -0.1, "human": 1},
	} {
		if _, ok := NormalizeAnswer(qRoute, Answer{Probs: probs}); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestNormalizeScore(t *testing.T) {
	// 例子取自公开文档：四档，分布集中在「高」与「紧急」
	a, ok := NormalizeAnswer(qLevel, Answer{Probs: map[string]float64{"低": 0, "中": 0.02, "高": 0.58, "紧急": 0.4}})
	if !ok || a.Choice != "高" || !near(a.Score, 0.02+2*0.58+3*0.4) {
		t.Fatalf("got %+v ok=%v", a, ok)
	}
	// MAD_unif(n=4) = (1.5+0.5+0.5+1.5)/4 = 1；spread = 0.02*1 + 0.4*1 = 0.42
	if !near(a.Confidence, 1-0.42) {
		t.Fatalf("confidence %v", a.Confidence)
	}
	// 远离峰值的概率扣得更多
	far, _ := NormalizeAnswer(qLevel, Answer{Probs: map[string]float64{"低": 0.4, "高": 0.6}})
	if !(far.Confidence < a.Confidence) {
		t.Fatalf("far %v should be below adjacent %v", far.Confidence, a.Confidence)
	}
	single, _ := NormalizeAnswer(Question{Name: "s", Kind: KindScore, Criteria: levels(1)}, Answer{Probs: map[string]float64{"l": 1}})
	if single.Confidence != 1 {
		t.Fatalf("single level confidence %v", single.Confidence)
	}
}

func TestNormalizeWithoutProbs(t *testing.T) {
	a, ok := NormalizeAnswer(qRoute, Answer{Choice: "human", Confidence: 0.7})
	if !ok || a.Choice != "human" || a.Probs != nil || a.Confidence != 0.7 {
		t.Fatalf("choice: %+v ok=%v", a, ok)
	}
	if _, ok := NormalizeAnswer(qRoute, Answer{Choice: "robot"}); ok {
		t.Error("unknown choice accepted")
	}
	a, ok = NormalizeAnswer(qLevel, Answer{Score: 2.38})
	if !ok || a.Choice != "高" || a.Score != 2.38 {
		t.Fatalf("score: %+v ok=%v", a, ok)
	}
	if _, ok := NormalizeAnswer(qLevel, Answer{Score: 3.5}); ok {
		t.Error("out-of-range score accepted")
	}
}

func TestFakeDeciderScript(t *testing.T) {
	d := NewFakeDecider(DeciderStep{Answers: map[string]Answer{
		"addressed": {P: 0.92},
		"route":     {Kind: KindNoul, P: 1}, // 类型不符 → Missing
		"unasked":   {P: 0.5},
	}})
	state := map[string]any{"items": []map[string]string{{"from": "妈妈", "text": "ARiA 明天下雨吗"}}}
	got, err := d.Decide(context.Background(), DecideInput{State: state}, []Question{qAddressed, qRoute, qLevel})
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Answers["addressed"]; a.P != 0.92 || !near(a.Confidence, 0.84) {
		t.Fatalf("addressed %+v", a)
	}
	if len(got.Answers) != 1 || strings.Join(got.Missing, ",") != "route,level" {
		t.Fatalf("answers %v missing %v", got.Answers, got.Missing)
	}
	if got.Model != "fake" || got.ID != "fake-1" || got.Usage.In <= 0 {
		t.Fatalf("meta %+v", got)
	}
	// 记录的是调用时刻的编码
	state["items"] = nil
	calls := d.Calls()
	if len(calls) != 1 || !strings.Contains(string(calls[0].State), "明天下雨") || len(calls[0].Questions) != 3 {
		t.Fatalf("calls %+v", calls)
	}
	if d.Left() != 0 {
		t.Fatal("step not consumed")
	}
	var fatal *FatalError
	if _, err := d.Decide(context.Background(), DecideInput{State: state}, []Question{qAddressed}); !errors.As(err, &fatal) {
		t.Fatalf("exhausted: %v", err)
	}
}

func TestFakeDeciderErrors(t *testing.T) {
	boom := errors.New("boom")
	d := NewFakeDecider(
		DeciderStep{Err: boom, Retryable: true},
		DeciderStep{Err: boom},
		DeciderStep{Delay: time.Hour},
	)
	qs := []Question{qAddressed}
	if _, err := d.Decide(context.Background(), DecideInput{State: "s"}, qs); !IsRetryable(err) || !errors.Is(err, boom) {
		t.Fatalf("retryable: %v", err)
	}
	if _, err := d.Decide(context.Background(), DecideInput{State: "s"}, qs); IsRetryable(err) || !errors.Is(err, boom) {
		t.Fatalf("fatal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := d.Decide(ctx, DecideInput{State: "s"}, qs); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	// 问题集超出能力：不消费脚本、不记录调用
	d = NewFakeDecider(DeciderStep{}).WithCaps(DeciderCaps{Kinds: []QuestionKind{KindNoul}})
	if _, err := d.Decide(context.Background(), DecideInput{State: "s"}, []Question{qLevel}); !errors.Is(err, ErrUnsupportedQuestion) {
		t.Fatalf("caps: %v", err)
	}
	if d.Left() != 1 || len(d.Calls()) != 0 {
		t.Fatal("rejected call consumed the script")
	}
	if _, err := d.Decide(context.Background(), DecideInput{State: make(chan int)}, qs); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("unencodable state accepted")
	}
}

// Fallback 只看 state 与问题，并发调用下结果确定。
func TestFakeDeciderFallbackConcurrent(t *testing.T) {
	d := NewFakeDecider().WithFallback(func(state json.RawMessage, q Question) (Answer, bool) {
		if q.Kind != KindNoul {
			return Answer{}, false
		}
		if strings.Contains(string(state), "ARiA") {
			return Answer{P: 0.95}, true
		}
		return Answer{P: 0.05}, true
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text, want := "今天好热", 0.05
			if i%2 == 0 {
				text, want = "ARiA 开空调", 0.95
			}
			got, err := d.Decide(context.Background(), DecideInput{State: map[string]string{"text": text}}, []Question{qAddressed, qRoute})
			if err != nil {
				t.Error(err)
				return
			}
			if got.Answers["addressed"].P != want || len(got.Missing) != 1 {
				t.Errorf("%q: %+v", text, got)
			}
		}(i)
	}
	wg.Wait()
	if len(d.Calls()) != 8 {
		t.Fatalf("calls %d", len(d.Calls()))
	}
}

func TestValidateInputImages(t *testing.T) {
	img := message.ImageBlock{Data: []byte{1}, MIME: "image/jpeg"}
	door, poster := NewImageRef(), NewImageRef()
	if door == poster || !strings.HasPrefix(door, "img-") || !validImageRef(door) {
		t.Fatalf("refs %q %q", door, poster)
	}
	qs := []Question{qAddressed}
	withRef := []Question{{Name: "same", Kind: KindNoul, Instructions: "`photo` 里的人和 " + poster + " 是同一个人吗"}}
	cases := []struct {
		name string
		in   DecideInput
		qs   []Question
		caps DeciderCaps
		want error
	}{
		{"text only ok", DecideInput{State: "s"}, qs, DeciderCaps{TextOnly: true}, nil},
		{"nil state", DecideInput{}, qs, DeciderCaps{}, ErrInvalidInput},
		{"image in state", DecideInput{State: map[string]string{"photo": door}, Images: map[string]message.ImageBlock{door: img}}, qs, DeciderCaps{}, nil},
		{"image in instructions", DecideInput{State: map[string]string{"photo": door},
			Images: map[string]message.ImageBlock{door: img, poster: img}}, withRef, DeciderCaps{}, nil},
		{"embedded in text", DecideInput{State: "门口：" + door + "（刚才）", Images: map[string]message.ImageBlock{door: img}}, qs, DeciderCaps{}, nil},
		{"unreferenced", DecideInput{State: "s", Images: map[string]message.ImageBlock{door: img}}, qs, DeciderCaps{}, ErrInvalidInput},
		{"no data", DecideInput{State: door, Images: map[string]message.ImageBlock{door: {URL: "http://x/a.jpg"}}}, qs, DeciderCaps{}, ErrInvalidInput},
		{"bad ref", DecideInput{State: "a b", Images: map[string]message.ImageBlock{"a b": img}}, qs, DeciderCaps{}, ErrInvalidInput},
		{"nested refs", DecideInput{State: "cat category", Images: map[string]message.ImageBlock{"cat": img, "category": img}}, qs, DeciderCaps{}, ErrInvalidInput},
		{"text-only model", DecideInput{State: door, Images: map[string]message.ImageBlock{door: img}}, qs, DeciderCaps{TextOnly: true}, ErrUnsupportedInput},
		{"bad question first", DecideInput{State: "s"}, nil, DeciderCaps{}, ErrInvalidQuestion},
	}
	for _, c := range cases {
		_, err := ValidateInput(c.in, c.qs, c.caps)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestFakeDeciderRecordsImages(t *testing.T) {
	a, b := "img-b", "img-a"
	d := NewFakeDecider(DeciderStep{Answers: map[string]Answer{"addressed": {P: 0.5}}})
	in := DecideInput{State: map[string]string{"x": a, "y": b},
		Images: map[string]message.ImageBlock{a: {Data: []byte{1}}, b: {Data: []byte{2}}}}
	got, err := d.Decide(context.Background(), in, []Question{qAddressed})
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Calls()[0]; strings.Join(c.Images, ",") != "img-a,img-b" {
		t.Fatalf("images %v", c.Images)
	}
	if got.Usage.In < 2*DefaultTokenRates.Image {
		t.Fatalf("usage %+v", got.Usage)
	}
}
