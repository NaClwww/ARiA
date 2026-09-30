// Package config 是宿主侧配置管理器（2026-09-14 定稿）：
//
//	启动：aria.toml（人写，带注释）+ aria.override.toml（网页保存的产物）→ 内存
//	运行：web 改 → 内存立即生效 → 原子写回 override 文件（只为下次启动）
//	重启：文件把状态还回来
//
// 生效优先级：local（进程内 flag）> override > base > 代码内默认值。
//
// 为什么分两个文件：viper 写回会把整个文件重写成一张 map，**注释必丢**
// （viper#1104，官方无修复计划）。所以 base 永远只读、由人写；只有 override
// （只含被改的键、程序独占写）会被重写——机器文件没有注释无所谓。
//
// **不做自动监听**：外部手改文件后要生效，调 Reload（面板「重新读取」按钮）
// 或重启进程。不监听也就不存在「漏事件」：文件级 fsnotify 对原子保存失明
// （vim 写临时文件再改名会换掉监听对象，viper#142），mtime 也不行——实测本机
// 时间戳粒度粗到 tmp+rename 前后完全相同。
package config

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/spf13/viper"
)

// Config 是生效配置的快照（字段与 aria.toml 一一对应）。各段是具名字段，
// 访问一律带段名（c.Provider.Model）：内嵌会让 c.Model 这类写法在多个段
// 有同名字段时产生歧义（Provider.Model 与 Compress.Model）。
type Config struct {
	Host     Host
	Server   Server
	Provider Provider
	Persona  Persona
	Session  Session
	LLM      LLM
	Compress Compress
	Tools    Tools
	Record   Record
	Limits   Limits
}

// Host 是宿主装配段（cmd/aria-host 的启动项基准）：地址与开关。同名 flag
// 显式给出时由 main.go 覆盖（进程内、不落盘）；机器差异（IP/端口/adb 转发
// 口）放 override 层。fake/api-key 不进本段——前者是冒烟开关、后者是秘密，
// 都留在 flag。
type Host struct {
	Backend     string // TTS 后端根地址（非 gateway 模式下也是 ASR 地址）
	ASRGateway  string // 非空 = ASR 走 asr-gateway（WS，需 device）
	Device      string // launcher 控制面根地址；空 = 本机 paplay、无灯无视觉
	LightColors string // 灯色 idle,listening,thinking（hex，逗号分隔）
	SpeakTool   bool   // TTS 改为 speak 工具：模型显式调用出声，正文不自动朗读
	NoASR       bool   // 停用 ASR 插头（纯终端开发）
	NoStdin     bool   // 停用 stdin 插头（纯语音形态）
	NoTTS       bool   // 停用 TTS 播放（只看文字）
	NoInputGate bool   // 关闭半双工闸门（说话/生成期间不收新输入）
	NoLight     bool   // 停用状态灯
	NoMicMute   bool   // 停用回合内闭耳
	NoVision    bool   // 停用视觉注入
}

type Server struct {
	Listen string
}

type Provider struct {
	BaseURL   string
	Model     string
	APIKeyEnv string // key 本体永远在环境变量；这里只存变量名
}

type Persona struct {
	SystemPrompt     string
	SystemPromptFile string // 非空优先；加载失败报错不静默
}

type Session struct {
	ID          string
	DefaultUser string
}

type LLM struct {
	Temperature float64
	MaxTokens   int
}

type Compress struct {
	Strategy    string // provider（默认，LLM 摘要）| keeplast（只留最近 N 条）| 将来 twopart
	KeepLastN   int    // strategy=keeplast 的条数；0 = 实现侧默认（window.DefaultKeepLast）
	Model       string // strategy=provider 的模型覆盖；空 = 跟随 provider.model
	Instruction string // strategy=provider 的提示词覆盖；空 = 默认提示词
}

type Tools struct {
	Builtin []string
}

type Record struct {
	Path string
}

type Limits struct {
	MaxTurns      int
	ToolTimeoutMS int
}

// Manager 持有两层文件并维护合并视图。并发安全。
type Manager struct {
	mu       sync.Mutex
	base     *viper.Viper // defaults + base 文件
	over     *viper.Viper // 仅 override 键（UI 写入的）
	local    map[string]any
	basePath string
	overPath string

	// 各层已消化的文件内容：Reload 时比对，未变（空写/touch/自己刚写的）就不重载。
	baseBytes, overBytes []byte

	logMu sync.Mutex                       // 只保护 logf：SetLogf 会被并发重注册
	logf  func(format string, args ...any) // 默认丢弃；宿主可接日志/面板

	listenersMu sync.Mutex
	listeners   []func(Config)
}

