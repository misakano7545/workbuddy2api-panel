package upstream

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbe11129 真机测 11129「invalid function call parameters」到底由
// 哪些 schema 形态触发。只在设了 WB_PROBE_AUTHDIR 时有意义；本地 go test 会跳过。
//
//	GOOS=linux GOARCH=amd64 go test -c -o x.test ./internal/upstream
//	WB_PROBE_AUTHDIR=/opt/.../auths WB_PROBE_UID=... ./x.test -test.run TestZZLiveProbe11129 -test.v
//
// 每个变体只改 tools[].function.parameters，其余（模型、消息、流式）完全一致。
func TestZZLiveProbe11129(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	uid := os.Getenv("WB_PROBE_UID")
	var acct *auth.Auth
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 auth 目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// uid 由文件名承载（workbuddy-<uid>.json），auth.Parse 需要 FilePath 才补出 UID。
		if uid != "" && !strings.Contains(e.Name(), uid) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Logf("跳过 %s: %v", e.Name(), err)
			continue
		}
		a.FilePath = filepath.Join(dir, e.Name())
		acct = a
		break
	}
	if acct == nil {
		t.Fatalf("auth 目录里找不到账号 uid=%q", uid)
	}
	t.Logf("账号 %s realm=%s", acct.UID, acct.Realm())

	c := New()
	c.ChatBaseCN = "https://www.workbuddy.cn"
	c.BillingBaseCN = "https://www.workbuddy.cn"

	// round 1：原始畸形形态（哪些真的会 11129）
	round1 := []struct{ name, params string }{
		{"空对象 {}", `{}`},
		{"标准空 object", `{"type":"object","properties":{}}`},
		{"description 为 null", `{"type":"object","properties":{"a":{"type":"string","description":null}}}`},
		{"Python 类型别名 str", `{"type":"object","properties":{"a":{"type":"str"}}}`},
		{"required 是字符串", `{"type":"object","properties":{"a":{"type":"string"}},"required":"a"}`},
		{"tuple items 单元素", `{"type":"object","properties":{"a":{"type":"array","items":[{"type":"str"}]}}}`},
		{"tuple items 双元素", `{"type":"object","properties":{"a":{"type":"array","items":[{"type":"string"},{"type":"integer"}]}}}`},
		{"顶层 $ref", `{"$ref":"#/$defs/Foo"}`},
		{"无 parameters 键", ""},
	}
	// round 2：规整后的候选形状（能不能过）
	round2 := []struct{ name, params string }{
		{"null 剥除后", `{"type":"object","properties":{"a":{"type":"string"}}}`},
		{"别名 str→string", `{"type":"object","properties":{"a":{"type":"string"}}}`},
		{"required 转数组", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
		{"tuple 单元素解包", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`},
		{"tuple 双元素取首个", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`},
		{"tuple 双元素 anyOf", `{"type":"object","properties":{"a":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}}`},
		{"$ref 内联（带 $defs）", `{"type":"object","properties":{"x":{"type":"string"}},"$defs":{"Foo":{"type":"object","properties":{"x":{"type":"string"}}}}}`},
		{"$ref 丢弃", `{"type":"object","properties":{}}`},
		{"属性值为 null 已剥", `{"type":"object","properties":{}}`},
		{"required 空数组", `{"type":"object","properties":{"a":{"type":"string"}},"required":[]}`},
	}
	cases := round1
	if os.Getenv("WB_PROBE_ROUND") == "2" {
		cases = round2
	}
	for _, tc := range cases {
		var tools string
		if tc.params == "" {
			tools = `[{"type":"function","function":{"name":"f"}}]`
		} else {
			tools = fmt.Sprintf(`[{"type":"function","function":{"name":"f","parameters":%s}}]`, tc.params)
		}
		body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"system","content":"You are a helpful assistant."},`+
			`{"role":"user","content":"调用 f 工具，参数用空对象。"}],"tools":%s}`, tools)
		rc, status, respBody, err := c.ChatStream(acct, []byte(body), "127.0.0.1", ChatMeta{})
		if rc != nil {
			_, _ = io.Copy(io.Discard, rc)
			rc.Close()
		}
		code := ""
		if m := strings.Index(string(respBody), `"code":`); m >= 0 {
			end := m + 22
			if end > len(respBody) {
				end = len(respBody)
			}
			code = string(respBody[m:end])
		}
		fmt.Printf("RESULT %-24s status=%d %v %s\n", tc.name, status, err != nil, code)
	}
}
