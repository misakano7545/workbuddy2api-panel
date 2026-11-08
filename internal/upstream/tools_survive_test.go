package upstream

import (
	"encoding/json"
	"testing"
)

// TestToolsSurviveForClientShapes 检查各种 tool_choice / tools 形态出站后工具还在不在。
//
// 背景：线上 31% 的「声明了 tools」请求最后 finish_reason=stop 且零 tool_calls，
// 其中 83% 正文不足 20 token —— 用户看到的就是「模型说一句『现在我来改…』然后停了」。
// 最可疑的网关侧成因是工具被 normalizeToolChoice 整段删掉（客户端说不了话就只能干讲）。
func TestToolsSurviveForClientShapes(t *testing.T) {
	tools := `[{"type":"function","name":"read_file","description":"读文件","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
	          {"type":"function","name":"run_command","description":"跑命令","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]`
	cases := []struct {
		name       string
		toolChoice string // 原样塞进 body 的 tool_choice 片段；空 = 不带该字段
		wantTools  bool
		why        string
	}{
		{"无 tool_choice", "", true, "客户端没说，工具必须原样出站"},
		{"auto 字符串", `"auto"`, true, "最常见的自动选择"},
		{"required 字符串", `"required"`, true, "强制调用"},
		{"none 字符串", `"none"`, false, "客户端明确禁用工具 → 删除 tools（既有契约）"},
		{"{type:auto}", `{"type":"auto"}`, true, "对象形态的 auto"},
		{"{type:required}", `{"type":"required"}`, true, "对象形态的 required"},
		{"{type:none}", `{"type":"none"}`, false, "对象形态的 none → 删除 tools"},
		{"chat 形态指定函数", `{"type":"function","function":{"name":"read_file"}}`, true, "指定单个函数仍需工具定义"},
		{"responses 形态指定函数", `{"type":"function","name":"read_file"}`, true, "Responses API 的函数选择形态"},
		{"未知对象", `{"type":"weird"}`, true, "认不出就只删 tool_choice，工具保留"},
	}
	for _, c := range cases {
		body := `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}],"tools":` + tools
		if c.toolChoice != "" {
			body += `,"tool_choice":` + c.toolChoice
		}
		body += `}`
		out, _ := PrepareBodyOptWithEffortPolicy([]byte(body), false, nil, nil, nil, "cn", nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("%s: 出站 body 非法: %v", c.name, err)
		}
		gotTools := obj["tools"] != nil
		if gotTools != c.wantTools {
			t.Errorf("%s: tools 存活=%v want %v（%s）\n  出站: %s", c.name, gotTools, c.wantTools, c.why, truncateForTest(out))
		}
	}
}

func truncateForTest(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
