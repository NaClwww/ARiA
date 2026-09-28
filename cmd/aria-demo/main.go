// aria-demo 是 ARiA 的宿主薄壳（docs/README「产品入口」：事件流订阅者 +
// Scope 制造者）。它演示可嵌入引擎的最小完整通路：
//
//	stdin 一行 = 一句已成轮的话语（模拟 ASR 文字流的输出侧）
//	  → Session.Input（03 §6 ingress 成轮后调用的同一个入口）
//	  → 流式打印回答 / 工具调用
//	  → 可选 JSONL 落盘
//
// 输入侧「留好接口」：真正的 ingress（VAD/ASR/人脸 → 成轮状态机，03 §6）
// 落地后替换掉下面的行读取循环，Input 调用点不变。
//
// 用法：
//
//	aria-demo --fake                          # 无网络冒烟（内置演示 provider）
//	aria-demo --base-url ... --model ...      # 真实 OpenAI 兼容服务
//	aria-demo --record chat.jsonl ...         # durable 事件落盘
//
// 交互：[名字] 开头切换说话人（多人共享一个会话）；Ctrl+C 打断当前回答，
// 连按两次强制退出；/quit 或 Ctrl+D 正常退出。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	"aria/core/loop"
	"aria/core/provider"
	"aria/core/tool"
	"aria/internal/assemble"
	"aria/internal/config"
	"aria/pkg/ctxx"
	"aria/pkg/message"
	"aria/plugins/persist/jsonl"
	openai "aria/plugins/provider/openai"
	"aria/runtime/agent"
	"aria/runtime/persist"
	"aria/runtime/window"
)

const defaultSystemPrompt = "你是 ARiA，一个实时陪伴型桌面助手。回应简短自然，像身边的伙伴；" +
	"可以调用工具完成任务，不确定时先问一句。\n" + window.TagPolicyInstruction

