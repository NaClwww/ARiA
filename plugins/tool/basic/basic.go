// Package basic 是内置通用工具集（docs/03 §1：工具是插件层能力；引擎不认识
// 任何具体工具，宿主按配置 [tools] builtin 的名字从这里的注册表挑选装配）：
//
//	now     —— 当前时间/日期/星期（语音助手最高频的基础事实）
//	calc    —— 四则运算（本地求值，无网络无依赖）
//	random  —— 随机整数/掷骰
//	weather —— wttr.in 天气简报（免 key，超时短退，失败以 IsError 喂回）
//	bash    —— 本机 shell 执行（stdout/stderr 合并回喂 + 退出码；以宿主身份
//	           跑在宿主机上，模型能做什么 = 宿主账号能做什么，是否启用由配置决定）
//
// 全部与宿主/设备解耦：不碰 launcher、不碰 backend，纯本地或公网只读。
// aria-demo 与 aria-host 共用本注册表；lorem 之类纯演示工具留在各自的宿主里。
package basic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aria/core/tool"
	"aria/pkg/message"
)

// All 返回名字 → 工具的注册表；宿主装配的唯一入口。
func All() map[string]tool.Tool {
	return map[string]tool.Tool{
		"now":     nowTool(),
		"calc":    calcTool(),
		"random":  randomTool(),
		"weather": weatherTool(nil),
		"bash":    bashTool(),
	}
}

// textTool 是单文本结果工具的骨架：Def 固定、args 空参容忍为 {}、错误包装
// 成 IsError 结果喂回模型自行纠偏——具体工具只提供 fn。
type textTool struct {
	name, desc string
	params     json.RawMessage
	fn         func(ctx context.Context, args json.RawMessage) (string, error)
}

func (t textTool) Def() tool.Def {
	return tool.Def{Name: t.name, Description: t.desc, Parameters: t.params}
}

func (t textTool) Exec(ctx context.Context, call tool.Call) tool.Result {
	args := json.RawMessage(call.Args)
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage("{}")
	}
	text, err := t.fn(ctx, args)
	if err != nil {
		return tool.Result{CallID: call.ID, IsError: true,
			Blocks: []message.Block{message.TextBlock{Text: err.Error()}}}
	}
	return tool.Result{CallID: call.ID,
		Blocks: []message.Block{message.TextBlock{Text: text}}}
}

// ---------- now ----------

// cnWeekday 是星期几的中文字（time.Weekday 从周日开始）。
var cnWeekday = [...]string{"日", "一", "二", "三", "四", "五", "六"}

func nowTool() textTool {
	return textTool{
		// now 是「现在几点」工具：模型没有时钟，任何涉及当下时刻的问题
		// （现在几点/今天几号/刚才过了多久）都该先调它，而不是凭语料猜。
		// 纯本地取时，无参数。输出两行：人类可读行（含星期与时区缩写）+
		// unix 时间戳（模型做「三小时后是几点」之类的时间算术用）。
		name: "now",
		desc: "查询当前本地时间（日期、星期、时区、Unix 时间戳）。凡涉及「现在/今天/明天/刚才/待会儿」的回答都先调用本工具，不要凭记忆推测当前时间。",
		fn: func(context.Context, json.RawMessage) (string, error) {
			now := time.Now()
			zone, _ := now.Zone()
			text := now.Format("2006-01-02 15:04:05 -07:00") + " 周" + cnWeekday[now.Weekday()]
			if zone != "" {
				text += " " + zone
			}
			return text + "\nunix: " + strconv.FormatInt(now.Unix(), 10), nil
		},
	}
}

// ---------- calc ----------

func calcTool() textTool {
	return textTool{
		name: "calc",
		desc: "四则运算计算器：+ - * / %、括号、一元负号；参数 expression=算式字符串",
		params: json.RawMessage(`{"type":"object","properties":{` +
			`"expression":{"type":"string","description":"算式，如 1+2*3 或 (4-1)/0.5"}},` +
			`"required":["expression"]}`),
		fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ Expression string }
			if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Expression) == "" {
				return "", fmt.Errorf("calc: 缺少 expression 参数")
			}
			v, err := evalCalc(in.Expression)
			if err != nil {
				return "", fmt.Errorf("calc: %w", err)
			}
			return in.Expression + " = " + v, nil
		},
	}
}

// evalCalc 用 big.Rat 做精确有理数求值（0.1+0.2 不漂），最后按整数/小数
// 转写展示；递归下降：expr := term (('+'|'-') term)*，term := unary
// (('*'|'/'|'%') unary)*，unary := ['-'] primary，primary := 数 | '(' expr ')'。
func evalCalc(s string) (string, error) {
	p := &calcParser{src: []rune(s)}
	p.skipSpace()
	v, err := p.expr()
	if err != nil {
		return "", err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return "", fmt.Errorf("算式有多余内容 %q", string(p.src[p.pos:]))
	}
	if !v.IsInt() {
		f, _ := v.Float64()
		if math.IsInf(f, 0) {
			return "", fmt.Errorf("结果超出可表示范围")
		}
		return trimFloat(f), nil
	}
	return v.RatString(), nil
}

