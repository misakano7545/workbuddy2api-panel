package upstream

import (
	"encoding/json"
	"testing"
)

func budgetObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return obj
}

func budgetOf(obj map[string]any) int64 {
	switch v := obj["max_tokens"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// TestEnsureMinOutputBudgetRaises 低额度的请求按档位抬到下限。
//
// 依据（2026-11-08 生产实测，cn:deepseek-v4.1-flash）：effort=high + max_tokens=2048
// 时上游把 2048 里的 2048 花在思考上、正文再占 2048 就 finish=length；同一提示词
// 不限额度时正文写到 13722 token 才自然收尾。所以 2048 这种额度是「装不下思考」的
// 组合错误，不是客户端真要的答案长度。
func TestEnsureMinOutputBudgetRaises(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		effort string
		want   int64
	}{
		{"high+2048", `{"model":"m","max_tokens":2048}`, "high", 8192},
		{"max+1024", `{"model":"m","max_tokens":1024}`, "max", 16384},
		{"low+512", `{"model":"m","max_tokens":512}`, "low", 4096},
		{"medium+2048", `{"model":"m","max_tokens":2048}`, "medium", 6144},
		{"xhigh+2048", `{"model":"m","max_tokens":2048}`, "xhigh", 12288},
		{"字符串额度+high", `{"model":"m","max_tokens":"2048"}`, "high", 8192},
		{"英文大写档位", `{"model":"m","max_tokens":2048}`, "HIGH", 8192},
		// 已经够大：不动。
		{"high+16384", `{"model":"m","max_tokens":16384}`, "high", 16384},
		{"high+8192(等于下限)", `{"model":"m","max_tokens":8192}`, "high", 8192},
	}
	for _, c := range cases {
		obj := budgetObj(t, c.body)
		ensureMinOutputBudget(obj, c.effort, nil)
		if got := budgetOf(obj); got != c.want {
			t.Errorf("%s: max_tokens=%d want %d", c.name, got, c.want)
		}
	}
}

// TestEnsureMinOutputBudgetLeavesAlone 不猜的四种情形：没给额度、探活级额度、
// 关闭思考、档位未知。
func TestEnsureMinOutputBudgetLeavesAlone(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		effort string
	}{
		{"没给额度（交给目录）", `{"model":"m"}`, "high"},
		{"探活 max_tokens=1", `{"model":"m","max_tokens":1}`, "max"},
		{"关闭思考", `{"model":"m","max_tokens":512,"thinking":{"type":"disabled"}}`, "high"},
		{"档位未知", `{"model":"m","max_tokens":512}`, ""},
		{"档位是 off", `{"model":"m","max_tokens":512}`, "off"},
	}
	for _, c := range cases {
		obj := budgetObj(t, c.body)
		before := budgetOf(obj)
		ensureMinOutputBudget(obj, c.effort, nil)
		if got := budgetOf(obj); got != before {
			t.Errorf("%s: max_tokens 被改成了 %d（want 原值 %d）", c.name, got, before)
		}
	}
}

// TestEnsureMinOutputBudgetRespectsCatalogCap 目录声明的输出上限更小时以目录为准：
// 抬到模型物理上写不了的值没有意义。
func TestEnsureMinOutputBudgetRespectsCatalogCap(t *testing.T) {
	obj := budgetObj(t, `{"model":"small-model","max_tokens":1024}`)
	ensureMinOutputBudget(obj, "max", map[string]int64{"small-model": 6000})
	if got := budgetOf(obj); got != 6000 {
		t.Fatalf("max_tokens=%d want 6000（目录上限封顶）", got)
	}
}

// TestPrepareBodyAppliesMinOutputBudget 端到端接线：真实管线里 max_tokens 被抬升，
// 且档位仍是客户端要的那一档（不因为抬额度改档位）。
func TestPrepareBodyAppliesMinOutputBudget(t *testing.T) {
	src := []byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"max","max_tokens":1024,
		"messages":[{"role":"user","content":"hi"}]}`)
	out, eff := PrepareBodyOptWithEffortPolicy(src, false,
		map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}},
		map[string]string{"deepseek-v4.1-flash": "high"},
		map[string]int64{"deepseek-v4.1-flash": 131072},
		"cn", &EffortPolicy{Enabled: true, DefaultEffort: "high", ClientPriority: true})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if got := budgetOf(obj); got != 16384 {
		t.Fatalf("max_tokens=%d want 16384（max 档下限）", got)
	}
	if eff != "max" {
		t.Fatalf("effective effort=%q want max（抬额度不得改档位）", eff)
	}
	if obj["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort=%v want max", obj["reasoning_effort"])
	}
}

// TestPrepareBodyKeepsLargeClientBudget 客户端给的额度已经够大时一个字节都不动。
func TestPrepareBodyKeepsLargeClientBudget(t *testing.T) {
	src := []byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"high","max_tokens":65536,
		"messages":[{"role":"user","content":"hi"}]}`)
	out, _ := PrepareBodyOptWithEffortPolicy(src, false,
		map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}},
		map[string]string{"deepseek-v4.1-flash": "high"},
		map[string]int64{"deepseek-v4.1-flash": 131072},
		"cn", &EffortPolicy{Enabled: true, DefaultEffort: "high", ClientPriority: true})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if got := budgetOf(obj); got != 65536 {
		t.Fatalf("max_tokens=%d want 65536（不动）", got)
	}
}
