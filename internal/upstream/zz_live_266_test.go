package upstream

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbe266 真机验证 issue #266 / PR #267 的取舍：规整不再碰数据关键字，
// 于是 `const: null`、`default: null`、`default` 里的对象、enum 里的对象这些**客户端
// 原始声明**会原样发到上游。要确认上游对它们的态度——如果上游 Go 强类型反序列化见到
// null 就 400，那「保留 null」在工程上等于把 11129 请回来，得改成「保留语义、去掉 null」
// 的折中写法。
//
// 每个变体只改 tools[].function.parameters，其余（模型、消息、流式）完全一致。
// 只在设了 WB_PROBE_AUTHDIR 时有意义；普通 go test 自动跳过。
//
//	GOOS=linux GOARCH=amd64 go test -c -o x.test ./internal/upstream
//	WB_PROBE_AUTHDIR=/opt/.../auths WB_PROBE_UID=<uid> ./x.test -test.run TestZZLiveProbe266 -test.v
func TestZZLiveProbe266(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := probe266Auth(t, dir, os.Getenv("WB_PROBE_UID"))
	c := New()
	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}
	fmt.Printf("账号 %s realm=%s\n", acct.UID, acct.Realm())

	cases := []struct{ name, params string }{
		// 基线：规整前后都一样的普通 schema，先确认链路本身通。
		{"基线 普通 schema", `{"type":"object","properties":{"a":{"type":"string"}}}`},

		// PR #267 之后会原样发出的形态（此前会被删/改）。
		{"const:null", `{"type":"object","properties":{"a":{"const":null}}}`},
		{"default:null", `{"type":"object","properties":{"a":{"type":["string","null"],"default":null}}}`},
		{"default 里带畸形对象", `{"type":"object","properties":{"m":{"type":"object","default":{"type":"str","items":["a"],"required":"x","note":null}}}}`},
		{"enum 里带对象", `{"type":"object","properties":{"s":{"enum":[{"type":"list","items":[1]}]}}}`},
		{"examples 里带 null", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"},"examples":[{"required":[null,""]}]}}}`},
		{"dependentRequired 名为 items", `{"type":"object","properties":{"items":{"type":"string"},"type":{"type":"string"}},"dependentRequired":{"items":["type"]}}`},
		{"dependencies 子 schema", `{"type":"object","properties":{"z":{"type":"string"}},"dependencies":{"z":{"type":"str"}}}`},

		// 对照：旧的删 null 行为会不会真的是必需（description:null 是 11129 已知形态）。
		{"description:null（对照）", `{"type":"object","properties":{"a":{"type":"string","description":null}}}`},
	}

	for _, tc := range cases {
		tools := fmt.Sprintf(`[{"type":"function","function":{"name":"f","parameters":%s}}]`, tc.params)
		body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"user","content":"只回复 OK，不要调用工具。"}],"tools":%s}`, tools)
		rc, status, respBody, err := c.ChatStream(acct, []byte(body), "127.0.0.1", ChatMeta{})
		var n int64
		if rc != nil {
			n, _ = io.Copy(io.Discard, rc)
			rc.Close()
		}
		code := ""
		if m := strings.Index(string(respBody), `"code":`); m >= 0 {
			end := m + 24
			if end > len(respBody) {
				end = len(respBody)
			}
			code = strings.TrimSpace(strings.ReplaceAll(string(respBody[m:end]), "\n", " "))
		}
		fmt.Printf("RESULT %-28s status=%d bytes=%-6d err=%-5v %s\n", tc.name, status, n, err != nil, code)
	}
}

// probe266Auth 从 auth 目录里挑一个账号（文件名含 uid 片段）。自带一份而不复用
// zz_live_257_test.go 的同名 helper：本探针要能单独拷到任意分支上跑。
func probe266Auth(t *testing.T, dir, uid string) *auth.Auth {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 auth 目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
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
		return a
	}
	t.Fatalf("auth 目录 %s 里找不到账号 uid=%q", dir, uid)
	return nil
}
