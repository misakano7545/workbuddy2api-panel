// restrict_test.go 11140 风控限制文书的识别与分类。
//
// 判据两条必须同时成立，缺一条都会把账算错：
//   - 只按 code 11140 判：会把「模型供应商限流」（同一 code）判成账号终态，白扔一个号；
//   - 只按文案判：正文里讨论「已被限制」的普通请求会被误判。
package upstream

import "testing"

func TestIsRestricted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"中文风控文书", `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true},
		{"英文 unusual activity", `{"code":11140,"msg":"Your account shows unusual activity and has been restricted."}`, true},
		{"字符串形态的 code", `{"code":"11140","msg":"账号存在异常行为"}`, true},
		{"带空白的 code", `{"code": 11140, "msg": "账号存在异常行为"}`, true},
		// 同一 code 承载的模型级限流：不能算「账号被限制」。
		{"只有 code 没有限制文案", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		// 有文案但 code 对不上（例如内容审核的业务码）：不是账号限制。
		{"只有文案没有 code", `{"code":50400,"msg":"账号存在异常行为"}`, false},
		// 授权形态（英文 request illegal）归 accountFaultMarkers，不是这条判据。
		{"request illegal 形态", `{"code":11140,"msg":"request illegal"}`, false},
		{"纯文本", `账号存在异常行为，已被限制使用`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRestricted(tc.body); got != tc.want {
				t.Fatalf("IsRestricted(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyRestrictionIsAccountFault 中文风控文书必须归账号级：落通用 4xx 兜底
// （ErrClient）只会「换号继续打」，一个请求就能把整池账号按顺序送进风控。
func TestClassifyRestrictionIsAccountFault(t *testing.T) {
	if got := Classify(403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`); got != ErrAccountFault {
		t.Fatalf("中文 11140 文书应归 ErrAccountFault，得到 %v", got)
	}
	// 同一 code 的模型限流不得被这条判据吃掉（仍是可自愈的软限流）。
	if got := Classify(429, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`); got != ErrSoftRate {
		t.Fatalf("11140 + 限流文案应保持 ErrSoftRate，得到 %v", got)
	}
}
