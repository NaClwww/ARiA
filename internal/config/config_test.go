package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeBase(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "aria.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const baseBody = `# 人写基准：注释必须永远保留
[server]
listen = "127.0.0.1:9999"

[provider]
base_url = "https://api.deepseek.com/v1"
model = "deepseek-chat"

[llm]
temperature = 0.5
`

func TestLoadMergePrecedence(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	if err := os.WriteFile(over, []byte("[llm]\ntemperature = 0.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	c := m.Effective()
	if c.LLM.Temperature != 0.9 { // override 胜
		t.Fatalf("temperature = %v, want 0.9 (override)", c.LLM.Temperature)
	}
	if c.Provider.Model != "deepseek-chat" { // base 填充
		t.Fatalf("model = %q, want base value", c.Provider.Model)
	}
	if c.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
	if c.Session.ID != "aria" || c.Provider.APIKeyEnv != "ARIA_API_KEY" { // 默认值兜底
		t.Fatalf("defaults missing: %+v", c)
	}
	if b := m.Base(); b.LLM.Temperature != 0.5 { // 基准视图不受 override 影响
		t.Fatalf("base temperature = %v, want 0.5", b.LLM.Temperature)
	}
}

func TestSetWritesOverrideOnly(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Set("provider.model", "deepseek-reasoner"); err != nil {
		t.Fatal(err)
	}
	if c := m.Effective(); c.Provider.Model != "deepseek-reasoner" {
		t.Fatalf("model = %q, want override", c.Provider.Model)
	}

	got, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != baseBody { // base 字节级不动（注释保留）
		t.Fatalf("base file rewritten:\n%s", got)
	}
	overBody, err := os.ReadFile(over)
	if err != nil {
		t.Fatal(err)
	}
	if len(overBody) == 0 {
		t.Fatal("override file not written")
	}
}

func TestClearRestoresBase(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, _ := Load(base, over)

	if err := m.Set("provider.model", "x"); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("provider.model"); err != nil {
		t.Fatal(err)
	}
	if c := m.Effective(); c.Provider.Model != "deepseek-chat" {
		t.Fatalf("model = %q, want base restored", c.Provider.Model)
	}
	if _, err := os.Stat(over); !os.IsNotExist(err) { // 覆盖层空了应删文件
		t.Fatalf("override file still exists: %v", err)
	}
	if err := m.Clear("provider.model"); err != nil { // 幂等
		t.Fatal(err)
	}
}

func TestSetLocalProcessOnly(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, _ := Load(base, over)

	m.SetLocal("llm.temperature", 0.1)
	if c := m.Effective(); c.LLM.Temperature != 0.1 {
		t.Fatalf("temperature = %v, want local", c.LLM.Temperature)
	}
	if _, err := os.Stat(over); !os.IsNotExist(err) {
		t.Fatal("local write touched override file")
	}
}

// 外部原子改 base（写临时文件 + rename，模拟 vim）→ 显式 Reload 拿到新值。
func TestReloadPicksUpExternalEdit(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}

	changed := make(chan Config, 1)
	m.OnChange(func(c Config) { changed <- c })

	// 原子写：新内容写临时文件再 rename（模拟 vim）
	tmp := base + ".new"
	if err := os.WriteFile(tmp, []byte(baseBody+"\n[limits]\nmax_turns = 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, base); err != nil {
		t.Fatal(err)
	}

	if !m.Reload() {
		t.Fatal("Reload reported no change after external edit")
	}
	if c := m.Effective(); c.Limits.MaxTurns != 7 {
		t.Fatalf("max_turns = %d, want 7", c.Limits.MaxTurns)
	}
	select {
	case c := <-changed:
		if c.Limits.MaxTurns != 7 {
			t.Fatalf("notified value max_turns = %d", c.Limits.MaxTurns)
		}
	default:
		t.Fatal("Reload did not notify listeners")
	}
}

func TestReloadNoopWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	m, _ := Load(base, filepath.Join(dir, "aria.override.toml"))
	if m.Reload() {
		t.Fatal("Reload reported change with no edits")
	}
}

// 外部把文件改出语法错误：保持旧值不崩，修好后 Reload 能恢复。
func TestReloadKeepsOldOnParseError(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	m, _ := Load(base, filepath.Join(dir, "aria.override.toml"))

	var logged bool
	m.SetLogf(func(string, ...any) { logged = true })

	if err := os.WriteFile(base, []byte("[llm\ntemperature = = 0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.Reload() {
		t.Fatal("Reload reported change for a broken file")
	}
	if !logged {
		t.Fatal("parse error was silent")
	}
	if c := m.Effective(); c.LLM.Temperature != 0.5 { // 仍是旧值
		t.Fatalf("temperature = %v, want previous 0.5", c.LLM.Temperature)
	}

	fixed := strings.Replace(baseBody, "temperature = 0.5", "temperature = 0.8", 1)
	if err := os.WriteFile(base, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.Reload() || m.Effective().LLM.Temperature != 0.8 {
		t.Fatal("reload after fixing the file did not apply")
	}
}

// web 保存的覆盖要能跨重启还原：Set 之后重新 Load 同一组文件，值仍在。
func TestOverridePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")

	m, _ := Load(base, over)
	if err := m.Set("llm.temperature", 0.42); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("provider.model", "deepseek-reasoner"); err != nil {
		t.Fatal(err)
	}

	restarted, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	c := restarted.Effective()
	if c.LLM.Temperature != 0.42 || c.Provider.Model != "deepseek-reasoner" {
		t.Fatalf("restart lost overrides: %+v", c)
	}
	if got, err := os.ReadFile(base); err != nil || string(got) != baseBody {
		t.Fatal("base file must stay untouched across saves")
	}
}

// 原子写不留临时文件。
func TestSetLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, _ := Load(base, over)
	if err := m.Set("llm.max_tokens", 512); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "aria.toml" && e.Name() != "aria.override.toml" {
			t.Fatalf("stray file left behind: %s", e.Name())
		}
	}
}

// 压缩策略点菜：strategy/keep_last_n 可配，默认 provider。
func TestCompressStrategyConfig(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody+`
[compress]
strategy = "keeplast"
keep_last_n = 7
`)
	m, err := Load(base, filepath.Join(dir, "aria.override.toml"))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Effective()
	if c.Compress.Strategy != "keeplast" || c.Compress.KeepLastN != 7 {
		t.Fatalf("compress = %+v", c.Compress)
	}

	// 未配置时走默认：provider、条数留给实现侧默认（0）。
	m2, err := Load(writeBase(t, t.TempDir(), baseBody), filepath.Join(t.TempDir(), "aria.override.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if d := m2.Effective().Compress; d.Strategy != "provider" || d.KeepLastN != 0 {
		t.Fatalf("默认 compress = %+v", d)
	}
}

// 覆盖层能改压缩策略（网页面板点菜路径）。
func TestCompressStrategyOverride(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	m, _ := Load(base, filepath.Join(dir, "aria.override.toml"))
	if err := m.Set("compress.strategy", "keeplast"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("compress.keep_last_n", 3); err != nil {
		t.Fatal(err)
	}
	if c := m.Effective().Compress; c.Strategy != "keeplast" || c.KeepLastN != 3 {
		t.Fatalf("覆盖后 compress = %+v", c)
	}
	if b := m.Base().Compress; b.Strategy != "provider" {
		t.Fatalf("基准层不该被改：%+v", b)
	}
	if err := m.Clear("compress.strategy"); err != nil {
		t.Fatal(err)
	}
	if c := m.Effective().Compress; c.Strategy != "provider" {
		t.Fatalf("恢复默认后 compress = %+v", c)
	}
}

// base 文件被外部删除：保持上次生效值并记日志——绝不静默重置为代码默认值。
// （旧行为：空内容按合法 TOML 解析 → 整份配置被默认值覆盖，还上报「有变更」。）
func TestReloadKeepsValuesWhenBaseDisappears(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	m, err := Load(base, filepath.Join(dir, "aria.override.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Effective().LLM.Temperature; got != 0.5 {
		t.Fatalf("前置值 = %v", got)
	}

	var logged []string
	m.SetLogf(func(f string, a ...any) { logged = append(logged, f) })

	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if m.Reload() {
		t.Fatal("base 消失不该上报为一次配置变更")
	}
	if got := m.Effective().LLM.Temperature; got != 0.5 {
		t.Fatalf("配置被静默重置：temperature = %v, want 0.5", got)
	}
	if got := m.Effective().Provider.Model; got != "deepseek-chat" {
		t.Fatalf("model = %q, want base value", got)
	}
	if len(logged) == 0 {
		t.Fatal("base 消失必须留下日志")
	}
}

// override 文件被外部删除 = 合法的「无覆盖层」：回到基准值并通知。
func TestReloadOverrideDeletionFallsBackToBase(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	over := filepath.Join(dir, "aria.override.toml")
	m, _ := Load(base, over)
	if err := m.Set("llm.temperature", 0.9); err != nil {
		t.Fatal(err)
	}
	notified := 0
	m.OnChange(func(Config) { notified++ })

	if err := os.Remove(over); err != nil {
		t.Fatal(err)
	}
	if !m.Reload() {
		t.Fatal("覆盖层删除应上报变更")
	}
	if got := m.Effective().LLM.Temperature; got != 0.5 {
		t.Fatalf("temperature = %v, want base 0.5", got)
	}
	if notified != 1 {
		t.Fatalf("通知次数 = %d, want 1", notified)
	}
}

// SetLogf 与 Reload 并发（旧实现：logf 在 m.mu 下写、在无锁路径读 → -race 报警）。
func TestSetLogfConcurrentWithReload(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	m, err := Load(base, filepath.Join(dir, "aria.override.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// 把文件改坏：Reload 走「解析失败 + 记日志」分支（读到 m.logf 的那条路径）。
	if err := os.WriteFile(base, []byte("[llm\ntemperature = = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.SetLogf(func(string, ...any) {})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				os.WriteFile(base, []byte("[llm\ntemperature = = 2\n"), 0o644)
				m.Reload()
			}
		}()
	}
	wg.Wait()
}

// 写盘失败必须回滚内存覆盖层：否则内存（生效值）与磁盘（下次启动的值）分叉。
func TestSetRollsBackWhenWriteFails(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody)
	// 覆盖层放在不存在的目录里：Load 容忍缺失，Set 落盘必然失败。
	over := filepath.Join(dir, "missing-dir", "aria.override.toml")
	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set("llm.temperature", 0.1); err == nil {
		t.Fatal("落盘不可能成功，却返回了 nil")
	}
	if got := m.Effective().LLM.Temperature; got != 0.5 {
		t.Fatalf("写失败后内存未回滚：temperature = %v, want 基准 0.5", got)
	}
}

// 配置层给工具超时一个安全默认（core 的 0 语义是「不限时」）。
func TestToolTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(writeBase(t, dir, baseBody), filepath.Join(dir, "aria.override.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Effective().Limits.ToolTimeoutMS; got != 30000 {
		t.Fatalf("tool_timeout_ms 默认 = %d, want 30000", got)
	}
	// 显式写 0 = 不限时，必须被尊重而不是被默认值顶掉。
	if err := m.Set("limits.tool_timeout_ms", 0); err != nil {
		t.Fatal(err)
	}
	if got := m.Effective().Limits.ToolTimeoutMS; got != 0 {
		t.Fatalf("显式 0 被默认值顶掉：%d", got)
	}
}

// [host] 段（宿主启动项）：base 给值、override 覆盖、未写的走代码默认。
func TestHostSection(t *testing.T) {
	dir := t.TempDir()
	base := writeBase(t, dir, baseBody+`
[host]
device = "http://127.0.0.1:18900"
no_tts = true
`)
	over := filepath.Join(dir, "aria.override.toml")
	if err := os.WriteFile(over, []byte("[host]\nasr_gateway = \"ws://172.16.53.179:8210\"\nno_tts = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	h := m.Effective().Host
	if h.Device != "http://127.0.0.1:18900" { // base 填充
		t.Fatalf("device = %q, want base value", h.Device)
	}
	if h.ASRGateway != "ws://172.16.53.179:8210" { // override 胜
		t.Fatalf("asr_gateway = %q, want override value", h.ASRGateway)
	}
	if h.NoTTS { // override 的 false 盖掉 base 的 true
		t.Fatal("no_tts = true, want override false")
	}
	if h.Backend != "http://127.0.0.1:8800" || h.LightColors != "202020,00a000,2050ff" { // 默认兜底
		t.Fatalf("host 默认缺失：%+v", h)
	}
	if h.NoASR || h.NoStdin || h.NoInputGate || h.NoLight || h.NoMicMute || h.NoVision {
		t.Fatalf("no_* 零值默认被破坏：%+v", h)
	}
}