// log 读 logf 并调用（读走 logMu；调用时已释放，回调里调 Manager 方法不会自锁）。
func (m *Manager) log(format string, args ...any) {
	m.logMu.Lock()
	fn := m.logf
	m.logMu.Unlock()
	if fn != nil {
		fn(format, args...)
	}
}

// SetLogf 接一个日志函数（外部文件语法错误等会走它）。不接则静默——但静默会
// 让「改了配置为什么没生效」无从排查，建议宿主接上。
func (m *Manager) SetLogf(fn func(format string, args ...any)) {
	if fn == nil {
		fn = func(string, ...any) {}
	}
	m.logMu.Lock()
	m.logf = fn
	m.logMu.Unlock()
}

// Load 建立 Manager。base 文件必须存在（语法错误直接报错，启动期 fail-closed）；
// override 文件可缺省（= 无覆盖层）。
func Load(basePath, overridePath string) (*Manager, error) {
	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		return nil, fmt.Errorf("config: read base: %w", err)
	}
	base := viper.New()
	setDefaults(base)
	base.SetConfigType("toml")
	if err := base.ReadConfig(bytes.NewReader(baseBytes)); err != nil {
		return nil, fmt.Errorf("config: parse base: %w", err)
	}

	overBytes, err := os.ReadFile(overridePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: read override: %w", err)
	}
	over := viper.New()
	over.SetConfigType("toml")
	if len(overBytes) > 0 {
		if err := over.ReadConfig(bytes.NewReader(overBytes)); err != nil {
			return nil, fmt.Errorf("config: parse override: %w", err)
		}
	}

	return &Manager{
		base: base, over: over,
		local:    map[string]any{},
		basePath: basePath, overPath: overridePath,
		baseBytes: baseBytes, overBytes: overBytes,
		logf: func(string, ...any) {},
	}, nil
}

// Effective 返回当前生效配置（local > override > base > 默认）。
func (m *Manager) Effective() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot()
}

// Base 返回无覆盖、无 local 的基准视图（UI 显示「默认值」用）。
func (m *Manager) Base() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return configFrom(mergeLayers(m.base.AllSettings())) // base 实例已含默认值
}

// Set 写一条覆盖：更新 override 层并落盘（base 不动），随后通知监听者。
// 落盘走「写临时文件 + 改名」的原子写：读者永远看不到半截文件。
// 落盘失败则回滚内存改动——否则内存（生效值）与磁盘（下次启动的值）分叉，
// 宿主只看到一次报错、之后却按「已保存」的值继续跑。
func (m *Manager) Set(key string, val any) error {
	m.mu.Lock()
	before := m.over.AllSettings()
	m.over.Set(key, val)
	if err := writeAtomic(m.over, m.overPath); err != nil {
		m.restoreOverLocked(before)
		m.mu.Unlock()
		return fmt.Errorf("config: write override: %w", err)
	}
	m.overBytes = readFileOrNil(m.overPath)
	snap := m.snapshot()
	m.mu.Unlock()

	m.notify(snap)
	return nil
}

// Clear 删除一条覆盖（回到基准值）。
func (m *Manager) Clear(key string) error {
	m.mu.Lock()
	if !m.over.IsSet(key) {
		m.mu.Unlock()
		return nil // 本来就没有覆盖，幂等
	}
	before := m.over.AllSettings()
	settings := m.over.AllSettings()
	deleteKey(settings, key)
	fresh := viper.New()
	fresh.SetConfigType("toml")
	if err := fresh.MergeConfigMap(settings); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("config: rebuild override: %w", err)
	}
	m.over = fresh
	if len(settings) == 0 {
		if err := os.Remove(m.overPath); err != nil && !os.IsNotExist(err) {
			m.restoreOverLocked(before)
			m.mu.Unlock()
			return fmt.Errorf("config: remove empty override: %w", err)
		}
	} else if err := writeAtomic(m.over, m.overPath); err != nil {
		m.restoreOverLocked(before)
		m.mu.Unlock()
		return fmt.Errorf("config: write override: %w", err)
	}
	m.overBytes = readFileOrNil(m.overPath)
	snap := m.snapshot()
	m.mu.Unlock()

	m.notify(snap)
	return nil
}

