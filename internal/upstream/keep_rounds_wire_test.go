package upstream

import (
	"encoding/json"
	"testing"
)

// TestClientKeepRecentRoundsReachesPipeline KeepRecentRounds 必须真的从 Client
// 走到出站 body（issue #99）：只测 trimHistory 而不测接线，开关写了不生效也照样绿。
func TestClientKeepRecentRoundsReachesPipeline(t *testing.T) {
	c := New()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
		{"role":"system","content":"s"},
		{"role":"user","content":"一"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"二"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"三"}]}`)

	// 关闭时全量透传（默认行为，与没有这个功能一致）。
	out, _, _ := c.prepareBody(body, "cn", "uid", "conv", "")
	if n := countMessages(t, out); n != 6 {
		t.Fatalf("keep_recent_rounds=0 不该裁历史，实际 %d 条", n)
	}

	c.KeepRecentRounds.Store(2)
	out, _, _ = c.prepareBody(body, "cn", "uid", "conv", "")
	roles := messageRoles(t, out)
	if got := len(roles); got != 4 {
		t.Fatalf("保留 2 轮应为 system+user+assistant+user 共 4 条，实际 %d 条: %v", got, roles)
	}
	if roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("裁完必须 system 开头、user 接续，实际 %v", roles)
	}
}

func countMessages(t *testing.T, body []byte) int {
	t.Helper()
	return len(messageRoles(t, body))
}

func messageRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	out := make([]string, 0, len(obj.Messages))
	for _, m := range obj.Messages {
		out = append(out, m.Role)
	}
	return out
}
