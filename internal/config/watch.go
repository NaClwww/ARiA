package config

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultWatchDebounce 是 Watch 的默认静默期：一次保存常产生多条事件（写临时文件、改名、
// 截断再写），静默期过后只回调一次，也避免读到写到一半的文件。
const DefaultWatchDebounce = 200 * time.Millisecond

// Watch 监听 paths 中的文件，任一文件被写入 / 创建 / 改名 / 删除后，静默 debounce
// （≤0 = DefaultWatchDebounce）再调用一次 fn；ctx 取消即停止。fn 在同一个 goroutine 里
// 串行调用。
//
// Watch 只负责「文件变了」这一信号，不认识 Manager：宿主通常传 Manager.Paths 与
// 「调 Manager.Reload」的 fn——变化与否由 Reload 比对内容判定，语法错误等也由 Reload
// 处理，所以多报（touch、自己写回 override）无害。换监听方式只需换掉本函数。
//
// 监听的是文件所在目录而不是文件本身：编辑器的原子保存（写临时文件再改名）会换掉
// 文件 inode，直接监听文件会在第一次保存后失明；文件被删再重建也照样能收到。
// 建立监听失败时同步返回错误，不启动 goroutine。
func Watch(ctx context.Context, paths []string, debounce time.Duration, fn func()) error {
	if debounce <= 0 {
		debounce = DefaultWatchDebounce
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config: watch: %w", err)
	}
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			w.Close()
			return fmt.Errorf("config: watch %s: %w", p, err)
		}
		files[abs] = true
		dirs[filepath.Dir(abs)] = true
	}
	for d := range dirs {
		if err := w.Add(d); err != nil {
			w.Close()
			return fmt.Errorf("config: watch %s: %w", d, err)
		}
	}

	go func() {
		defer w.Close()
		timer := time.NewTimer(debounce)
		timer.Stop()
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !files[filepath.Clean(ev.Name)] || ev.Op == fsnotify.Chmod {
					continue
				}
				timer.Reset(debounce)
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				// 事件溢出等：保守地当作「可能变了」，由 Reload 比对内容。
				timer.Reset(debounce)
			case <-timer.C:
				fn()
			}
		}
	}()
	return nil
}

// Paths 返回 Manager 读取的两个文件路径（base、override），供 Watch 使用。
func (m *Manager) Paths() []string { return []string{m.basePath, m.overPath} }
