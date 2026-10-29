package assemble

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"

	"aria/internal/config"
	"aria/plugins/persist/jsonl"
	"aria/runtime/persist"
)

// MemoryCache 缓存记忆服务装配结果：[memory] 段不变时复用同一实例（热更新的每次重新装配不重建）。
// 换下来的实例在会话切换前仍被会话使用，所以建过的全部保留，由 Close 统一收尾。并发安全。
type MemoryCache struct {
	Log *slog.Logger

	mu  sync.Mutex
	key config.Memory
	res MemoryResult
	ok  bool
	all []MemoryResult
}

// Get 返回 cfg 对应的记忆服务；[memory] 段变化时重新装配，失败时不替换缓存。
func (c *MemoryCache) Get(cfg config.Memory) (MemoryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && reflect.DeepEqual(c.key, cfg) {
		return c.res, nil
	}
	res, err := Memory(cfg, c.Log)
	if err != nil {
		return MemoryResult{}, err
	}
	c.key, c.res, c.ok = cfg, res, true
	c.all = append(c.all, res)
	return res, nil
}

// Close 等待建过的全部记忆服务结束后台提交（各实例的 Close），ctx 为总上限。
func (c *MemoryCache) Close(ctx context.Context) error {
	c.mu.Lock()
	all := c.all
	c.all = nil
	c.mu.Unlock()
	var errs []error
	for _, r := range all {
		if r.Close != nil {
			errs = append(errs, r.Close(ctx))
		}
	}
	return errors.Join(errs...)
}

// StoreCache 缓存落盘 store：[record] path 不变时复用同一实例；空路径 = 不落盘（nil）。
// 换下来的 store 在会话切换前仍在写，建过的全部保留到 Close。并发安全。
type StoreCache struct {
	mu    sync.Mutex
	path  string
	store persist.Store
	ok    bool
	all   []*jsonl.Store
}

// Get 返回 path 对应的 store；path 变化时打开新文件，失败时不替换缓存。
func (c *StoreCache) Get(path string) (persist.Store, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.path == path {
		return c.store, nil
	}
	var store persist.Store
	if path != "" {
		s, err := jsonl.New(path)
		if err != nil {
			return nil, err
		}
		c.all = append(c.all, s)
		store = s
	}
	c.path, c.store, c.ok = path, store, true
	return store, nil
}

// Close 关闭建过的全部 store。
func (c *StoreCache) Close() error {
	c.mu.Lock()
	all := c.all
	c.all = nil
	c.mu.Unlock()
	var errs []error
	for _, s := range all {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}
