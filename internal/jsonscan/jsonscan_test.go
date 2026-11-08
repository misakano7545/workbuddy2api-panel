package jsonscan

import (
	"encoding/json"
	"testing"
)

func TestTopLevelString(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","stream":true,"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"m-1"},
		"nested":{"model":"inner"},"esc":"a\"b\\c\n","n":null}`)
	cases := []struct {
		key, want string
		ok        bool
	}{
		{"model", "cn:deepseek-v4.1-flash", true},
		{"esc", "a\"b\\c\n", true},
		{"missing", "", false},
		{"stream", "", false},   // 非字符串
		{"n", "", false},        // null
		{"messages", "", false}, // 数组
	}
	for _, c := range cases {
		got, ok := TopLevelString(body, c.key)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("TopLevelString(%q) = %q,%v want %q,%v", c.key, got, ok, c.want, c.ok)
		}
	}
	// 嵌套对象里的同名键不能被顶层扫描到
	if got, ok := TopLevelString(body, "nested"); ok || got != "" {
		t.Errorf("不应把嵌套对象当字符串: %q %v", got, ok)
	}
}

func TestNestedString(t *testing.T) {
	body := []byte(`{"messages":[],"metadata":{"conversation_id":"m-1","user_id":"u-9","n":1}}`)
	if v, ok := NestedString(body, "metadata", "conversation_id"); !ok || v != "m-1" {
		t.Fatalf("got %q ok=%v", v, ok)
	}
	if v, ok := NestedString(body, "metadata", "user_id"); !ok || v != "u-9" {
		t.Fatalf("got %q ok=%v", v, ok)
	}
	if _, ok := NestedString(body, "metadata", "n"); ok {
		t.Fatal("数字不应算字符串")
	}
	if _, ok := NestedString(body, "absent", "x"); ok {
		t.Fatal("缺键不应命中")
	}
}

func TestFirstMessageRole(t *testing.T) {
	cases := []struct {
		body, want string
		ok         bool
	}{
		{`{"messages":[{"role":"system","content":"x"}]}`, "system", true},
		{`{"messages": [ {"content":[{"type":"text","text":"y"}],"role":"developer"} ]}`, "developer", true},
		{`{"messages":[]}`, "", false},
		{`{"messages":"nope"}`, "", false},
		{`{"messages":[{"content":"no role"}]}`, "", false},
		{`{"model":"m"}`, "", false},
		{`{}`, "", false},
		{``, "", false},
		{`{"messages":[{"role":1}]}`, "", false},
	}
	for _, c := range cases {
		got, ok := FirstMessageRole([]byte(c.body))
		if ok != c.ok || got != c.want {
			t.Errorf("FirstMessageRole(%s) = %q,%v want %q,%v", c.body, got, ok, c.want, c.ok)
		}
	}
}

// TestScanMatchesEncodingJSON 与整树解析对拍：真实形状的 body 上两者必须给出相同结果。
func TestScanMatchesEncodingJSON(t *testing.T) {
	bodies := []string{
		`{"model":"kimi-k2","tools":[{"function":{"parameters":{"$ref":"#/$defs/a"}}}],"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
		`{"messages":[{"role":"user","content":"含转义 \" 与 \\\\ 与 \u4e2d\u6587"}]}`,
		`{"conversation_id":"c-1","metadata":{"conversationId":"c-2","user_id":"u"},"messages":[{"role":"assistant"}]}`,
		`{"prompt_cache_key":"p-1","user_id":"u-1","messages":[{"role":"system"}]}`,
	}
	for _, b := range bodies {
		if err := Validate([]byte(b)); err != nil {
			t.Errorf("%v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(b), &obj); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		if msgs, ok := obj["messages"].([]any); ok && len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				want, _ := m["role"].(string)
				got, ok := FirstMessageRole([]byte(b))
				if want != "" && (!ok || got != want) {
					t.Errorf("role: got %q ok=%v want %q", got, ok, want)
				}
			}
		}
	}
}

func TestSkipValueDeepNesting(t *testing.T) {
	body := []byte(`{"a":{"b":[{"c":[[["x"]]]},2]},"model":"m"}`)
	if v, ok := TopLevelString(body, "model"); !ok || v != "m" {
		t.Fatalf("深层嵌套后应仍能读到 model: %q %v", v, ok)
	}
}

func FuzzTopLevelString(f *testing.F) {
	for _, s := range []string{
		`{"model":"m"}`, `{}`, `{"a":1`, `{"model":}}`, `[]`, ``, `{"model":"m\u0041"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got, ok := TopLevelString(b, "model")
		var obj map[string]any
		if err := json.Unmarshal(b, &obj); err != nil {
			return
		}
		want, wantOK := obj["model"].(string)
		if ok != wantOK || got != want {
			t.Fatalf("mismatch on %q: got %q,%v want %q,%v", b, got, ok, want, wantOK)
		}
	})
}
