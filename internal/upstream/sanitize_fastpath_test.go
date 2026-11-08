package upstream

import (
	"strings"
	"testing"
)

// bareHdrKey 裸键名（无冒号）字面量，从正则推导的探针复制而来——不从显示文本手抄
// （本仓的显示层会把 "header" 缩成 "hdr"，照抄会写错字节）。
func bareHdrKey() string { return sanitizeBareHdrProbe }

// TestBareHdrProbeMatchesRegex 预检与正则必须等价。
//
// 踩过的坑：探针手抄自正则时抄错了一个词（"…-hdr" vs "…-header"），预检永远判假，
// 整条净化被静默跳过，而所有既有测试仍然全绿——它们测的是正则本身，不是预检。
func TestBareHdrProbeMatchesRegex(t *testing.T) {
	if sanitizeBareHdrProbe == "" {
		t.Fatalf("探针为空：sanitizeBareHdrRe=%q 已不是纯 (?i) 字面量，预检回落成直接跑正则（慢但不判错）",
			sanitizeBareHdrRe.String())
	}
	cases := []string{
		bareHdrKey(),
		"X-Anthropic-Billing-" + "HDR",
		"prefix X-ANTHROPIC-BILLING-" + "HDR suffix",
		"not a fingerprint at all",
		"anthropic billing hdr without dashes",
		"",
	}
	for _, c := range cases {
		got, want := containsFoldASCII(c, sanitizeBareHdrProbe), sanitizeBareHdrRe.MatchString(c)
		// 折叠预检允许误命中（多跑一次正则），但不允许漏判。
		if want && !got {
			t.Errorf("预检漏判 %q：fold=false 但 regex=true", c)
		}
		if !want && got {
			t.Logf("预检误命中 %q（可接受：后续正则是否掉）", c)
		}
	}
}

func TestContainsFoldASCII(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"Hello World", "hello", true},
		{"Hello World", "WORLD", true},
		{"Hello World", "world", true},
		{"Hello", "hello world", false},
		{"", "a", false},
		{"abc", "", true},
		{"xxABCxx", "abc", true},
		{"xxABxx", "abc", false},
	}
	for _, c := range cases {
		if got := containsFoldASCII(c.s, c.sub); got != c.want {
			t.Errorf("containsFoldASCII(%q,%q)=%v want %v", c.s, c.sub, got, c.want)
		}
	}
}

// TestHasIdentityVerbShape 形状探测只作预检：命中不代表要清洗，但正则命中必须命中它。
func TestHasIdentityVerbShape(t *testing.T) {
	hits := []string{
		"You are a helpful assistant powered by Anthropic.",
		"This tool was built by the vendor.",
		"model built on top of another",
	}
	for _, s := range hits {
		if !hasIdentityVerbShape(s) {
			t.Errorf("应判出形状: %q", s)
		}
	}
	misses := []string{
		"nothing to see here",
		"bypassed the check",
		"onlywordson",
		"",
	}
	for _, s := range misses {
		if hasIdentityVerbShape(s) {
			t.Errorf("不应判出形状: %q", s)
		}
	}
}

// TestIdentityFingerprintStillDetected 换掉 ToLower 后身份句判定不能退化。
func TestIdentityFingerprintStillDetected(t *testing.T) {
	yes := "You are a helpful AI assistant Powered By DeepSeek."
	if !hasIdentityFingerprint(yes) {
		t.Fatalf("身份句漏判: %q", yes)
	}
	no := "Please write a Go HTTP handler with middleware."
	if hasIdentityFingerprint(no) {
		t.Fatalf("普通消息误判: %q", no)
	}
	if !hasIdentityFingerprint("brand is " + sanitizeIdentityFeatures[0]) {
		t.Fatalf("品牌词漏判")
	}
}

// TestSanitizeStillStripsHeader 端到端：探针改成折叠扫描后，整条净化行为不变。
func TestSanitizeStillStripsHeader(t *testing.T) {
	key := bareHdrKey()                          // 小写字面量，从正则推导
	titled := strings.ToUpper(key[:1]) + key[1:] // 首字母大写，覆盖大小写变体
	in := titled + ": " + "cc_version=1.0;"
	out := strings.ToLower(sanitizeText(in))
	if strings.Contains(out, "anthropic") {
		t.Fatalf("键值段未被剥离: %q -> %q", in, out)
	}
	bare := "quote `" + titled + "` in docs"
	if got := strings.ToLower(sanitizeText(bare)); strings.Contains(got, key) {
		t.Fatalf("裸键名未被缩写: %q -> %q", bare, got)
	}
}
