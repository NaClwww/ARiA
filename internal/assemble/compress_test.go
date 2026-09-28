package assemble

import (
	"strings"
	"testing"

	"aria/core/provider"
	"aria/internal/config"
	"aria/runtime/window"
)

func TestCompressorStrategies(t *testing.T) {
	prov := provider.NewFake()
	base := config.Config{}
	base.Compress.Strategy = "provider"
	base.Compress.Model = "sum-model"
	base.Compress.Instruction = "自定义提示词"

	tests := []struct {
		name     string
		cfg      config.Config
		wantErr  string
		wantKeep int  // >0 表示期望 KeepLast(n)
		wantProv bool // 期望 ProviderCompressor
	}{
		{
			name:     "默认（空）走 provider",
			cfg:      config.Config{},
			wantProv: true,
		},
		{
			name:     "provider 带模型与提示词覆盖",
			cfg:      base,
			wantProv: true,
		},
		{
			name: "keeplast 指定条数",
			cfg: func() config.Config {
				c := config.Config{}
				c.Compress.Strategy = "keeplast"
				c.Compress.KeepLastN = 7
				return c
			}(),
			wantKeep: 7,
		},
		{
			name: "keeplast 未指定条数用引擎默认",
			cfg: func() config.Config {
				c := config.Config{}
				c.Compress.Strategy = "keeplast"
				return c
			}(),
			wantKeep: window.DefaultKeepLast,
		},
		{
			name: "未知策略显式报错",
			cfg: func() config.Config {
				c := config.Config{}
				c.Compress.Strategy = "twopart"
				return c
			}(),
			wantErr: "twopart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Compressor(tt.cfg, prov)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want 提到 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch got := c.(type) {
			case *window.ProviderCompressor:
				if !tt.wantProv {
					t.Fatalf("得到 ProviderCompressor，期望别的：%+v", got)
				}
				if tt.cfg.Compress.Model != "" && got.Model != tt.cfg.Compress.Model {
					t.Fatalf("Model = %q", got.Model)
				}
				if tt.cfg.Compress.Instruction != "" && got.Instruction != tt.cfg.Compress.Instruction {
					t.Fatalf("Instruction = %q", got.Instruction)
				}
				if got.Provider == nil {
					t.Fatal("Provider 未注入")
				}
			case window.KeepLast:
				if tt.wantKeep == 0 {
					t.Fatalf("得到 KeepLast(%d)，期望 ProviderCompressor", int(got))
				}
				if int(got) != tt.wantKeep {
					t.Fatalf("KeepLast = %d, want %d", int(got), tt.wantKeep)
				}
			default:
				t.Fatalf("未知实现类型 %T", c)
			}
		})
	}
}

func TestCompressorNamesCoverAllStrategies(t *testing.T) {
	for _, name := range CompressorNames() {
		c := config.Config{}
		c.Compress.Strategy = name
		if _, err := Compressor(c, provider.NewFake()); err != nil {
			t.Fatalf("CompressorNames 列出的 %q 装配失败：%v", name, err)
		}
	}
}
