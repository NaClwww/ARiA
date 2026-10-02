package assemble

import (
	"testing"

	"aria/internal/config"
)

// engine 为空返回 nil；hindsight 建立适配层；未知名字报错。
func TestMemoryAssembly(t *testing.T) {
	if svc, err := Memory(config.Memory{}, nil); svc != nil || err != nil {
		t.Fatalf("空 engine 应返回 nil：%v %v", svc, err)
	}
	if svc, err := Memory(config.Memory{Engine: "hindsight", Dir: t.TempDir()}, nil); svc == nil || err != nil {
		t.Fatalf("hindsight 应装配成功：%v %v", svc, err)
	}
	if _, err := Memory(config.Memory{Engine: "mem0"}, nil); err == nil {
		t.Fatal("未知名字应报错")
	}
}
