package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

// 档位不在模型支持列表里时取「最近档」，不再一律降级（issue #175）：Codex 等客户端
// 默认发 medium，而 CN 模型常见档位是 low/high/max（无 medium）——旧逻辑取「≤ medium
// 的最高支持档」会降到 low，比**不携带该字段**时的模型默认档 high 还弱且客户端无感。
// 同距时优先模型声明的默认档，其次偏高的一档。返回值即日志行里显示的生效档位。
// 上游 PR #159：chat content 里的思考片段（reasoning_text / reasoning）必须剔除——
// 上游 content 类型白名单不含它，命中即 400 code=11101（第三方 responses→chat 转换器
// 会把助手历史里的思考片段原样透传进来）。
func TestDropReasoningContentParts(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"reasoning_text 剥离、text 保留",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`},
		{"reasoning 别名同样剥离",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"think"},{"type":"output_text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"output_text","text":"ans"}]}]`},
		{"type 前后空白同样命中",
			`{"messages":[{"role":"assistant","content":[{"type":" reasoning_text ","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`},
		{"剥空且无 tool_calls：整条消息删除",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"}]},{"role":"user","content":"hi"}]}`,
			`[{"role":"user","content":"hi"}]`},
		{"剥空但有 tool_calls：保留空数组 content（不破坏配对）",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"t"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			`[{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]`},
		{"字符串 content 不受影响",
			`{"messages":[{"role":"assistant","content":"plain"},{"role":"user","content":"hi"}]}`,
			`[{"role":"assistant","content":"plain"},{"role":"user","content":"hi"}]`},
		{"非对象片段原样保留",
			`{"messages":[{"role":"assistant","content":["raw",{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":["raw",{"type":"text","text":"ans"}]}]`},
		{"messages 缺失", `{}`, `null`},
		{"messages 非数组", `{"messages":"oops"}`, `"oops"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var obj map[string]any
			if err := json.Unmarshal([]byte(c.body), &obj); err != nil {
				t.Fatal(err)
			}
			dropReasoningContentParts(obj) // 不得 panic
			got, err := json.Marshal(obj["messages"])
			if err != nil {
				t.Fatal(err)
			}
			// 期望值也过一遍序列化：Go 的 map 按键排序输出，直接比字符串会被键序绊住。
			var want any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			wantOut, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(wantOut) {
				t.Errorf("messages=%s want %s", got, wantOut)
			}
		})
	}
}

// 管线必须挂上剥离，且与 sanitize 开关无关（同 normalizeRoles / 工具配对的口径）。
func TestPrepareBodyDropsReasoningTextParts(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]}]}`)
	for _, sanitize := range []bool{false, true} {
		out := PrepareBodyOpt(body, sanitize)
		if strings.Contains(string(out), "reasoning_text") {
			t.Errorf("sanitize=%v 时管线未剥离思考片段: %s", sanitize, out)
		}
		if !strings.Contains(string(out), `"ans"`) {
			t.Errorf("sanitize=%v 时正文被误删: %s", sanitize, out)
		}
	}
}

func TestPrepareBodyOptNormalizeStop(t *testing.T) {
	cases := []struct {
		name, in, wantStop string // wantStop 为期望的 stop 字段 JSON 片段
	}{
		{"字符串形态转数组", `{"model":"m","stop":"\n\nHuman:"}`, `"stop":["\n\nHuman:"]`},
		// 前后空白是停止词语义本身，不得 TrimSpace（首版裁剪被此用例抓到）。
		{"保留空白", `{"model":"m","stop":" END "}`, `"stop":[" END "]`},
		{"空串保留", `{"model":"m","stop":""}`, `"stop":[""]`},
		{"数组形态原样", `{"model":"m","stop":["a","b"]}`, `"stop":["a","b"]`},
	}
	for _, c := range cases {
		out := string(PrepareBodyOpt([]byte(c.in), false))
		if !strings.Contains(out, c.wantStop) {
			t.Errorf("%s: out=%s want 含 %s", c.name, out, c.wantStop)
		}
	}
	// null / 字段缺失不动：null 保持 null（不替客户端造值）。
	if out := string(PrepareBodyOpt([]byte(`{"model":"m","stop":null}`), false)); !strings.Contains(out, `"stop":null`) {
		t.Errorf("null 形态应原样保留: %s", out)
	}
	if out := string(PrepareBodyOpt([]byte(`{"model":"m"}`), false)); strings.Contains(out, "stop") {
		t.Errorf("缺失 stop 不应被补出: %s", out)
	}
}

