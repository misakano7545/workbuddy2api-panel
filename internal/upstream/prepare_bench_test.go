package upstream

import (
	"encoding/json"
	"testing"
)

// benchAgenticBody 造一个「真实代理负载」形状的请求体：大上下文（≈170K token /
// 600KB 文本）+ 工具声明 + 长轮次历史。线上抓到的 /v1/responses 请求就是这个尺
// 寸（request-logs 实测 prompt_tokens 15.7 万~17.1 万、tools 8~63 个）。
//
// 用它量的是**网关自己的 CPU**（JSON 解析 + 各归一化步骤 + 脱敏 + 重新序列化），
// 与上游无关——所以基准里不发网络请求。
func benchAgenticBody(rounds, payloadPerMsg int) []byte {
	filler := make([]byte, payloadPerMsg)
	for i := range filler {
		filler[i] = byte('a' + i%26)
	}
	chunk := string(filler)

	msgs := []any{
		map[string]any{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude. " +
			"Use the read tool to inspect files. Tokens prefixed with @ are paths the user explicitly referenced. " +
			"Relative paths are resolved against the workspace root."},
	}
	for i := 0; i < rounds; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "please inspect module " + chunk[:payloadPerMsg/2]},
			map[string]any{"role": "assistant", "content": "reading now", "reasoning_content": chunk[:payloadPerMsg/4],
				"tool_calls": []any{map[string]any{
					"id": "call_" + chunk[:6], "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"internal/server/handler.go"}`},
				}}},
			map[string]any{"role": "tool", "tool_call_id": "call_" + chunk[:6], "content": chunk},
		)
	}
	tools := []any{}
	for _, name := range []string{"read_file", "write_file", "bash", "grep", "glob", "web_fetch", "todo_write", "task"} {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Tool " + name + " for the agent. Powered by Anthropic Claude. " + chunk[:200],
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string", "description": "path"}},
					"required":   []any{"path"},
				},
			},
		})
	}
	body := map[string]any{
		"model":            "cn:deepseek-v4.1-flash",
		"stream":           true,
		"max_tokens":       2048,
		"reasoning_effort": "xhigh",
		"messages":         msgs,
		"tools":            tools,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

// BenchmarkPrepareBodyAgentic 量「一次请求的请求体预处理」成本。
//
// 线上实测（2026-11-08，7863，170K token 上下文）：prep 中位 99ms、p99 673ms，
// 10 请求/秒时这一项就要吃掉约 1 个核。基准给的是同一量级的输入，改动造成
// 的放大/收敛都能在 ns/op 上直接看到。
func BenchmarkPrepareBodyAgentic(b *testing.B) {
	src := benchAgenticBody(30, 16*1024) // ≈30 轮 × 3 条 × 16KB ≈ 1.4MB/≈380K token
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := PrepareBodyOpt(src, true); len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

// BenchmarkPrepareBodyAgenticNoSanitize 同一份请求体但关掉脱敏层：
// 两者的差值就是「脱敏（正则 walks 全历史）」的净成本。
func BenchmarkPrepareBodyAgenticNoSanitize(b *testing.B) {
	src := benchAgenticBody(30, 16*1024)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := PrepareBodyOpt(src, false); len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

// BenchmarkPrepareBodyUnmarshalOnly 只算一次 JSON 解析 + 序列化，作为下界：
// 任何「比这个贵很多」的步骤都是可优化的对象。
func BenchmarkPrepareBodyUnmarshalOnly(b *testing.B) {
	src := benchAgenticBody(30, 16*1024)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var obj map[string]any
		if err := json.Unmarshal(src, &obj); err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(obj); err != nil {
			b.Fatal(err)
		}
	}
}
