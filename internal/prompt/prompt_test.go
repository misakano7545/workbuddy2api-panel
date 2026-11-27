package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemRoles 提取 messages 中所有 role 值，用于断言改写后的角色序列。
func systemRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg not map: %v", m)
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

func TestRewriteReplacesSystemAndDeveloper(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"被替换的旧提示词"},
			{"role":"developer","content":"开发者指令"},
			{"role":"user","content":"你好"}
		],
		"metadata":{"conversation_id":"c1"}
	}`)
	out := Rewrite(in, "我是自有提示词")
	roles := systemRoles(t, out)
	wantRoles := []string{"system", "user"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("roles=%v want %v", roles, wantRoles)
	}
	for i, r := range roles {
		if r != wantRoles[i] {
			t.Fatalf("roles[%d]=%q want %q (all=%v)", i, r, wantRoles[i], roles)
		}
	}
	// 头部 system 内容恰为自有提示词，旧 system/developer 内容零残留。
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["content"] != "我是自有提示词" {
		t.Errorf("first system content=%v", first["content"])
	}
	if strings.Contains(string(out), "被替换的旧提示词") || strings.Contains(string(out), "开发者指令") {
		t.Errorf("old system/developer content leaked: %s", out)
	}
}

func TestRewriteKeepsUserAssistantToolUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"user","content":"u-content"},
			{"role":"assistant","content":"a-content"},
			{"role":"tool","tool_call_id":"t1","content":"tool-result"}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	// 预期：system(新) + user + assistant + tool，顺序保留。
	if len(msgs) != 4 {
		t.Fatalf("len=%d", len(msgs))
	}
	roles := systemRoles(t, out)
	want := []string{"system", "user", "assistant", "tool"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	// user/assistant/tool 字段逐字不动。
	userMsg := msgs[1].(map[string]any)
	if userMsg["content"] != "u-content" {
		t.Errorf("user content changed: %v", userMsg["content"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool changed: %v", toolMsg)
	}
}

func TestRewriteKeepsMetadataAndOtherFields(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
}

func TestRewriteInjectsSystemWhenAbsent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := Rewrite(in, "SYS")
	roles := systemRoles(t, out)
	want := []string{"system", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	for i, r := range roles {
		if r != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
}

func TestRewriteMultimodalContentUntouched(t *testing.T) {
	// user content 为多模态数组（text + image_url），Rewrite 只动 messages 层级，
	// 不应改动 content 内部结构。
	in := []byte(`{
		"messages":[
			{"role":"system","content":"old"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len=%d", len(msgs))
	}
	userMsg := msgs[1].(map[string]any)
	arr, ok := userMsg["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multimodal content changed: %v", userMsg["content"])
	}
	textPart := arr[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "看图" {
		t.Errorf("text part changed: %v", textPart)
	}
}

func TestRewriteInvalidJSONReturnedAsIs(t *testing.T) {
	in := []byte(`{not valid json`)
	out := Rewrite(in, "SYS")
	if string(out) != string(in) {
		t.Errorf("invalid json should return as-is: got %s", out)
	}
}

func TestRewriteEmptyBodyReturnedAsIs(t *testing.T) {
	out := Rewrite([]byte{}, "SYS")
	if len(out) != 0 {
		t.Errorf("empty body should return as-is: got %s", out)
	}
}

func TestRewriteEmptyPromptReturnedAsIs(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	out := Rewrite(in, "")
	// systemPrompt 空 → 不改写，原样返回。
	if string(out) != string(in) {
		t.Errorf("empty prompt should return as-is: got %s", out)
	}
}

func TestLoadDefaultWhenFileEmpty(t *testing.T) {
	got, err := Load("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPrompt {
		t.Errorf("Load() returned non-default prompt (len=%d vs %d)", len(got), len(defaultPrompt))
	}
	if len(got) == 0 {
		t.Error("default prompt is empty")
	}
}

func TestLoadFileOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "my.md")
	want := "这是我的自定义人格入口。"
	os.WriteFile(fp, []byte(want), 0o600)
	got, err := Load("custom", fp)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Load()=%q want %q", got, want)
	}
}

func TestLoadFileMissingFailsFast(t *testing.T) {
	if _, err := Load("custom", "/nonexistent/promp.md"); err == nil {
		t.Fatal("missing file should return error (fail fast)")
	}
}

// system 后置：头部的 system 块整体挪到尾部，头部换固定桩。
// 断言的是「前缀可复用的结构」而不是文案：两轮请求的 messages 前 N 条必须逐字相同，
// 这正是缓存命中的前提（实测 99.3% vs 头部变化 0%）。
func TestSystemToTailMovesLeadBlock(t *testing.T) {
	turn1 := `{"model":"cn:x","messages":[` +
		`{"role":"system","content":"你是助手。cwd=/a，git clean，10:00"},` +
		`{"role":"user","content":"记住：做缓存测试"}]}`
	got, ok := SystemToTail([]byte(turn1))
	if !ok {
		t.Fatal("应改写")
	}
	if roles := systemRoles(t, got); strings.Join(roles, ",") != "system,user,system" {
		t.Fatalf("角色序列应为 system,user,system，实际 %v", roles)
	}
	// 原文一字不丢，且落在最后一条。
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("坏 JSON: %v", err)
	}
	if obj.Messages[0].Content != SystemTailStub {
		t.Fatalf("头部应是固定桩，实际 %q", obj.Messages[0].Content)
	}
	if !strings.Contains(obj.Messages[2].Content, "cwd=/a") {
		t.Fatalf("原文应原样落到尾部，实际 %q", obj.Messages[2].Content)
	}

	// 第二轮：历史追加一轮、system 换了内容 —— 前缀（桩 + 历史）必须仍逐字相同。
	turn2 := `{"model":"cn:x","messages":[` +
		`{"role":"system","content":"你是助手。cwd=/a，git 2 files changed，10:01"},` +
		`{"role":"user","content":"记住：做缓存测试"},` +
		`{"role":"assistant","content":"好的"},` +
		`{"role":"user","content":"继续"}]}`
	got2, ok := SystemToTail([]byte(turn2))
	if !ok {
		t.Fatal("第二轮也应改写")
	}
	var obj2 struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got2, &obj2); err != nil {
		t.Fatalf("坏 JSON: %v", err)
	}
	for i := 0; i < 2; i++ {
		if obj.Messages[i].Content != obj2.Messages[i].Content {
			t.Fatalf("第 %d 条应逐字相同（缓存前缀），%q vs %q", i, obj.Messages[i].Content, obj2.Messages[i].Content)
		}
	}
}

// 结构不安全时一律不动：没有前导 system、只有 system 一条、坏 JSON。
func TestSystemToTailGuards(t *testing.T) {
	cases := map[string]string{
		"首条不是 system": `{"messages":[{"role":"user","content":"hi"}]}`,
		"只有一条 system": `{"messages":[{"role":"system","content":"s"}]}`,
		"没有 messages":  `{"model":"cn:x"}`,
		"坏 JSON":       `{`,
	}
	for name, body := range cases {
		if _, ok := SystemToTail([]byte(body)); ok {
			t.Fatalf("%s：不该改写", name)
		}
	}
}
