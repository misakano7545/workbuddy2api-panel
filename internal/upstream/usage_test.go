package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeUsageCacheAliasesMirrorsNestedHit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            21041.0,
		"completion_tokens":        8.0,
		"total_tokens":             21049.0,
		"cache_read_input_tokens":  0.0,
		"cached_tokens":            0.0,
		"prompt_cache_hit_tokens":  0.0,
		"prompt_cache_miss_tokens": 177.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 20864.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	for _, key := range []string{
		"cache_read_input_tokens",
		"cached_tokens",
		"prompt_cache_hit_tokens",
	} {
		if got[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, got[key])
		}
	}
	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 20864.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 20864", details["cached_tokens"])
	}
}

func TestNormalizeUsageCacheAliasesPreservesZeroResult(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           35.0,
		"completion_tokens":       2.0,
		"total_tokens":            37.0,
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 0.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 0", details["cached_tokens"])
	}
}

func TestAggregateNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	if usage["cache_read_input_tokens"] != 20864.0 {
		t.Fatalf("cache_read_input_tokens=%v want 20864", usage["cache_read_input_tokens"])
	}
	if usage["cached_tokens"] != 20864.0 {
		t.Fatalf("cached_tokens=%v want 20864", usage["cached_tokens"])
	}
	if usage["prompt_cache_hit_tokens"] != 20864.0 {
		t.Fatalf("prompt_cache_hit_tokens=%v want 20864", usage["prompt_cache_hit_tokens"])
	}
}

func TestStreamNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	recorder := httptest.NewRecorder()
	if err := Stream(recorder, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}

	var usage map[string]any
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if raw, ok := frame["usage"].(map[string]any); ok && raw != nil {
			usage = raw
		}
	}
	if usage == nil {
		t.Fatal("stream response missing usage")
	}
	if usage["cache_read_input_tokens"] != 20864.0 {
		t.Fatalf("cache_read_input_tokens=%v want 20864", usage["cache_read_input_tokens"])
	}
	if usage["prompt_cache_hit_tokens"] != 20864.0 {
		t.Fatalf("prompt_cache_hit_tokens=%v want 20864", usage["prompt_cache_hit_tokens"])
	}
}