func main() {
	var (
		fake         = flag.Bool("fake", false, "使用内置演示 provider（无网络，确定性输出）")
		configPath   = flag.String("config", "aria.toml", "配置文件路径（人写基准，启动时读一次）")
		overridePath = flag.String("override", "aria.override.toml", "覆盖文件路径（网页设置面板保存的目标）")
		baseURL      = flag.String("base-url", "", "覆盖 provider.base_url")
		model        = flag.String("model", "", "覆盖 provider.model")
		apiKey       = flag.String("api-key", "", "API key；空则按配置的 api_key_env 读环境变量")
		user         = flag.String("user", "", "覆盖 session.default_user")
		session      = flag.String("session", "", "覆盖 session.id")
		record       = flag.String("record", "", "覆盖 record.path（durable 事件 JSONL）")
		system       = flag.String("system", "", "覆盖 persona.system_prompt")
		printConfig  = flag.Bool("print-config", false, "打印生效配置后退出")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// 配置：启动读一次。下面的 flag 只作为进程内覆盖（local 层，不落盘、不改文件）。
	mgr, err := config.Load(*configPath, *overridePath)
	if err != nil {
		log.Error("config load failed", "err", err, "hint", "仓库根有 aria.toml 样例，--config 可指定路径")
		os.Exit(2)
	}
	mgr.SetLogf(func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "base-url":
			mgr.SetLocal("provider.base_url", *baseURL)
		case "model":
			mgr.SetLocal("provider.model", *model)
		case "user":
			mgr.SetLocal("session.default_user", *user)
		case "session":
			mgr.SetLocal("session.id", *session)
		case "record":
			mgr.SetLocal("record.path", *record)
		case "system":
			mgr.SetLocal("persona.system_prompt", *system)
		}
	})
	if *fake {
		// 演示 provider 不做真摘要：显式改成 keeplast（进程内覆盖，不改文件）。
		// 放在 printConfig 之前——打印出来的就是实际会用的。
		mgr.SetLocal("compress.strategy", "keeplast")
	}
	cfg := mgr.Effective()

	if *printConfig {
		printEffective(os.Stdout, *configPath, *overridePath, mgr)
		return
	}

	var (
		prov       provider.Provider
		compressor window.Compressor
	)
	if *fake {
		prov = newDemoProvider()
		log.Info("provider: fake（无网络；压缩策略已改为 keeplast 以保持确定性）")
	} else {
		key := *apiKey
		if key == "" && cfg.Provider.APIKeyEnv != "" {
			key = os.Getenv(cfg.Provider.APIKeyEnv)
		}
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" || cfg.Provider.Model == "" {
			fmt.Fprintf(os.Stderr, "真实模式需要模型与 API key：配置 provider.model + 环境变量 %s（或用 --model/--api-key/--fake）\n",
				orDefault(cfg.Provider.APIKeyEnv, "ARIA_API_KEY"))
			os.Exit(2)
		}
		prov = openai.New(openai.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, Model: cfg.Provider.Model})
		log.Info("provider: openai-compatible", "model", cfg.Provider.Model, "base_url", orDefault(cfg.Provider.BaseURL, "(官方默认)"))
	}

	compressor, err = assemble.Compressor(cfg, prov)
	if err != nil {
		log.Error("compress assemble failed", "err", err)
		os.Exit(2)
	}
	log.Info("compress", "strategy", cfg.Compress.Strategy)

	systemPrompt, err := resolvePersona(cfg.Persona, defaultSystemPrompt)
	if err != nil {
		log.Error("persona load failed", "err", err)
		os.Exit(2)
	}

	var store persist.Store
	if cfg.Record.Path != "" {
		s, err := jsonl.New(cfg.Record.Path)
		if err != nil {
			log.Error("record open failed", "err", err)
			os.Exit(1)
		}
		store = s
		log.Info("record", "path", cfg.Record.Path)
	}

	tools, err := buildTools(cfg.Tools.Builtin, log)
	if err != nil {
		log.Error("tools build failed", "err", err)
		os.Exit(2)
	}

	ag, err := agent.New(agent.Config{
		Provider:     prov,
		Tools:        tools,
		Compressor:   compressor,
		Store:        store,
		SystemPrompt: systemPrompt,
		MaxTurns:     cfg.Limits.MaxTurns,
		ToolTimeout:  time.Duration(cfg.Limits.ToolTimeoutMS) * time.Millisecond,
		Logger:       log,
	})
	if err != nil {
		log.Error("agent new failed", "err", err)
		os.Exit(1)
	}
	sess, err := ag.NewSession(ctxx.Scope{SessionID: cfg.Session.ID, UserID: cfg.Session.DefaultUser})
	if err != nil {
		log.Error("session new failed", "err", err)
		os.Exit(1)
	}

	// 事件流订阅：独立 goroutine 消费（Input 阻塞期间也要持续排空，
	// 否则订阅者停滞会被总线断开——durable/渲染承诺随之失效）。
	ch, unsub := sess.Subscribe(0)
	printerDone := make(chan struct{})
	go func() {
		defer close(printerDone)
		printEvents(ch, os.Stdout, os.Stderr)
	}()

	fmt.Fprintf(os.Stderr, "ARiA demo · 配置 %s · 模型 %s · 会话 %s · 说话人 %s\n",
		*configPath, modelName(*fake, cfg.Provider.Model), cfg.Session.ID, cfg.Session.DefaultUser)
	fmt.Fprintln(os.Stderr, "输入一句话回车发送；[名字] 开头切换说话人；Ctrl+C 打断；/quit 退出。")

	// Ctrl+C：第一次打断当前回答（steering），第二次强制退出。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\n（打断当前回答；再按一次强制退出）")
		sess.Interrupt()
		<-sig
		os.Exit(130)
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	exit := run(sess, lines, mgr, prov, os.Stderr)
	unsub()
	if err := sess.Close(); err != nil {
		log.Error("session close", "err", err)
	}
	<-printerDone
	if store != nil {
		if s, ok := store.(io.Closer); ok {
			s.Close()
		}
	}
	os.Exit(exit)
}

// run 是主循环：每行 = 一句已结算话语。这里就是未来 ingress 的接入缝——
// 成轮状态机（03 §6）产出的每条「有主人的输入」照同样方式调 Input。
//
// 每轮重新取一次生效配置：面板保存（写 override 层）或 Reload 之后，下一轮
// 就用上新模型/新参数——这是「改配置不必重启」在本壳里的体现。
func run(sess *agent.Session, lines <-chan string, mgr *config.Manager, prov provider.Provider, errw io.Writer) int {
	for line := range lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		if text == "/quit" || text == "/exit" {
			return 0
		}
		cfg := mgr.Effective()
		if text == "/reload" {
			if !mgr.Reload() {
				fmt.Fprintln(errw, "（配置文件无变化）")
				continue
			}
			cfg = mgr.Effective() // 重读后必须重新取快照：上面那份是 Reload 之前的
			// 策略可能变了：重新装配并热切换（在途压缩用旧策略跑完）。
			c, err := assemble.Compressor(cfg, prov)
			if err != nil {
				fmt.Fprintf(errw, "（压缩策略未变：%v）\n", err)
				continue
			}
			sess.SetCompressor(c)
			fmt.Fprintf(errw, "（已重读配置；模型 %s，压缩策略 %s）\n", cfg.Provider.Model, cfg.Compress.Strategy)
			continue
		}
		defUser := orDefault(cfg.Session.DefaultUser, "user")
		speaker, utterance := splitSpeaker(text, defUser)
		if utterance == "" {
			continue
		}
		if speaker != defUser {
			utterance = "[" + speaker + "] " + utterance
		}
		ctx := ctxx.WithScope(context.Background(), ctxx.Scope{UserID: speaker})
		temp := cfg.LLM.Temperature
		ctx = ctxx.WithOptions(ctx, ctxx.Options{
			Model: cfg.Provider.Model, Temperature: &temp, MaxTokens: cfg.LLM.MaxTokens,
		})
		fmt.Fprintf(errw, "%s> %s\n", speaker, utterance)
		if _, err := sess.Input(ctx, message.NewUser(utterance)); err != nil {
			fmt.Fprintf(errw, "！输入失败：%v\n", err)
			return 1
		}
	}
	return 0
}

