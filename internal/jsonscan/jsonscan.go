// Package jsonscan 从**原始 JSON 字节**里读取少数几个字段，不做整棵树的解析。
//
// 为什么需要它：网关每处理一个 chat 请求，会对同一个 body 做 4~6 次
// json.Unmarshal（会话键、首条 role、kimi 工具、body 归一化、会话字段注入），
// 而线上代理负载的 body 常在 100~500KB。整棵树解析要给每个字符串解转义、
// 建 map、分配几十万对象；其中大部分字段根本没人看。
//
// 本包遍历一次、只解出被问到的少数几个字符串，其余值按 JSON 语法跳过（零分配）。
// 语义与 encoding/json 一致：解出来的值走 json.Unmarshal 解转义；跳过只看语法。
// 任何解析不了的情况都返回 Ok=false，调用方**必须**回落到整树解析。
//
// 性能要点（都是实测踩出来的）：
//   - 不要为「查一个键」扫整个 body 多次——一次遍历收集所有想要的键；
//   - 不要对跳过的字符串调用 json.Unmarshal（第一版的致命错误：600KB 历史消息里
//     几千个字符串各解一次，比整树解析还慢 4 倍）；
//   - 字符串边界用 bytes.IndexByte，只在疑似反斜杠时才回退逐字节。
package jsonscan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Query 描述要读的字段。
type Query struct {
	// Top 顶层键（只取值是字符串的那些）。
	Top []string
	// Nested 顶层子对象键（例如 "metadata"）；与 NestedKeys 都非空时才生效。
	Nested     string
	NestedKeys []string
}

// Result 读取结果。Ok=false 表示 body 不是本包能完整走完的 JSON 对象，
// 调用方应回落整树解析（此时其它字段的值不可信）。
type Result struct {
	Top    map[string]string
	Nested map[string]string
	Ok     bool
}

// Get 取顶层键的值（未命中 / 非字符串 → ""）。
func (r Result) Get(key string) string { return r.Top[key] }

// GetNested 取顶层子对象里的键值（未命中 / 非字符串 → ""）。
func (r Result) GetNested(key string) string { return r.Nested[key] }

// Lookup 单遍遍历顶层对象，收集 Query 要求的字符串值。
//
// 大数据量的字段（messages/tools）在这里只是被跳过，不做任何解码。
func Lookup(body []byte, q Query) Result {
	res := Result{Top: map[string]string{}, Nested: map[string]string{}}
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return res
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) {
			return res
		}
		if body[i] == '}' {
			res.Ok = true
			return res
		}
		key, next, ok := readKey(body, i)
		if !ok {
			return res
		}
		i = skipSpace(body, next)
		if i >= len(body) || body[i] != ':' {
			return res
		}
		i = skipSpace(body, i+1)
		if q.Nested != "" && len(q.NestedKeys) > 0 && string(key) == q.Nested {
			res.Nested = collect(body, i, q.NestedKeys)
			j, ok := skipValue(body, i)
			if !ok {
				return res
			}
			i = skipSpace(body, j)
		} else if matchAny(key, q.Top) {
			if v, after, ok := readStrValue(body, i); ok {
				res.Top[string(key)] = v
				i = skipSpace(body, after)
			} else {
				j, ok := skipValue(body, i)
				if !ok {
					return res
				}
				i = skipSpace(body, j)
			}
		} else {
			j, ok := skipValue(body, i)
			if !ok {
				return res
			}
			i = skipSpace(body, j)
		}
		if i >= len(body) {
			return res
		}
		switch body[i] {
		case ',':
			i++
		case '}':
			res.Ok = true
			return res
		default:
			return res
		}
	}
}

// collect 在 body[i] 处的对象里收集 keys 的字符串值（未命中留空）。
func collect(body []byte, i int, keys []string) map[string]string {
	out := map[string]string{}
	i = skipSpace(body, i)
	if i >= len(body) || body[i] != '{' {
		return out
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) || body[i] == '}' {
			return out
		}
		key, next, ok := readKey(body, i)
		if !ok {
			return out
		}
		i = skipSpace(body, next)
		if i >= len(body) || body[i] != ':' {
			return out
		}
		i = skipSpace(body, i+1)
		if matchAny(key, keys) {
			if v, after, ok := readStrValue(body, i); ok {
				out[string(key)] = v
				i = skipSpace(body, after)
			} else {
				j, ok := skipValue(body, i)
				if !ok {
					return out
				}
				i = skipSpace(body, j)
			}
		} else {
			j, ok := skipValue(body, i)
			if !ok {
				return out
			}
			i = skipSpace(body, j)
		}
		if i >= len(body) {
			return out
		}
		switch body[i] {
		case ',':
			i++
		case '}':
			return out
		default:
			return out
		}
	}
}

// TopLevelString 取顶层对象里某个键的字符串值（便捷入口；查多个键时直接用 Lookup）。
func TopLevelString(body []byte, key string) (string, bool) {
	res := Lookup(body, Query{Top: []string{key}})
	if !res.Ok {
		return "", false
	}
	v, ok := res.Top[key]
	return v, ok
}

// NestedString 取顶层对象.objKey 这个子对象里 key 的字符串值。
func NestedString(body []byte, objKey, key string) (string, bool) {
	res := Lookup(body, Query{Nested: objKey, NestedKeys: []string{key}})
	if !res.Ok {
		return "", false
	}
	v, ok := res.Nested[key]
	return v, ok
}