func TestPrepareBodyOptEffortNearestPick(t *testing.T) {
	efforts := map[string][]string{
		"deepseek-v4.1-flash": {"low", "high", "max"}, // 无 medium（线上实测的真实形状）
		"tie-default-low":     {"low", "high"},        // medium 与 low/high 同距，默认档 low
		"tie-no-default":      {"low", "high"},        // 同距且模型未声明默认档 → 宁高
		"gap-only":            {"low", "medium"},
	}
	defaults := map[string]string{
		"deepseek-v4.1-flash": "high",
		"tie-default-low":     "low",
		"gap-only":            "high",
	}
	cases := []struct{ name, body, want string }{
		{"无 medium 时取最近档 high（不再降到 low）",
			`{"model":"deepseek-v4.1-flash","reasoning_effort":"medium"}`, "high"},
		{"同距优先模型默认档",
			`{"model":"tie-default-low","reasoning_effort":"medium"}`, "low"},
		{"同距且无默认档 → 宁高",
			`{"model":"tie-no-default","reasoning_effort":"medium"}`, "high"},
		{"xhigh 取最近档 medium",
			`{"model":"gap-only","reasoning_effort":"xhigh"}`, "medium"},
		{"高于最高档取最高支持档",
			`{"model":"gap-only","reasoning_effort":"max"}`, "medium"},
		{"低于最低档取最低支持档",
			`{"model":"deepseek-v4.1-flash","reasoning_effort":"minimal"}`, "low"},
		{"支持则原样透传",
			`{"model":"deepseek-v4.1-flash","reasoning_effort":"max"}`, "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, eff := PrepareBodyOptWithEffortsDefaultAndCaps([]byte(c.body), false, efforts, defaults, nil)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if got := m["reasoning_effort"]; got != c.want {
				t.Errorf("reasoning_effort=%v want %v", got, c.want)
			}
			if eff != c.want {
				t.Errorf("生效档位回传=%q want %q（日志行显示的就是它）", eff, c.want)
			}
		})
	}
}