// resolvePersona 取人设：文件优先，其次内联文本，都为空用内置默认。
func resolvePersona(p config.Persona, fallback string) (string, error) {
	if p.SystemPromptFile != "" {
		b, err := os.ReadFile(p.SystemPromptFile)
		if err != nil {
			return "", fmt.Errorf("读 persona 文件 %s: %w", p.SystemPromptFile, err)
		}
		return string(b), nil
	}
	if p.SystemPrompt != "" {
		return p.SystemPrompt, nil
	}
	return fallback, nil
}

// buildTools 按配置里的名字装配演示工具；未知名字报错（不静默少工具）。
func buildTools(names []string, log *slog.Logger) ([]tool.Tool, error) {
	var tools []tool.Tool
	for _, name := range names {
		switch name {
		case "now":
			tools = append(tools, makeNowTool())
		case "lorem":
			tools = append(tools, makeLoremTool())
		default:
			return nil, fmt.Errorf("未知内置工具 %q（可选：now, lorem）", name)
		}
	}
	log.Info("tools", "count", len(tools))
	return tools, nil
}

// printEffective 打印生效配置（含各层的覆盖关系），排查「改了为什么没生效」用。
func printEffective(w io.Writer, configPath, overridePath string, mgr *config.Manager) {
	eff, base := mgr.Effective(), mgr.Base()
	fmt.Fprintf(w, "配置文件   %s（基准）\n", configPath)
	fmt.Fprintf(w, "覆盖文件   %s\n", overridePath)
	fmt.Fprintf(w, "模型       %s", eff.Provider.Model)
	if eff.Provider.Model != base.Provider.Model {
		fmt.Fprintf(w, "   [覆盖：基准 %q]", base.Provider.Model)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "服务地址   %s\n", orDefault(eff.Provider.BaseURL, "(官方默认)"))
	fmt.Fprintf(w, "key 环境变量 %s\n", eff.Provider.APIKeyEnv)
	fmt.Fprintf(w, "人设       %s\n", personaSource(eff.Persona))
	fmt.Fprintf(w, "温度/上限   %v / %d\n", eff.LLM.Temperature, eff.LLM.MaxTokens)
	fmt.Fprintf(w, "压缩       strategy=%s keep_last_n=%d model=%q instruction=%s\n",
		eff.Compress.Strategy, eff.Compress.KeepLastN, eff.Compress.Model,
		orDefault(eff.Compress.Instruction, "(默认提示词)"))
	fmt.Fprintf(w, "工具       %v\n", eff.Tools.Builtin)
	fmt.Fprintf(w, "落盘       %s\n", orDefault(eff.Record.Path, "(关闭)"))
	fmt.Fprintf(w, "会话/说话人 %s / %s\n", eff.Session.ID, eff.Session.DefaultUser)
	fmt.Fprintf(w, "上限       max_turns=%d tool_timeout_ms=%d\n", eff.Limits.MaxTurns, eff.Limits.ToolTimeoutMS)
}

