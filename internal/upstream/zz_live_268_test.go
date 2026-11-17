package upstream

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbe268 真机确认 issue #268 的前提：请求级错误到底以**哪种形态**回来。
//
// 两条路径的处理完全不同：
//   - HTTP ≥400：Classify 判 kind，handler 立即回 400、不罚号（早已实现）；
//   - HTTP 200 + 首帧 error 帧：此前只会换号轮转，末端 503 no_healthy_account。
//
// PR #269 修的是后者。若真机上这类错误其实只走 HTTP ≥400，那么 PR 的价值就只是
// 「防一手」而不是「修线上现象」，回帖里必须说清楚。本探针用必然被拒的工具定义去打，
// 逐条打印状态码与响应首帧，判断实际走哪条路。
//
//	WB_PROBE_AUTHDIR=/path/to/auths WB_PROBE_UID=<uid> go test ./internal/upstream -run TestZZLiveProbe268 -v
func TestZZLiveProbe268(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := probe268Auth(t, dir, os.Getenv("WB_PROBE_UID"))
	c := New()
	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}
	fmt.Printf("账号 %s realm=%s\n", acct.UID, acct.Realm())

	// 这些工具定义在上游的强类型反序列化里没有合法解释：tools[].function.parameters
	// 必须是 object schema，类型别名 / 字符串 items / required 写成字符串都会被拒。
	cases := []struct{ name, params string }{
		{"parameters 是字符串", `"{}"`},
		{"parameters 是数组", `[]`},
		{"required 是字符串", `{"type":"object","properties":{"a":{"type":"string"}},"required":"a"}`},
		{"properties 是数组", `{"type":"object","properties":[]}`},
		{"type 是数字", `{"type":123,"properties":{}}`},
	}

	for _, tc := range cases {
		tools := fmt.Sprintf(`[{"type":"function","function":{"name":"f","parameters":%s}}]`, tc.params)
		body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"user","content":"hi"}],"tools":%s}`, tools)
		rc, status, respBody, err := c.ChatStream(acct, []byte(body), "127.0.0.1", ChatMeta{})

		first := strings.TrimSpace(string(respBody))
		if rc != nil {
			// 200 时体在流里：读首帧就够，读满会拖长整轮对话。
			br := bufio.NewReaderSize(rc, 1<<16)
			var sb strings.Builder
			for sb.Len() < 600 {
				line, rerr := br.ReadString('\n')
				sb.WriteString(line)
				if strings.Contains(sb.String(), "\"error\"") || strings.Contains(sb.String(), "[DONE]") {
					break
				}
				if rerr != nil {
					break
				}
			}
			rc.Close()
			first = strings.TrimSpace(sb.String())
		}
		path := "HTTP ≥400（Classify 路径）"
		if status == 200 {
			if strings.Contains(first, "\"error\"") {
				path = "HTTP 200 + 首帧 error 帧（#268 要修的那条）"
			} else {
				path = "HTTP 200 + 正常流（未被拒）"
			}
		}
		fmt.Printf("\n--- %s ---\n状态: %d  形态: %s\n首帧: %s\n",
			tc.name, status, path, truncate268(first, 300))
		if err != nil {
			fmt.Printf("err: %v\n", err)
		}
	}
}

func truncate268(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// probe268Auth 从 auth 目录里挑一个账号（文件名含 uid 片段）。
func probe268Auth(t *testing.T, dir, uid string) *auth.Auth {
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
			continue
		}
		a.FilePath = filepath.Join(dir, e.Name())
		return a
	}
	t.Fatalf("auth 目录 %s 里找不到账号 uid=%q", dir, uid)
	return nil
}

// TestZZLiveProbe268TooLong 真机确认「请求级错误」的实际传输形态：拿一个必然触发
// 11115（prompt too long）的超长请求去打，看上游是回报 HTTP 400（走 Classify 路径），
// 还是 HTTP 200 + 首帧 error 帧（走 #268 修的首帧否决路径）。
//
// 这一步很关键：PR #269 修的是后者，且 sse.go 的 headOpen 窗口一旦被上游自己的
// ": heartbeat" 注释行关掉，后续 error 帧就只会以 200 透出。所以要看清上游到底是
// 先 heartbeat 后报错，还是直接报错。
func TestZZLiveProbe268TooLong(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := probe268Auth(t, dir, os.Getenv("WB_PROBE_UID"))
	c := New()
	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}

	// 约 40 万字符 ≈ 十几万 token，超过窗口；内容用无意义字符避免撞内容策略。
	huge := strings.Repeat("abc ", 130000)
	body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[`+
		`{"role":"user","content":%q}]}`, huge)

	start := time.Now()
	rc, status, respBody, err := c.ChatStream(acct, []byte(body), "127.0.0.1", ChatMeta{})
	fmt.Printf("body=%d 字节  状态=%d  耗时=%s  err=%v\n", len(body), status, time.Since(start).Round(time.Millisecond), err)
	fmt.Printf("立即返回的 body（HTTP 阶段）: %s\n", truncate268(strings.TrimSpace(string(respBody)), 400))

	if rc != nil {
		br := bufio.NewReaderSize(rc, 1<<16)
		var sb strings.Builder
		for sb.Len() < 800 {
			line, rerr := br.ReadString('\n')
			sb.WriteString(line)
			if strings.Contains(sb.String(), "\"error\"") || strings.Contains(sb.String(), "[DONE]") || rerr != nil {
				break
			}
		}
		rc.Close()
		fmt.Printf("流首行（200 形态）: %s\n", truncate268(strings.TrimSpace(sb.String()), 500))
	}
}
