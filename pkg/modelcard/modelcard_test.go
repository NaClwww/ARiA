package modelcard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// 字段取自 models.dev 真实条目（2026-10，opencode-go）；另含一个无价格的模型与无关 provider。
const catalog = `{
  "other": {"id": "other", "models": {"x": {"id": "x", "limit": {"context": 1, "output": 1}}}},
  "opencode-go": {
    "id": "opencode-go", "api": "https://opencode.ai/inference/go/openai/v1",
    "models": {
      "glm-5.3-flash": {
        "id": "glm-5.3-flash", "name": "GLM-5.3-Flash", "family": "glm-flash",
        "attachment": true, "reasoning": true,
        "reasoning_options": [{"type": "effort", "values": ["low", "high", "max"]}],
        "tool_call": true, "interleaved": {"field": "reasoning_content"},
        "structured_output": true, "temperature": true,
        "release_date": "2026-08-26", "last_updated": "2026-08-26",
        "modalities": {"input": ["text", "image", "video"], "output": ["text"]},
        "open_weights": true,
        "cost": {"input": 0.3, "output": 1.2, "cache_read": 0.006, "tiers": [{"x": 1}]},
        "limit": {"context": 1000000, "output": 131072},
        "provider": {"api": "https://opencode.ai/inference/go/openai/v1"},
        "canonical_model_id": "zhipuai/glm-5.3-flash"
      },
      "hy3": {"id": "hy3", "interleaved": true, "status": "beta",
        "modalities": {"input": ["text"], "output": ["text"]},
        "limit": {"context": 256000, "input": 200000, "output": 32000}}
    }
  }
}`

func serve(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != userAgent {
			t.Errorf("User-Agent = %q", ua)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetch(t *testing.T) {
	cards, err := Fetch(context.Background(), nil, serve(t, http.StatusOK, catalog), "opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(cards))
	}
	want := Card{
		ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", Family: "glm-flash", CanonicalID: "zhipuai/glm-5.3-flash",
		Attachment: true, Reasoning: true, ToolCall: true, StructuredOutput: true, Temperature: true, OpenWeights: true,
		ReasoningOptions: []ReasoningOption{{Type: "effort", Values: []string{"low", "high", "max"}}},
		Modalities:       Modalities{Input: []string{"text", "image", "video"}, Output: []string{"text"}},
		Limit:            Limit{Context: 1000000, Output: 131072},
		Cost:             &Cost{Input: 0.3, Output: 1.2, CacheRead: 0.006},
		ReleaseDate:      "2026-08-26", LastUpdated: "2026-08-26",
		Endpoint: Endpoint{API: "https://opencode.ai/inference/go/openai/v1"},
	}
	if got := cards["glm-5.3-flash"]; !reflect.DeepEqual(got, want) {
		t.Errorf("card =\n%+v\nwant\n%+v", got, want)
	}

	hy := cards["hy3"]
	if hy.Cost != nil || hy.Status != "beta" || hy.Limit != (Limit{Context: 256000, Input: 200000, Output: 32000}) {
		t.Errorf("hy3 = %+v", hy)
	}
	if !cards["glm-5.3-flash"].Accepts("image") || hy.Accepts("image") {
		t.Error("Accepts(image) wrong")
	}
}

// Override：非零项覆盖，零值项沿用目录值；不改动原卡。
func TestOverride(t *testing.T) {
	c := Card{ID: "m", Limit: Limit{Context: 1000000, Output: 131072}}
	got := c.Override(Limit{Context: 200000})
	if got.Limit != (Limit{Context: 200000, Output: 131072}) {
		t.Errorf("Override(context) = %+v", got.Limit)
	}
	if got := c.Override(Limit{Output: 8192, Input: 1}); got.Limit != (Limit{Context: 1000000, Input: 1, Output: 8192}) {
		t.Errorf("Override(output,input) = %+v", got.Limit)
	}
	if got := c.Override(Limit{}); got.Limit != c.Limit {
		t.Errorf("Override(zero) = %+v", got.Limit)
	}
	if c.Limit != (Limit{Context: 1000000, Output: 131072}) {
		t.Errorf("original card modified: %+v", c.Limit)
	}
}

func TestFetchErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Fetch(ctx, nil, serve(t, http.StatusInternalServerError, catalog), "opencode-go"); err == nil {
		t.Error("want error on status 500")
	}
	if _, err := Fetch(ctx, nil, serve(t, http.StatusOK, catalog), "missing"); err == nil {
		t.Error("want error on missing provider")
	}
	if _, err := Fetch(ctx, nil, serve(t, http.StatusOK, "not json"), "opencode-go"); err == nil {
		t.Error("want error on bad JSON")
	}
}
