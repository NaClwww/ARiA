package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aria/core/provider"
)

var (
	qAddressed = provider.Question{Name: "addressed", Kind: provider.KindNoul, Instructions: "`items[0]` 是在对 ARiA 说话"}
	qRoute     = provider.Question{Name: "route", Kind: provider.KindChoice, Instructions: "该交给谁",
		Criteria: []provider.Criterion{{Name: "aria", Description: "ARiA 能处理"}, {Name: "human"}}}
	qLevel = provider.Question{Name: "level", Kind: provider.KindScore, Instructions: "有多重要",
		Criteria: []provider.Criterion{{Name: "低", Description: "可以不管"}, {Name: "中"}, {Name: "高"}, {Name: "紧急", Description: "立刻处理"}}}
	allQs = []provider.Question{qAddressed, qRoute, qLevel}
)

func TestQuestionsEncoding(t *testing.T) {
	b, err := json.Marshal(Questions(allQs))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if n := got["addressed"]; n["type"] != "noul" || n["instructions"] != qAddressed.Instructions {
		t.Fatalf("noul %v", n)
	} else if _, ok := n["criteria"]; ok {
		t.Fatalf("noul without described criteria sent criteria: %v", n)
	}
	choice := got["route"]["criteria"].(map[string]any)
	if v, ok := choice["human"]; !ok || v != nil || choice["aria"] != "ARiA 能处理" {
		t.Fatalf("choice criteria %v (option without description must be null)", choice)
	}
	levels := got["level"]["criteria"].([]any)
	if len(levels) != 4 || levels[0] != "可以不管" || levels[1] != "中" || levels[3] != "立刻处理" {
		t.Fatalf("score criteria %v", levels)
	}

	// noul：只描述了一边时另一边用名字补齐（协议要求成对）
	q := provider.Question{Name: "x", Kind: provider.KindNoul, Criteria: []provider.Criterion{{Name: "true", Description: "在叫她"}, {Name: "false"}}}
	if c := criteria(q).(map[string]string); c["true"] != "在叫她" || c["false"] != "false" {
		t.Fatalf("noul criteria %v", c)
	}
}

func raw(s string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

// 形状取自公开文档：noul 无 confidence；score 的分布按下标字符串作键、乱序，并带 legend。
func TestAnswersDecode(t *testing.T) {
	got, missing := Answers(allQs, raw(`{
	  "addressed": {"type": "noul", "noul": 0.96},
	  "route": {"type": "choice", "choice": "aria", "probabilities": {"human": 0.22, "aria": 0.78}, "confidence": 0.67},
	  "level": {"type": "score", "score": 2.38, "legend": {"0": "可以不管", "1": "中", "2": "高", "3": "立刻处理"},
	            "probabilities": {"3": 0.4, "0": 0, "2": 0.58, "1": 0.02}, "confidence": 0.58}
	}`))
	if len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if n := got["addressed"]; n.P != 0.96 || math.Abs(n.Confidence-0.92) > 1e-9 {
		t.Fatalf("noul %+v", n)
	}
	// confidence 由规范层重算：(0.78-0.5)/0.5 = 0.56，而非厂商的 0.67
	if c := got["route"]; c.Choice != "aria" || math.Abs(c.Confidence-0.56) > 1e-9 {
		t.Fatalf("choice %+v", c)
	}
	if s := got["level"]; s.Choice != "高" || s.Probs["紧急"] != 0.4 || math.Abs(s.Score-2.38) > 1e-9 || math.Abs(s.Confidence-0.58) > 1e-9 {
		t.Fatalf("score %+v", s)
	}
}

func TestAnswersMissingAndMalformed(t *testing.T) {
	got, missing := Answers(allQs, raw(`{
	  "addressed": {"type": "choice", "choice": "aria"},
	  "level": {"type": "score", "score": 1, "probabilities": {"7": 1}},
	  "extra": {"type": "box", "box": {"top": 0, "bottom": 1, "left": 0, "right": 1}}
	}`))
	// 类型不符、漏答、下标越界 → missing（按提问顺序）；没被问到的 box 忽略
	if strings.Join(missing, ",") != "addressed,route,level" || len(got) != 0 {
		t.Fatalf("answers %v missing %v", got, missing)
	}
	_, missing = Answers([]provider.Question{qAddressed}, raw(`{"addressed": {"type": "noul"}}`))
	if len(missing) != 1 {
		t.Fatal("noul without value accepted")
	}
}

// 厂商省略分布（协议里 probabilities 可选）时退回其 choice / score 与自报 confidence。
func TestAnswersWithoutProbabilities(t *testing.T) {
	got, missing := Answers([]provider.Question{qRoute, qLevel}, raw(`{
	  "route": {"type": "choice", "choice": "human", "confidence": 0.7},
	  "level": {"type": "score", "score": 0.6}
	}`))
	if len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if c := got["route"]; c.Choice != "human" || c.Confidence != 0.7 || c.Probs != nil {
		t.Fatalf("choice %+v", c)
	}
	if s := got["level"]; s.Choice != "中" || s.Score != 0.6 {
		t.Fatalf("score %+v", s)
	}
}

func call(url string) Call {
	return Call{
		Endpoint:  "test",
		URL:       url,
		Header:    http.Header{"Authorization": {"Bearer k"}},
		Body:      map[string]string{"a": "b"},
		ErrorText: func(b []byte) string { return "" },
		RequestID: func(h http.Header) string { return h.Get("x-req") },
	}
}

func TestPostSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer k" ||
			r.Header.Get("Content-Type") != "application/json" || string(b) != `{"a":"b"}` {
			t.Errorf("request %s %v %s", r.Method, r.Header, b)
		}
		w.Header().Set("x-req", "r1")
		io.WriteString(w, `{"ok": true}`)
	}))
	defer srv.Close()
	var out struct{ OK bool }
	h, err := Post(context.Background(), srv.Client(), call(srv.URL), &out)
	if err != nil || !out.OK || h.Get("x-req") != "r1" {
		t.Fatalf("out %+v err %v", out, err)
	}
}

func TestPostErrors(t *testing.T) {
	cases := []struct {
		status    int
		header    map[string]string
		body      string
		retryable bool
		msg       string
		after     time.Duration
	}{
		{429, map[string]string{"retry-after-ms": "1500"}, ``, true, "(无响应体)", 1500 * time.Millisecond},
		{503, map[string]string{"retry-after": "2"}, `oops`, true, "oops", 2 * time.Second},
		{408, nil, ``, true, "(无响应体)", 0},
		{400, nil, strings.Repeat("长", 300), false, strings.Repeat("长", 200) + "…", 0},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range c.header {
				w.Header().Set(k, v)
			}
			w.Header().Set("x-req", "r2")
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		_, err := Post(context.Background(), srv.Client(), call(srv.URL), &struct{}{})
		srv.Close()
		var e *APIError
		if !errors.As(err, &e) || e.Status != c.status || e.Message != c.msg || e.RetryAfter != c.after || e.RequestID != "r2" {
			t.Errorf("%d: err %v (%+v)", c.status, err, e)
			continue
		}
		if provider.IsRetryable(err) != c.retryable {
			t.Errorf("%d: retryable %v", c.status, provider.IsRetryable(err))
		}
	}
}

func TestPostConnectionAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := Post(context.Background(), http.DefaultClient, call(url), &struct{}{}); !provider.IsRetryable(err) {
		t.Fatalf("connection refused: %v", err)
	}

	block := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Post(ctx, srv.Client(), call(srv.URL), &struct{}{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}