func personaSource(p config.Persona) string {
	switch {
	case p.SystemPromptFile != "":
		return "文件 " + p.SystemPromptFile
	case p.SystemPrompt != "":
		return fmt.Sprintf("内联（%d 字符）", len([]rune(p.SystemPrompt)))
	default:
		return "(内置默认)"
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// splitSpeaker 解析「[名字] 内容」的说话人前缀；无前缀用默认说话人。
func splitSpeaker(line, def string) (string, string) {
	if strings.HasPrefix(line, "[") {
		if i := strings.Index(line, "]"); i > 0 && i <= 24 {
			name, rest := strings.TrimSpace(line[1:i]), strings.TrimSpace(line[i+1:])
			if name != "" {
				return name, rest
			}
		}
	}
	return def, line
}

// printEvents 把事件流渲染到终端：回答增量进 stdout（保持可管道化），
// 提示与工具轨迹进 stderr。
func printEvents(ch <-chan loop.Event, out, errw io.Writer) {
	for ev := range ch {
		switch d := ev.Data.(type) {
		case loop.MessageUpdateData:
			fmt.Fprint(out, d.TextDelta)
		case loop.MessageEndData:
			if d.Message.Role == message.RoleAssistant && d.Message.Text() != "" {
				fmt.Fprintln(out)
			}
		case loop.ToolExecStartData:
			fmt.Fprintf(errw, "  · 工具 %s(%s)\n", d.Call.Name, truncStr(string(d.Call.Args), 60))
		case loop.ToolExecEndData:
			if d.Denied {
				fmt.Fprintf(errw, "  · 已拒绝 %s\n", d.Call.Name)
			} else if d.Result.IsError {
				fmt.Fprintf(errw, "  · 失败 %s：%s\n", d.Call.Name, firstLine(d.Result))
			}
		}
	}
}

func firstLine(r message.ToolResult) string {
	text := strings.TrimSpace(r.ToMessage().Text())
	if i := strings.IndexByte(text, '\n'); i > 0 {
		text = text[:i]
	}
	return truncStr(text, 80)
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func modelName(fake bool, m string) string {
	if fake {
		return "demo-fake"
	}
	return m
}

// ---------- 演示工具 ----------

type simpleTool struct {
	name, desc string
	fn         func(ctx context.Context, args json.RawMessage) string
}

func (t simpleTool) Def() tool.Def {
	return tool.Def{Name: t.name, Description: t.desc}
}

func (t simpleTool) Exec(ctx context.Context, call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID, Blocks: []message.Block{message.TextBlock{Text: t.fn(ctx, call.Args)}}}
}

func makeNowTool() simpleTool {
	return simpleTool{
		name: "now",
		desc: "查询当前时间",
		fn: func(_ context.Context, _ json.RawMessage) string {
			return "现在是 " + time.Now().Format("2006-01-02 15:04:05 MST Mon")
		},
	}
}

func makeLoremTool() simpleTool {
	return simpleTool{
		name: "lorem",
		desc: "生成长文本（演示超长工具结果的截断与 artifact 取回）；参数 n=字符数，默认 8000，上限 200000",
		fn: func(_ context.Context, args json.RawMessage) string {
			var in struct{ N int }
			_ = json.Unmarshal(args, &in)
			if in.N <= 0 {
				in.N = 8000
			}
			if in.N > 200000 {
				in.N = 200000
			}
			unit := []rune("敏捷的棕色狐狸跳过了懒惰的狗。")
			var b []rune
			for len(b) < in.N {
				b = append(b, unit...)
			}
			return string(b[:in.N])
		},
	}
}

// ---------- fake 模式：内置演示 provider ----------

// demoProvider 永不耗尽的确定性 provider：问到时间先调 now 工具；
// 拿到工具结果后作答；其余情况复读用户话语。流式分片输出。
type demoProvider struct{ toolCalled atomic.Bool }

func newDemoProvider() *demoProvider { return &demoProvider{} }

func (p *demoProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.StreamEvent, error) {
	// 只看最后一条非 system 消息：工具轮的收尾是 tool 消息；
	// 之后的新输入以 user 消息收尾——不能用「历史里出现过 tool」判断。
	input, lastIsTool := "", false
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == message.RoleSystem {
			continue
		}
		if m.Role == message.RoleUser {
			input = m.Text()
		}
		lastIsTool = m.Role == message.RoleTool
		break
	}

	var reply string
	var calls []message.ToolCall
	switch {
	case lastIsTool:
		reply = "查到了，工具结果已在上面。"
	case !p.toolCalled.Load() && (strings.Contains(input, "时间") || strings.Contains(input, "几点")):
		p.toolCalled.Store(true)
		calls = append(calls, message.ToolCall{ID: "call-demo-1", Name: "now", Args: json.RawMessage("{}")})
	case strings.Contains(input, "长文本"):
		calls = append(calls, message.ToolCall{ID: "call-demo-2", Name: "lorem", Args: json.RawMessage(`{"n":50000}`)})
	default:
		reply = "（演示应答）你说：" + input
	}

	ch := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(ch)
		if len(calls) > 0 {
			ch <- provider.MessageComplete{Message: message.Message{
				Role: message.RoleAssistant, ToolCalls: calls,
			}}
			return
		}
		runes := []rune(reply)
		step := len(runes)/4 + 1
		for i := 0; i < len(runes); i += step {
			end := min(i+step, len(runes)) // 切片上界必须钳制：cap 内越界不 panic、静默读出零值 rune
			ch <- provider.PartDelta{Text: string(runes[i:end])}
		}
		ch <- provider.MessageComplete{Message: message.Message{
			Role:   message.RoleAssistant,
			Blocks: []message.Block{message.TextBlock{Text: reply}},
		}}
	}()
	return ch, nil
}
