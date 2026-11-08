package upstream

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/misakano7545/workbuddy2api-panel/internal/jsonscan"
)

// kimiToolsParses prepareKimiTools 做整树解析的次数（观测/回归用）。
// 非 kimi 模型必须 0 次：那一路只需读一个 model 字段。
var kimiToolsParses atomic.Int64

// KimiToolsParseCount 返回 prepareKimiTools 累计的整树解析次数（供测试）。
func KimiToolsParseCount() int64 { return kimiToolsParses.Load() }

// ResetKimiToolsParseCount 归零计数（供测试）。
func ResetKimiToolsParseCount() { kimiToolsParses.Store(0) }

// prepareKimiTools 把 CN Kimi 出站请求里工具 schema 的本地 $ref 展开成内联定义
// （Kimi 不接受 parameters 里的 $ref/#/$defs），其余模型、其余域原样返回字节。
//
// 只做 $ref 展开，**不裁剪任何工具**：本网关是通用中转，按工具名前缀裁剪是把某个
// 部署的绕行手段写进了共享路径，其它用户的 MCP 工具会静默消失。
func prepareKimiTools(body []byte, realm string) []byte {
	if realmKey(realm) != "cn" {
		return body
	}
	// 快路径：先用零分配的扫描读 model；非 kimi 直接返回，不做整树解析。
	// 线上绝大多数请求走这里——此前每个 CN 请求都要为了一句前缀判断把整个
	// body 解析成 map[string]json.RawMessage（100~500KB，全部值都复制一遍）。
	model, ok := jsonscan.TopLevelString(body, "model")
	if !ok || !strings.HasPrefix(strings.TrimPrefix(model, "cn:"), "kimi") {
		return body
	}
	kimiToolsParses.Add(1)
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	var tools []map[string]any
	if json.Unmarshal(obj["tools"], &tools) != nil || len(tools) == 0 {
		return body
	}
	kept := make([]map[string]any, 0, len(tools))
	budget := 10000
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			// Preserve non-function tools rather than converting their protocol.
			kept = append(kept, tool)
			continue
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			defs, _ := params["$defs"].(map[string]any)
			fn["parameters"] = inlineKimiSchemaBudget(params, defs, map[string]bool{}, 0, &budget)
			if budget <= 0 {
				return body
			}
		}
		kept = append(kept, tool)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	obj["tools"] = encoded
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// Cyclic schemas cannot be inlined; retain the local proxy's empty-schema
// fallback. Depth is bounded so an adversarial schema cannot exhaust the stack.
func inlineKimiSchema(node any, defs map[string]any, seen map[string]bool, depth int) any {
	budget := 10000
	return inlineKimiSchemaBudget(node, defs, seen, depth, &budget)
}

func inlineKimiSchemaBudget(node any, defs map[string]any, seen map[string]bool, depth int, budget *int) any {
	if *budget <= 0 {
		return nil
	}
	*budget--
	if depth > 64 {
		return map[string]any{}
	}
	switch v := node.(type) {
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			if *budget <= 0 {
				break
			}
			out[i] = inlineKimiSchemaBudget(x, defs, seen, depth+1, budget)
		}
		return out
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			name := strings.TrimPrefix(ref, "#/$defs/")
			name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
			if seen[name] {
				return map[string]any{}
			}
			target, ok := defs[name]
			if !ok {
				return map[string]any{"type": "object"}
			}
			next := make(map[string]bool, len(seen)+1)
			for k, x := range seen {
				next[k] = x
			}
			next[name] = true
			return inlineKimiSchemaBudget(target, defs, next, depth+1, budget)
		}
		out := make(map[string]any, len(v))
		for k, x := range v {
			if *budget <= 0 {
				break
			}
			if k != "$defs" && k != "$schema" {
				out[k] = inlineKimiSchemaBudget(x, defs, seen, depth+1, budget)
			}
		}
		return out
	default:
		return node
	}
}