func TestPrepareBodyOptRequestedVsEffectiveEffort(t *testing.T) {
	// issue #43 / #18：日志里「客户端设了 max、显示却是 high」有两种完全不同的成因，
	// 必须能靠一次日志分辨：客户端根本没发（网关补的默认档），还是发了被降级
	// （模型档位表里没有这一档）。requested 在 injectThinking 之前抓，就是为此。
	onlyHigh := map[string][]string{"deepseek-v4.1-flash": {"high"}}
	full := map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}
	defaults := map[string]string{"deepseek-v4.1-flash": "high"}

	cases := []struct {
		name             string
		body             string
		efforts          map[string][]string
		wantReq, wantEff string
	}{
		{
			name:    "客户端发 max 且模型支持 → 原样透传，日志 max->不显示降级",
			body:    `{"model":"deepseek-v4.1-flash","reasoning_effort":"max"}`,
			efforts: full,
			wantReq: "max", wantEff: "max",
		},
		{
			name:    "客户端发 max 但只支持 high → 降级可见（max->high）",
			body:    `{"model":"deepseek-v4.1-flash","reasoning_effort":"max"}`,
			efforts: onlyHigh,
			wantReq: "max", wantEff: "high",
		},
		{
			name:    "客户端没发 → requested 为空，日志不会误报成降级",
			body:    `{"model":"deepseek-v4.1-flash"}`,
			efforts: onlyHigh,
			wantReq: "", wantEff: "high",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, req, eff := prepareBodyOpt([]byte(c.body), false, true, c.efforts, defaults, nil, "", nil, "", "", false, 0, "")
			if req != c.wantReq {
				t.Errorf("requested=%q want %q（客户端原始档位）", req, c.wantReq)
			}
			if eff != c.wantEff {
				t.Errorf("effective=%q want %q（出站档位）", eff, c.wantEff)
			}
		})
	}
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestInjectCatalogMaxTokens 缺失输出限额时按目录动态值补 max_tokens：
// 上游对不带 max_tokens 的请求套保守默认 32000（Codex 实测被无声截断），
// 目录声明的 maxOutputTokens 才是真实预算；显式限额（含别名翻译产物）优先。
func TestInjectCatalogMaxTokens(t *testing.T) {
	maxOut := map[string]int64{"deepseek-v4.1-flash": 393216}

	assertMaxTokens := func(t *testing.T, body []byte, want any, absent bool) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal: %v (body=%s)", err, body)
		}
		got, has := m["max_tokens"]
		if absent {
			if has {
				t.Fatalf("max_tokens 不该存在, got %v", got)
			}
			return
		}
		if !has || got != want {
			t.Fatalf("max_tokens=%v (%T) want %v", got, got, want)
		}
	}

	// 无限额 + 目录命中 → 补上（int64 落 JSON 后按 float64 读回）。
	body, _ := PrepareBodyOptWithEffortsDefaultAndCaps([]byte(`{"model":"deepseek-v4.1-flash","messages":[]}`), false, nil, nil, maxOut)
	assertMaxTokens(t, body, float64(393216), false)

	// 显式 max_tokens 优先：不覆盖。
	body, _ = PrepareBodyOptWithEffortsDefaultAndCaps([]byte(`{"model":"deepseek-v4.1-flash","max_tokens":123,"messages":[]}`), false, nil, nil, maxOut)
	assertMaxTokens(t, body, float64(123), false)

	// 别名 max_completion_tokens 翻译后同样算显式限额，不被目录值覆盖。
	body, _ = PrepareBodyOptWithEffortsDefaultAndCaps([]byte(`{"model":"deepseek-v4.1-flash","max_completion_tokens":456,"messages":[]}`), false, nil, nil, maxOut)
	assertMaxTokens(t, body, float64(456), false)

	// 模型未知 → 不补。
	body, _ = PrepareBodyOptWithEffortsDefaultAndCaps([]byte(`{"model":"unknown","messages":[]}`), false, nil, nil, maxOut)
	assertMaxTokens(t, body, nil, true)

	// 快照 nil → 不补（旧行为完全一致）。
	body, _ = PrepareBodyOptWithEffortsDefaultAndCaps([]byte(`{"model":"deepseek-v4.1-flash","messages":[]}`), false, nil, nil, nil)
	assertMaxTokens(t, body, nil, true)
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte
		for round := 0; round < 5; round++ {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}
			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	lookupPattern := func(t *testing.T, body string, path ...any) string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		cur := any(obj)
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		s, _ := cur.(string)
		return s
	}

	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		bs := string(byte(92))                                                                                                                                                                                                                                                                                                                // 反斜杠，测试体经工具链多层转义易被吞，运行时拼装保真
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + bs + bs + `_run` + bs + bs + `_"}` + `,"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
			t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
		}
		// 同 schema 的兄弟字段不受影响
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
			t.Fatalf("sibling field disturbed: %q", got)
		}
	})

	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + bs + bs + `_y"}}},{"type":"string"}]}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
			t.Fatalf("nested pattern = %q, want %q", got, "x_y")
		}
	})

	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + bs + bs + `_q"}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
			t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
		}
	})

	t.Run("patternProperties key normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + bs + bs + `_b` + bs + bs + `_":{"type":"string"}}}}}]}` //nolint:lll
		var obj map[string]any
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		props := tool["patternProperties"].(map[string]any)
		if _, ok := props["^a_b_"]; !ok {
			t.Fatalf("normalized key missing: %v", props)
		}
		for k := range props {
			if strings.Contains(k, string(byte(92))) {
				t.Fatalf("patternProperties key not normalized: %q", k)
			}
		}
	})

	t.Run("message content backslash untouched", func(t *testing.T) {
		bs := string(byte(92))
		raw := `{"messages":[{"role":"user","content":"path C:` + bs + bs + `_dir and regex a` + bs + bs + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + bs + bs + `_r"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		msg := obj["messages"].([]any)[0].(map[string]any)
		if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
			t.Fatalf("message content mutated: %q", got)
		}
	})

	t.Run("clean pattern untouched", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + bs + bs + `d_$"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
			t.Fatalf("clean pattern mutated: %q", got)
		}
	})

	t.Run("no tools is no-op", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"model":"m","messages":[{"role":"user","content":"a` + bs + bs + `_b"}]}`
		var inObj, outObj map[string]any
		if err := json.Unmarshal([]byte(body), &inObj); err != nil {
			t.Fatal(err)
		}
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &outObj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		inMsg, _ := json.Marshal(inObj["messages"])
		outMsg, _ := json.Marshal(outObj["messages"])
		if string(inMsg) != string(outMsg) {
			t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
		}
		if _, has := outObj["tools"]; has {
			t.Fatal("tools appeared out of nowhere")
		}
	})

	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + bs + bs + `_b"}}}}}]}` //nolint:lll
		for _, sanitize := range []bool{false, true} {
			out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
			if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
				t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
			}
		}
	})
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200，吸收上游 fd3e142）。
// Claude Code 切模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。
// 非 GPT 模型与未携带字段一律不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"gpt zero clamped like unset", `{"model":"gpt-6-sol","max_tokens":0,"messages":[]}`, float64(16)},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("max_tokens = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitRealmPrefix(t *testing.T) {
	cases := []struct {
		in, realm, bare string
	}{
		{"cn:hy3", "cn", "hy3"},
		{"global:gpt-x", "global", "gpt-x"},
		{"hy3", "cn", "hy3"},
		{"weird:model", "cn", "weird:model"},
		{"cn:", "cn", ""},
		{":", "cn", ":"},
		{"", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := SplitRealmPrefix(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("SplitRealmPrefix(%q) = (%q,%q), want (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}

func TestPrepareBodyStripsRealmPrefix(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"cn prefix", `{"model":"cn:hy3","messages":[]}`, "hy3"},
		{"global prefix", `{"model":"global:gpt-x","messages":[]}`, "gpt-x"},
		{"bare name", `{"model":"hy3","messages":[]}`, "hy3"},
		{"unknown prefix", `{"model":"weird:model","messages":[]}`, "weird:model"},
		{"missing model", `{"messages":[]}`, ""},
		{"non-string model", `{"model":123,"messages":[]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(c.body), false)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, _ := obj["model"].(string)
			if got != c.want {
				t.Fatalf("model = %q, want %q (body=%s)", got, c.want, out)
			}
		})
	}
}

// 归一化必须发生在 catalog/efforts 查表之前：带前缀的 model 也要命中裸名条目。
func TestPrepareBodyPrefixedModelUsesBareForCatalogAndEffort(t *testing.T) {
	body := []byte(`{"model":"cn:hy3","reasoning_effort":"max","messages":[]}`)
	out, effort := PrepareBodyOptWithEffortsDefaultAndCaps(body, false,
		map[string][]string{"hy3": {"low", "high"}},
		map[string]string{"hy3": "high"},
		map[string]int64{"hy3": 123},
	)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["model"] != "hy3" {
		t.Fatalf("model = %v, want hy3", obj["model"])
	}
	if obj["max_tokens"] != float64(123) {
		t.Fatalf("max_tokens = %v, want 123 (catalog lookup used prefixed model?)", obj["max_tokens"])
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high (efforts lookup used prefixed model?)", obj["reasoning_effort"])
	}
	if effort != "high" {
		t.Fatalf("effort = %q, want high", effort)
	}
}