func trimFloat(f float64) string {
	// %.10f 再去尾零：避免 %g 的科学计数法与精度尾巴。
	s := strings.TrimRight(fmt.Sprintf("%.10f", f), "0")
	return strings.TrimSuffix(s, ".")
}

type calcParser struct {
	src []rune
	pos int
}

// ratMod 是截断除法的取模（C 风格，符号随被除数）：r = a - trunc(a/b)*b。
func ratMod(a, b *big.Rat) *big.Rat {
	// a/b = n/d（整数比），big.Int.Quo 截断取整即 trunc(a/b)。
	n := new(big.Int).Mul(a.Num(), b.Denom())
	d := new(big.Int).Mul(a.Denom(), b.Num())
	q := new(big.Rat).SetFrac(new(big.Int).Quo(n, d), big.NewInt(1))
	return new(big.Rat).Sub(a, q.Mul(q, b))
}

func (p *calcParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *calcParser) expr() (*big.Rat, error) {
	v, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return v, nil
		}
		switch p.src[p.pos] {
		case '+':
			p.pos++
			w, err := p.term()
			if err != nil {
				return nil, err
			}
			v = v.Add(v, w)
		case '-':
			p.pos++
			w, err := p.term()
			if err != nil {
				return nil, err
			}
			v = v.Sub(v, w)
		default:
			return v, nil
		}
	}
}

func (p *calcParser) term() (*big.Rat, error) {
	v, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return v, nil
		}
		var op func(a, b *big.Rat) *big.Rat
		switch p.src[p.pos] {
		case '*':
			op = func(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }
		case '/':
			op = func(a, b *big.Rat) *big.Rat { return new(big.Rat).Quo(a, b) }
		case '%':
			op = ratMod
		default:
			return v, nil
		}
		p.pos++
		w, err := p.unary()
		if err != nil {
			return nil, err
		}
		if w.Sign() == 0 {
			return nil, fmt.Errorf("除数为零")
		}
		v = op(v, w)
	}
}

func (p *calcParser) unary() (*big.Rat, error) {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		return v.Neg(v), nil
	}
	return p.primary()
}

func (p *calcParser) primary() (*big.Rat, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("算式不完整")
	}
	if p.src[p.pos] == '(' {
		p.pos++
		v, err := p.expr()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return nil, fmt.Errorf("缺少右括号")
		}
		p.pos++
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return nil, fmt.Errorf("无法识别的字符 %q", string(p.src[p.pos]))
	}
	v, ok := new(big.Rat).SetString(string(p.src[start:p.pos]))
	if !ok {
		return nil, fmt.Errorf("无法解析的数字 %q", string(p.src[start:p.pos]))
	}
	return v, nil
}

// ---------- random ----------

func randomTool() textTool {
	return textTool{
		name: "random",
		desc: "随机整数（掷骰/抽签）：参数 min、max 为闭区间（默认 1..100），count 为个数（默认 1，上限 10）",
		params: json.RawMessage(`{"type":"object","properties":{` +
			`"min":{"type":"integer","description":"下界，默认 1"},` +
			`"max":{"type":"integer","description":"上界，默认 100"},` +
			`"count":{"type":"integer","description":"个数，默认 1，上限 10"}}}`),
		fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ Min, Max, Count int }
			_ = json.Unmarshal(args, &in)
			if in.Min == 0 && in.Max == 0 {
				in.Min, in.Max = 1, 100
			}
			if in.Min > in.Max {
				return "", fmt.Errorf("random: min(%d) 大于 max(%d)", in.Min, in.Max)
			}
			// 区间宽度按 uint64 计算（Min≤Max 时补码相减即宽度），Max-Min+1 在 int 下可溢出为 0 或负数，
			// 使 rand.Intn panic；宽度达到 MaxInt64 时 Int63n 的参数无法表示，返回错误结果。
			span := uint64(in.Max) - uint64(in.Min)
			if span >= math.MaxInt64 {
				return "", fmt.Errorf("random: 区间过大（max-min 须小于 %d）", int64(math.MaxInt64))
			}
			if in.Count <= 0 {
				in.Count = 1
			}
			if in.Count > 10 {
				in.Count = 10
			}
			parts := make([]string, in.Count)
			for i := range parts {
				parts[i] = fmt.Sprint(int64(in.Min) + rand.Int63n(int64(span)+1))
			}
			return strings.Join(parts, "、"), nil
		},
	}
}

// ---------- weather ----------

