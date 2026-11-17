package upstream

import "testing"

// TestClassify11129Tolerant 11129 的识别不能依赖紧凑字面量 `"code":11129` 与区分大小写的文案：
// 上游信封的空白、code 的字符串形态、嵌套层级、文案大小写与中文 displayMsg 都见过，
// 漏判落 ErrClient 就会全池轮转、末端 503（见 handler 的请求级错误分支）。
func TestClassify11129Tolerant(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ErrKind
	}{
		{"code 带空格", `{"code": 11129, "msg":"x"}`, ErrBadParams},
		{"code 字符串", `{"code":"11129","msg":"x"}`, ErrBadParams},
		{"code 嵌套带空格", `{"error": {"code": 11129, "message":"x"}}`, ErrBadParams},
		{"文案大小写", `Invalid Function Call Parameters`, ErrBadParams},
		{"文案下划线形态", `invalid_function_call_parameters`, ErrBadParams},
		{"中文 displayMsg", `{"msg":"x","displayMsg":{"zh":"工具定义不合规"}}`, ErrBadParams},
		{"相邻 code 不误伤", `{"code":111290,"msg":"x"}`, ErrClient},
	} {
		if got := Classify(400, tc.body); got != tc.want {
			t.Errorf("%s: Classify(400, %s) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
}

// TestFrameKindRequestLevelByCode 首帧 error 帧里 code 才是权威：FrameKind 只把 error.message
// 交给 Classify，message 措辞不含已知文案时（或根本是别的话）11129 / 11151 会被判成普通 4xx，
// 首帧否决后照样换号。
func TestFrameKindRequestLevelByCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    ErrKind
	}{
		{"11129 数字 code", `{"error":{"message":"something else","code":11129}}`, ErrBadParams},
		{"11129 字符串 code", `{"error":{"message":"something else","code":"11129"}}`, ErrBadParams},
		{"11129 中文文案", `{"error":{"message":"工具定义不合规"}}`, ErrBadParams},
		{"11151 只有 code", `{"error":{"message":"a message has empty content","code":11151}}`, ErrEmptyContent},
		// 上游 11133 的真实措辞（client_test 同款）不含 isModelParamInvalid 的任何文案 marker，
		// 帧形态只能靠 code 认出来；11115 / 11135 / 11148 同理，message 不保证带已知文案。
		{"11133 真实措辞", `{"error":{"message":"the request parameters were rejected by the model provider","code":11133}}`, ErrModelParamInvalid},
		{"11115 只有 code", `{"error":{"message":"too many tokens","code":"11115"}}`, ErrPromptTooLong},
		{"11135 只有 code", `{"error":{"message":"bad picture","code":11135}}`, ErrImageInvalid},
		{"11148 只有 code", `{"error":{"message":"retry will not help","code":11148}}`, ErrToolMismatch},
		{"相邻 code 不误伤", `{"error":{"message":"x","code":11130}}`, ErrClient},
	} {
		if got := FrameKind(tc.payload); got != tc.want {
			t.Errorf("%s: FrameKind(%s) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}