// SetLocal 注入进程内覆盖（flag 用）：只影响本进程的 Effective，不落盘。
func (m *Manager) SetLocal(key string, val any) {
	m.mu.Lock()
	m.local[key] = val
	snap := m.snapshot()
	m.mu.Unlock()
	m.notify(snap)
}

// OnChange 注册变更回调（Set/Clear/SetLocal 与外部文件重载都会触发）。
func (m *Manager) OnChange(fn func(Config)) {
	m.listenersMu.Lock()
	m.listeners = append(m.listeners, fn)
	m.listenersMu.Unlock()
}

// Reload 显式重读两层文件（不做任何自动监听）：外部手改了配置、想让改动生效
// 时调它（web 面板给一个「重新读取」按钮）。返回是否有实际变化。
//
// 设计取向（2026-09-14 定稿）：**启动时读一次，运行期只写回**——web 是配置的
// 操作面（内存即真相），文件是持久化载体（下次启动还原）。
// 免去监听后，文件级监听的原子保存盲区（viper#142）、mtime 粒度（实测同纳秒）
// 这些坑随之消失。base 仍永不被程序重写，注释安全。
func (m *Manager) Reload() bool {
	baseChanged := m.reloadIfChanged(m.basePath, true)
	overChanged := m.reloadIfChanged(m.overPath, false)
	return baseChanged || overChanged
}

// reloadIfChanged 若文件内容与已消化值不同则（持锁）重载对应层并通知，
// 返回是否重载。内容比对在锁内做：seen 字段与 Set 的写入并发。
//
// 读不到文件时的取向：**override 消失 = 合法的「无覆盖层」**（回到基准）；
// **base 消失/读不了 = 保持上次的值并记日志**——base 是必需文件，若按空内容
// 解析会把整份配置静默重置成代码默认值（比语法错误更严重）。语法错误同样
// 保持旧层不动，仅记日志。
func (m *Manager) reloadIfChanged(path string, isBase bool) bool {
	cur, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !isBase {
			cur = nil // 覆盖层被删除：等价于空覆盖
		} else {
			m.log("config: 读 %s 失败（%v），保持上次生效的值", path, err)
			return false
		}
	}

	m.mu.Lock()
	seen := m.baseBytes
	if !isBase {
		seen = m.overBytes
	}
	if bytes.Equal(cur, seen) {
		m.mu.Unlock()
		return false
	}
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(cur)); err != nil {
		m.mu.Unlock()
		m.log("config: %s has a parse error, keeping previous values: %v", path, err)
		return false
	}
	if isBase {
		setDefaults(v) // 重载后仍带默认值兜底
		m.base, m.baseBytes = v, cur
	} else {
		m.over, m.overBytes = v, cur
	}
	snap := m.snapshot()
	m.mu.Unlock()

	m.notify(snap)
	return true
}

