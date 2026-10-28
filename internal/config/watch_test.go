package config

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const testDebounce = 50 * time.Millisecond

// watchCount 监听 paths，返回回调计数。
func watchCount(t *testing.T, paths ...string) *atomic.Int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var n atomic.Int32
	if err := Watch(ctx, paths, testDebounce, func() { n.Add(1) }); err != nil {
		t.Fatal(err)
	}
	return &n
}

// eventually 等 cond 成立（文件事件是异步的）。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle 等过若干静默期，确认没有多余回调。
func settle() { time.Sleep(4 * testDebounce) }

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 编辑器式原子保存（写临时文件 + 改名）：多条事件合并成一次回调；之后再保存照样收得到。
func TestWatchAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.toml")
	writeFile(t, path, "x = 1\n")
	n := watchCount(t, path)

	save := func(s string) {
		tmp := filepath.Join(dir, ".a.toml.swp")
		writeFile(t, tmp, s)
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	save("x = 2\n")
	eventually(t, "first callback", func() bool { return n.Load() == 1 })
	settle()
	if got := n.Load(); got != 1 {
		t.Fatalf("callbacks after one save = %d, want 1", got)
	}
	save("x = 3\n")
	eventually(t, "second callback", func() bool { return n.Load() == 2 })
}

// 同目录的其他文件不触发；被删再重建都触发（override 被 Clear 删掉后仍在监听）。
func TestWatchOtherFilesAndRecreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "over.toml") // 不存在也能监听
	n := watchCount(t, path)

	writeFile(t, filepath.Join(dir, "unrelated.txt"), "hi")
	settle()
	if got := n.Load(); got != 0 {
		t.Fatalf("unrelated file triggered %d callbacks", got)
	}

	writeFile(t, path, "a = 1\n")
	eventually(t, "create", func() bool { return n.Load() == 1 })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, "remove", func() bool { return n.Load() == 2 })
	writeFile(t, path, "a = 2\n")
	eventually(t, "recreate", func() bool { return n.Load() == 3 })
}

// ctx 取消后不再回调；目录不存在时同步报错。
func TestWatchStopAndError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.toml")
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	if err := Watch(ctx, []string{path}, testDebounce, func() { n.Add(1) }); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(testDebounce)
	writeFile(t, path, "x = 1\n")
	settle()
	if got := n.Load(); got != 0 {
		t.Fatalf("callbacks after cancel = %d", got)
	}

	if err := Watch(context.Background(), []string{filepath.Join(dir, "missing", "a.toml")}, 0, func() {}); err == nil {
		t.Fatal("want error for missing directory")
	}
}

// 与 Manager 组合（宿主的用法）：外部改 base → Watch 回调 Reload → OnChange 拿到新值。
func TestWatchDrivesReload(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "aria.toml")
	over := filepath.Join(dir, "aria.override.toml")
	writeFile(t, base, "[provider]\nmodel = \"a\"\n")
	m, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Value
	m.OnChange(func(c Config) { got.Store(c.Provider.Model) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Watch(ctx, m.Paths(), testDebounce, func() { m.Reload() }); err != nil {
		t.Fatal(err)
	}

	writeFile(t, base, "[provider]\nmodel = \"b\"\n")
	eventually(t, "OnChange with new model", func() bool { v, _ := got.Load().(string); return v == "b" })
	if m.Effective().Provider.Model != "b" {
		t.Fatalf("Effective model = %q", m.Effective().Provider.Model)
	}

	// 程序自己写 override（Set）也会触发文件事件，但内容已消化，Reload 不重复通知。
	var calls atomic.Int32
	m.OnChange(func(Config) { calls.Add(1) })
	if err := m.Set("provider.model", "c"); err != nil {
		t.Fatal(err)
	}
	settle()
	if n := calls.Load(); n != 1 {
		t.Fatalf("OnChange after Set = %d, want 1 (no duplicate from watcher)", n)
	}
}