// FirstMessageRole 取 messages[0].role。语义与整树解析一致：
// messages 不是非空数组 / 首元素不是对象 / role 缺失或不是字符串 → ok=false。
func FirstMessageRole(body []byte) (string, bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return "", false
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) || body[i] == '}' {
			return "", false
		}
		key, next, ok := readKey(body, i)
		if !ok {
			return "", false
		}
		i = skipSpace(body, next)
		if i >= len(body) || body[i] != ':' {
			return "", false
		}
		i = skipSpace(body, i+1)
		if string(key) == "messages" {
			i = skipSpace(body, i)
			if i >= len(body) || body[i] != '[' {
				return "", false
			}
			i = skipSpace(body, i+1)
			if i < len(body) && body[i] == ']' {
				return "", false
			}
			first := collect(body, i, []string{"role"})
			role, ok := first["role"]
			return role, ok
		}
		j, ok := skipValue(body, i)
		if !ok {
			return "", false
		}
		i = skipSpace(body, j)
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case ',':
			i++
		case '}':
			return "", false
		default:
			return "", false
		}
	}
}

// IsObject 报告扫描器能否完整走完这个 body 的顶层对象。
//
// 用途：调用方用它决定「扫描结果可信」还是「回落整树解析」。走不通说明 body
// 不是合法对象或形态超出本包假设——此时不要相信「没找到键」的结论。
func IsObject(body []byte) bool {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) {
			return false
		}
		if body[i] == '}' {
			return true
		}
		_, next, ok := readKey(body, i)
		if !ok {
			return false
		}
		i = skipSpace(body, next)
		if i >= len(body) || body[i] != ':' {
			return false
		}
		j, ok := skipValue(body, i+1)
		if !ok {
			return false
		}
		i = skipSpace(body, j)
		if i >= len(body) {
			return false
		}
		switch body[i] {
		case ',':
			i++
		case '}':
			return true
		default:
			return false
		}
	}
}

// matchAny 报告 key 是否在 want 里。
func matchAny(key []byte, want []string) bool {
	for _, w := range want {
		if string(key) == w {
			return true
		}
	}
	return false
}

// skipSpace 跳过 JSON 空白。
func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// readKey 读一个对象键（b[i] 必须是 '"'），返回**未解转义**的原始字节与结束偏移。
// 键几乎总是纯 ASCII，原始字节可直接与候选键比较；含转义的键由调用方按原文比较
// （对候选键无转义这一前提成立）。这里不做解码，避免为每个键付一次 Unmarshal。
func readKey(b []byte, i int) ([]byte, int, bool) {
	if i >= len(b) || b[i] != '"' {
		return nil, i, false
	}
	j := i + 1
	for {
		n := bytes.IndexByte(b[j:], '"')
		if n < 0 {
			return nil, i, false
		}
		j += n
		if !escaped(b, i, j) {
			return b[i+1 : j], j + 1, true
		}
		j++
	}
}

// escaped 报告 b[pos] 处的引号是否被反斜杠转义（数前面连续反斜杠的奇偶）。
func escaped(b []byte, start, pos int) bool {
	bs := 0
	for k := pos - 1; k > start && b[k] == '\\'; k-- {
		bs++
	}
	return bs%2 == 1
}

// readStrValue 读一个字符串值并解转义。b[i] 必须是 '"'。
func readStrValue(b []byte, i int) (string, int, bool) {
	raw, end, ok := readKey(b, i)
	if !ok {
		return "", i, false
	}
	// 快路径只处理「无转义 + 合法 UTF-8」的字符串（绝大多数情况）。
	// 其余一律交给 encoding/json：它会把非法 UTF-8 字节替换成 U+FFFD、把 \uXXXX
	// 解成对应字符——直接 string(raw) 会漏掉前者（fuzz 抓到的真实差异）。
	if bytes.IndexByte(raw, '\\') < 0 && utf8.Valid(raw) {
		return string(raw), end, true
	}
	var s string
	if err := json.Unmarshal(b[i:end], &s); err != nil {
		return "", i, false
	}
	return s, end, true
}

// skipString 跳过一个字符串（不解转义、不分配），返回结束偏移。
func skipString(b []byte, i int) (int, bool) {
	if i >= len(b) || b[i] != '"' {
		return i, false
	}
	j := i + 1
	for {
		n := bytes.IndexByte(b[j:], '"')
		if n < 0 {
			return i, false
		}
		j += n
		if !escaped(b, i, j) {
			return j + 1, true
		}
		j++
	}
}

// skipValue 从 b[i] 起跳过一个完整 JSON 值，返回其后的偏移。零分配、不解转义。
func skipValue(b []byte, i int) (int, bool) {
	i = skipSpace(b, i)
	if i >= len(b) {
		return i, false
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				next, ok := skipString(b, i)
				if !ok {
					return i, false
				}
				i = next
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	default:
		// 数字 / true / false / null：扫到分隔符为止。
		start := i
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				if i == start {
					return i, false
				}
				return i, true
			}
			i++
		}
		return i, i > start
	}
}

// Validate 只在测试里用：确认扫描器与整树解析对同一 body 的判定一致。
func Validate(body []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil // 坏 body 不是本函数的关注点
	}
	keys := []string{"model", "conversation_id", "conversationId", "user_id", "prompt_cache_key"}
	res := Lookup(body, Query{Top: keys})
	if !res.Ok {
		return fmt.Errorf("scanner could not walk object: %s", body)
	}
	for _, k := range keys {
		want, _ := obj[k].(string)
		if want != res.Top[k] {
			return fmt.Errorf("key %q: got %q want %q", k, res.Top[k], want)
		}
	}
	return nil
}
