package basic

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"aria/core/tool"
	"aria/pkg/message"
)

func runTool(t *testing.T, tl textTool, args string) (string, bool) {
	t.Helper()
	res := tl.Exec(context.Background(), tool.Call{ID: "t1", Args: json.RawMessage(args)})
	var b strings.Builder
	for _, blk := range res.Blocks {
		if tb, ok := blk.(message.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String(), res.IsError
}

func TestCalc(t *testing.T) {
	tl := calcTool()
	cases := []struct {
		in, want string
		isErr    bool
	}{
		{"1+2*3", "1+2*3 = 7", false},
		{"(4-1)/0.5", "(4-1)/0.5 = 6", false},
		{"0.1+0.2", "0.1+0.2 = 0.3", false},
		{"-3*4", "-3*4 = -12", false},
		{"10 % 3", "10 % 3 = 1", false},
		{"1/0", "", true},
		{"1+", "", true},
		{"(1+2", "", true},
		{"1 2", "", true},
	}
	for _, c := range cases {
		got, isErr := runTool(t, tl, `{"expression":"`+c.in+`"}`)
		if isErr != c.isErr {
			t.Errorf("calc(%q) isErr = %v, want %v（结果 %q）", c.in, isErr, c.isErr, got)
			continue
		}
		if !c.isErr && got != c.want {
			t.Errorf("calc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRandomBounded(t *testing.T) {
	tl := randomTool()
	for range 200 {
		got, isErr := runTool(t, tl, `{"min":5,"max":7,"count":3}`)
		if isErr {
			t.Fatalf("random 意外报错: %s", got)
		}
		parts := strings.Split(got, "、")
		if len(parts) != 3 {
			t.Fatalf("random count=3 得到 %q", got)
		}
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 5 || n > 7 {
				t.Fatalf("random 越界: %q", got)
			}
		}
	}
	if _, isErr := runTool(t, tl, `{"min":9,"max":3}`); !isErr {
		t.Error("min>max 应报错")
	}
	// 区间宽度达到 int64 上限：返回错误结果，不得 panic。
	if _, isErr := runTool(t, tl, `{"min":0,"max":9223372036854775807}`); !isErr {
		t.Error("max-min 达到 MaxInt64 应报错")
	}
	if _, isErr := runTool(t, tl, `{"min":-9223372036854775808,"max":9223372036854775807}`); !isErr {
		t.Error("全 int64 区间应报错")
	}
	// 跨零的大区间（宽度小于 MaxInt64）正常取值。
	got, isErr := runTool(t, tl, `{"min":-2305843009213693952,"max":2305843009213693952}`)
	if isErr {
		t.Fatalf("宽度小于 MaxInt64 的区间不应报错: %s", got)
	}
	if n, err := strconv.ParseInt(got, 10, 64); err != nil || n < -2305843009213693952 || n > 2305843009213693952 {
		t.Fatalf("random 越界: %q", got)
	}
}

func TestNow(t *testing.T) {
	got, isErr := runTool(t, nowTool(), ``)
	if isErr || !strings.Contains(got, "\nunix: ") {
		t.Errorf("now 输出缺人类可读行或 unix 行: %q (isErr=%v)", got, isErr)
	}
}

func TestWeatherErrorFedBack(t *testing.T) {
	// 假传输层：永远 500。验证失败以 IsError + 可读文案喂回，不 panic 不编数据。
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Status: "500 Internal Server Error",
			Body: http.NoBody}, nil
	})
	tl := weatherTool(&http.Client{Transport: rt})
	got, isErr := runTool(t, tl, `{"location":"杭州"}`)
	if !isErr || !strings.Contains(got, "500") {
		t.Errorf("weather 失败应 IsError 且带状态码，得到 %q (isErr=%v)", got, isErr)
	}
	if _, isErr := runTool(t, tl, `{}`); !isErr {
		t.Error("缺 location 应报错")
	}
}

func TestBash(t *testing.T) {
	tl := bashTool()
	if got, isErr := runTool(t, tl, `{"command":"echo hello"}`); isErr || got != "hello" {
		t.Errorf("bash echo = %q (isErr=%v)", got, isErr)
	}
	// 退出码非 0：IsError + stderr 在场，模型据此改命令重试。
	if got, isErr := runTool(t, tl, `{"command":"echo boom >&2; exit 3"}`); !isErr || !strings.Contains(got, "退出码 3") || !strings.Contains(got, "boom") {
		t.Errorf("bash exit 3 = %q (isErr=%v)", got, isErr)
	}
	// 超时上限：sleep 远超上限应被打断，而不是真等 30s。
	if got, isErr := runTool(t, tl, `{"command":"sleep 60","timeout_ms":300}`); !isErr {
		t.Errorf("bash 超时应 IsError，得到 %q", got)
	}
	// 超时击杀必须干脆：ctx 只杀 bash 本体时，后台子进程（curl/sleep）会
	// 孤儿化握住输出管道，CombinedOutput 拖满 WaitDelay 才返回（实测 +2s/次
	// 且进程泄漏）。进程组击杀应在超时点附近立即返回。
	start := time.Now()
	if _, isErr := runTool(t, tl, `{"command":"sleep 30 & wait","timeout_ms":300}`); !isErr {
		t.Error("带后台子进程的超时应 IsError")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("超时击杀拖尾 %v：孤儿进程握管道，WaitDelay 被吃满", elapsed)
	}
	// 管道与子 shell 语义（bash -c）；printf 格式串必须加引号，否则 bash
	// 先把 \n 吃成字面 n。
	if got, isErr := runTool(t, tl, `{"command":"printf 'b\\na\\nc' | sort | head -2"}`); isErr || got != "a\nb" {
		t.Errorf("bash 管道 = %q (isErr=%v)", got, isErr)
	}
}

func TestAllRegistry(t *testing.T) {
	for _, name := range []string{"now", "calc", "random", "weather", "bash"} {
		if tl, ok := All()[name]; !ok || tl.Def().Name != name {
			t.Errorf("注册表缺少 %q", name)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