// weatherTool 走 wttr.in 文本简报：免 key、一行结果适合语音播报。client 注入
// 是为了测试替换；失败按错误结果喂回——天气是外部事实，宁可承认查不到，
// 不编一个。
func weatherTool(client *http.Client) textTool {
	return textTool{
		name: "weather",
		desc: "查询城市当前天气简报；参数 location=城市名（支持中文/拼音，如「杭州」或 \"Hangzhou\"）",
		params: json.RawMessage(`{"type":"object","properties":{` +
			`"location":{"type":"string","description":"城市名"}},` +
			`"required":["location"]}`),
		fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct{ Location string }
			if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Location) == "" {
				return "", fmt.Errorf("weather: 缺少 location 参数")
			}
			if client == nil {
				client = &http.Client{Timeout: 5 * time.Second}
			}
			u := "https://wttr.in/" + url.PathEscape(in.Location) +
				"?format=" + url.QueryEscape(weatherFormat) + "&lang=zh"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return "", fmt.Errorf("weather: %w", err)
			}
			// wttr.in 按 UA 决定给彩色 ANSI 还是纯文本；curl 才是纯文本。
			req.Header.Set("User-Agent", "curl/8.0")
			resp, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("weather: 查询 %q 失败：%w", in.Location, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
				return "", fmt.Errorf("weather: 查询 %q 返回 %s（城市名可能无法识别）", in.Location, resp.Status)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			if err != nil {
				return "", fmt.Errorf("weather: 读取响应失败：%w", err)
			}
			return strings.TrimSpace(string(body)), nil
		},
	}
}

const weatherFormat = "%l：%c %t（体感 %f）湿度 %h 风 %w"

// ---------- bash ----------

const (
	bashDefaultTimeoutMS = 10_000  // 单命令默认上限；够 ls/cat/curl 一类
	bashMaxTimeoutMS     = 30_000  // 与 [limits] tool_timeout_ms 的基准值对齐
	bashMaxOutput        = 8 << 10 // 输出截断 8KiB：agent 侧还有 artifact 兜底，先少喂
)

func bashTool() textTool {
	return textTool{
		// 简易 bash：命令以宿主身份在宿主机上执行（工作目录 = 进程 cwd），
		// 无白名单无沙箱——启不启用交给配置（不进默认 builtin 就没有）。
		// stdout/stderr 合并回喂（模型看报错靠 stderr），退出码非 0 以
		// IsError 结果给出：一次失败喂回去，模型自己改命令重试。
		name: "bash",
		desc: "在本机执行 shell 命令并返回输出。参数 command=命令字符串（bash -c 语义，可用管道/重定向），timeout_ms=超时毫秒（默认 10000，上限 30000）。用于查文件、跑脚本、调系统命令等。",
		params: json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"string","description":"要执行的命令"},` +
			`"timeout_ms":{"type":"integer","description":"超时毫秒，默认 10000，上限 30000"}},` +
			`"required":["command"]}`),
		fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Command   string `json:"command"`
				TimeoutMS int    `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Command) == "" {
				return "", fmt.Errorf("bash: 缺少 command 参数")
			}
			timeout := time.Duration(in.TimeoutMS) * time.Millisecond
			if timeout <= 0 {
				timeout = bashDefaultTimeoutMS * time.Millisecond
			}
			if timeout > bashMaxTimeoutMS*time.Millisecond {
				timeout = bashMaxTimeoutMS * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", in.Command)
			// 独立进程组 + 超时击杀整组：ctx 只杀 bash 本体时，它 fork 的
			// curl/sleep 会孤儿化并握着输出管道——CombinedOutput 得等满
			// WaitDelay 才返回（实测每次超时多付 2s，且进程泄漏）。杀组则
			// 管道立刻 EOF、子进程一并了断。
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if cmd.Process != nil {
					return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				return nil
			}
			// WaitDelay 只兜「正常退出后仍有孤儿写管道」的尾（如命令自带
			// 后台任务）；1s 足够排空缓冲，不陪孤儿等。
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			text := string(out)
			if len(text) > bashMaxOutput {
				text = text[:bashMaxOutput] + "\n…（输出超长，已截断）"
			}
			text = strings.TrimRight(text, "\n")
			if text == "" {
				text = "（无输出）"
			}
			if err == nil {
				return text, nil
			}
			// 判断顺序：先 ctx（CommandContext 超时杀进程也表现为 ExitError
			// signal，只有 ctx.Err() 才能区分「超时被打断」与「自己失败退出」）。
			if ctx.Err() != nil {
				return "", fmt.Errorf("bash: 超时或被取消（%v）：%s", ctx.Err(), text)
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", fmt.Errorf("退出码 %d：%s", exit.ExitCode(), text)
			}
			return "", fmt.Errorf("bash: 启动失败：%w", err)
		},
	}
}