// writeAtomic 原子写配置文件：写同目录临时文件 + 改名覆盖。
// viper 按扩展名判格式，临时名保留 .toml 后缀。
func writeAtomic(v *viper.Viper, path string) error {
	tmp := path + ".tmp.toml"
	if err := v.WriteConfigAs(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---------- 内部 ----------

// snapshot 出当前生效视图：local 覆盖 override，override 覆盖 base。
func (m *Manager) snapshot() Config {
	return configFrom(mergeLayers(m.base.AllSettings(), m.over.AllSettings(), flatten(m.local)))
}

// mergeLayers 依次深合并各层（后者胜），产出一份只读快照。
func mergeLayers(layers ...map[string]any) *viper.Viper {
	v := viper.New()
	v.SetConfigType("toml")
	for _, l := range layers {
		if len(l) > 0 {
			_ = v.MergeConfigMap(l)
		}
	}
	return v
}

func (m *Manager) notify(c Config) {
	m.listenersMu.Lock()
	fns := make([]func(Config), len(m.listeners))
	copy(fns, m.listeners)
	m.listenersMu.Unlock()
	for _, fn := range fns {
		fn(c)
	}
}

func configFrom(v *viper.Viper) Config {
	return Config{
		Host: Host{
			Backend:     v.GetString("host.backend"),
			ASRGateway:  v.GetString("host.asr_gateway"),
			Device:      v.GetString("host.device"),
			LightColors: v.GetString("host.light_colors"),
			SpeakTool:   v.GetBool("host.speak_tool"),
			NoASR:       v.GetBool("host.no_asr"),
			NoStdin:     v.GetBool("host.no_stdin"),
			NoTTS:       v.GetBool("host.no_tts"),
			NoInputGate: v.GetBool("host.no_input_gate"),
			NoLight:     v.GetBool("host.no_light"),
			NoMicMute:   v.GetBool("host.no_mic_mute"),
			NoVision:    v.GetBool("host.no_vision"),
		},
		Server: Server{Listen: v.GetString("server.listen")},
		Provider: Provider{
			BaseURL:   v.GetString("provider.base_url"),
			Model:     v.GetString("provider.model"),
			APIKeyEnv: v.GetString("provider.api_key_env"),
		},
		Persona: Persona{
			SystemPrompt:     v.GetString("persona.system_prompt"),
			SystemPromptFile: v.GetString("persona.system_prompt_file"),
		},
		Session: Session{
			ID:          v.GetString("session.id"),
			DefaultUser: v.GetString("session.default_user"),
		},
		LLM: LLM{
			Temperature: v.GetFloat64("llm.temperature"),
			MaxTokens:   v.GetInt("llm.max_tokens"),
		},
		Compress: Compress{
			Strategy:    v.GetString("compress.strategy"),
			KeepLastN:   v.GetInt("compress.keep_last_n"),
			Model:       v.GetString("compress.model"),
			Instruction: v.GetString("compress.instruction"),
		},
		Tools:  Tools{Builtin: v.GetStringSlice("tools.builtin")},
		Record: Record{Path: v.GetString("record.path")},
		Limits: Limits{
			MaxTurns:      v.GetInt("limits.max_turns"),
			ToolTimeoutMS: v.GetInt("limits.tool_timeout_ms"),
		},
	}
}

func setDefaults(v *viper.Viper) {
	// host.no_* 不设默认：bool 零值 false 就是默认（关）。
	v.SetDefault("host.backend", "http://127.0.0.1:8800")
	v.SetDefault("host.asr_gateway", "")
	v.SetDefault("host.device", "")
	v.SetDefault("host.light_colors", "202020,00a000,2050ff")
	v.SetDefault("server.listen", "127.0.0.1:8080")
	v.SetDefault("provider.base_url", "")
	v.SetDefault("provider.model", "")
	v.SetDefault("provider.api_key_env", "ARIA_API_KEY")
	v.SetDefault("persona.system_prompt", "")
	v.SetDefault("persona.system_prompt_file", "")
	v.SetDefault("session.id", "aria")
	v.SetDefault("session.default_user", "user")
	// llm.temperature / llm.max_tokens 故意无默认：0 = 不下发该字段，
	// 交给 provider 的模型默认（thinking 型模型一般不用这两个旋钮）。
	v.SetDefault("compress.strategy", "provider")
	v.SetDefault("compress.keep_last_n", 0)
	v.SetDefault("compress.model", "")
	v.SetDefault("compress.instruction", "")
	v.SetDefault("tools.builtin", []string{"now", "lorem"})
	v.SetDefault("record.path", "")
	v.SetDefault("limits.max_turns", 0)
	v.SetDefault("limits.tool_timeout_ms", 30000) // 30s：core 的 0 语义是「不限时」，配置层给个安全默认
}

// restoreOverLocked 按快照重建 override 层（写盘失败后的回滚；调用方持锁）。
func (m *Manager) restoreOverLocked(settings map[string]any) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.MergeConfigMap(settings); err == nil {
		m.over = v
	}
}

// readFileOrNil 读文件；不存在返回 nil（= 空层）。
func readFileOrNil(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// deleteKey 按点分路径从嵌套 map 删除键，并自底向上清掉变空的父表
// （否则空 [provider] 会让 override 文件删不掉、Clear 回不到「无覆盖」态）。
func deleteKey(settings map[string]any, key string) {
	parts := splitKey(key)
	chain := []map[string]any{settings}
	cur := settings
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return
		}
		chain = append(chain, next)
		cur = next
	}
	delete(cur, parts[len(parts)-1])
	for i := len(chain) - 1; i > 0; i-- {
		parent, child := chain[i-1], parts[i-1]
		if len(chain[i]) == 0 {
			delete(parent, child)
		}
	}
}

// flatten 把 {"a.b": v} 展开为 {"a":{"b": v}}，供 MergeConfigMap 消费。
func flatten(kv map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range kv {
		parts := splitKey(k)
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = v
	}
	return out
}

func splitKey(key string) []string {
	var parts []string
	cur := ""
	for _, r := range key {
		if r == '.' {
			parts = append(parts, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(parts, cur)
}
