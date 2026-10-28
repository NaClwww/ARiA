// Package modelcard 是模型元数据卡（docs/01 §3）：上下文窗口、输出上限、输入模态、思考/工具
// 能力与价格等。数据源是 models.dev（https://models.dev/api.json，OpenCode 维护的开放目录，
// 按 provider 键分组，同一模型在不同 provider 下的卡可以不同）。
//
// 本包只依赖标准库：类型 + 一次性拉取，不做缓存与后台刷新——何时拉、拉到前怎么办由调用方
// （provider 适配器）决定。
package modelcard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// DefaultURL 是 models.dev 的完整目录（全部 provider，数 MB）。
const DefaultURL = "https://models.dev/api.json"

const userAgent = "aria/1.0"

// Card 是一个模型在某个 provider 下的元数据。缺失的字段为零值。
type Card struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Family      string `json:"family"`
	CanonicalID string `json:"canonical_model_id"` // 跨 provider 的统一标识，如 "zhipuai/glm-5.3-flash"
	Status      string `json:"status"`             // 空 | alpha | beta | deprecated

	Attachment       bool              `json:"attachment"` // 接受文件/图片附件
	Reasoning        bool              `json:"reasoning"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options"`
	ToolCall         bool              `json:"tool_call"`
	StructuredOutput bool              `json:"structured_output"`
	Temperature      bool              `json:"temperature"` // 接受 temperature 参数
	OpenWeights      bool              `json:"open_weights"`

	Modalities Modalities `json:"modalities"`
	Limit      Limit      `json:"limit"`
	Cost       *Cost      `json:"cost"` // nil = 目录未给价格

	Knowledge   string `json:"knowledge"`    // 知识截止，如 "2025-05"
	ReleaseDate string `json:"release_date"` // YYYY-MM-DD
	LastUpdated string `json:"last_updated"`

	// Endpoint 是该模型在本 provider 下的特殊端点（如走 Anthropic messages 的模型）；
	// 零值 = 用 provider 的默认端点。
	Endpoint Endpoint `json:"provider"`
}

// ReasoningOption 是一种思考控制方式：Type 为 "effort"（Values 为可选等级）或 "toggle"（开关）。
type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

// Modalities 是输入/输出模态：text、image、audio、video、pdf。
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Limit 是 token 限额。Input 多数模型不给（0 = 与 Context 相同）。
type Limit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"` // 模型的最大输出，不是请求不设 max_tokens 时的服务端默认值
}

// Cost 是价格，单位美元 / 1M token。
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Endpoint 是模型级端点覆盖：API 为端点根地址，NPM 为对应的 AI SDK 包名（标识 API 风格）。
type Endpoint struct {
	API string `json:"api"`
	NPM string `json:"npm"`
}

// Override 返回把 l 的非零字段覆盖到 c.Limit 上的副本：配置里写了的限额优先于目录值，
// 没写（0）的沿用目录值。
func (c Card) Override(l Limit) Card {
	if l.Context > 0 {
		c.Limit.Context = l.Context
	}
	if l.Input > 0 {
		c.Limit.Input = l.Input
	}
	if l.Output > 0 {
		c.Limit.Output = l.Output
	}
	return c
}

// Accepts 报告模型是否接受输入模态 m（如 "image"）。
func (c Card) Accepts(m string) bool { return slices.Contains(c.Modalities.Input, m) }

// Cards 是一个 provider 下的全部模型卡，键为模型 ID。
type Cards map[string]Card

// Fetch 从 models.dev 格式的目录 url 读取 provider 键下的全部模型卡；hc 为 nil 时用
// http.DefaultClient（超时由 ctx 控制）。目录中没有该 provider 时报错。
func Fetch(ctx context.Context, hc *http.Client, url, provider string) (Cards, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelcard: status %d", resp.StatusCode)
	}
	// 顶层只解到 RawMessage，再解所需的一个 provider：其余几百个不做结构化解码。
	var all map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("modelcard: decode catalog: %w", err)
	}
	raw, ok := all[provider]
	if !ok {
		return nil, fmt.Errorf("modelcard: catalog has no provider %q", provider)
	}
	var p struct {
		Models Cards `json:"models"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("modelcard: decode provider %q: %w", provider, err)
	}
	return p.Models, nil
}
