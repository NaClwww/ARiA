package assemble

import (
	"testing"

	"aria/internal/config"
	"aria/runtime/memory"
)

// engine 为空返回空结果；hindsight 建立适配层、工具与 Close；未知名字报错。
func TestMemoryAssembly(t *testing.T) {
	if res, err := Memory(config.Memory{}, nil); res.Service != nil || res.Tool != nil || res.Close != nil || err != nil {
		t.Fatalf("空 engine 应返回空结果：%+v %v", res, err)
	}
	res, err := Memory(config.Memory{Engine: "hindsight", Dir: t.TempDir()}, nil)
	if err != nil || res.Service == nil || res.Close == nil || res.Tool == nil || res.Tool.Def().Name != memory.RecallToolName {
		t.Fatalf("hindsight 应装配成功：%+v %v", res, err)
	}
	if _, err := Memory(config.Memory{Engine: "mem0"}, nil); err == nil {
		t.Fatal("未知名字应报错")
	}
}
