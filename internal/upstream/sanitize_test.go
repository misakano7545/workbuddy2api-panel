package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11-128 以打断精确匹配。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := sanitizeText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Errorf("error code not rewritten as expected: %q", out)
	}
}

// reasoning 字段：thinking.go 的回填把客户端送来的 reasoning_content 镜像进
// reasoning（且回填就排在 sanitizeMessages 之前），此前只洗 content /
// reasoning_content / tool_calls → 镜像出来的那一份指纹原样出站。
// 注意指纹字面量必须与 sanitizeFeatures 里的形态逐字一致。
func TestSanitizeMessagesScrubsReasoningField(t *testing.T) {
	// 用 ccIdentity（测试文件里已有的纯 ASCII 指纹常量）当脏内容：它走同一条
	// sanitizeText 改写路径，且不用硬编码数字串（本仓工具链会改写数字字面量）。
	ms := []any{map[string]any{
		"role":      "assistant",
		"content":   "hi",
		"reasoning": ccIdentity,
	}}
	if !sanitizeMessages(ms, true) {
		t.Fatal("reasoning 字段里的指纹未被净化")
	}
	got, _ := ms[0].(map[string]any)["reasoning"].(string)
	if got == ccIdentity {
		t.Fatalf("reasoning 未被净化: %q", got)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo 11128"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs, true) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "11128") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含 11128 也不含反馈整句的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content, true)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 集成：完整请求体经 PrepareBodyOpt 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodyOptSanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

func TestPrepareBodyOptDisabledPreservesFingerprints(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), ccIdentity) {
		t.Error("sanitize=false should preserve fingerprints")
	}
	// 但 stream 仍强制
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != true {
		t.Error("stream should still be forced")
	}
}

// PrepareBody 默认行为 = 开启脱敏（保持向后兼容）。
func TestPrepareBodyDefaultSanitizes(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), ccIdentity) {
		t.Error("PrepareBody default should sanitize")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// 出站边界（Codex 场景）：CPA 把 instructions 折成 role=system 的首条消息，
// 净化后 wire body 不得残留 Codex 逐字指纹；其余消息不受影响。
func TestChatStreamWireBodyCodexInstructionsSanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"` + codexInstructions + `"},` +
		`{"role":"user","content":"say ok"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, codexInstructions) ||
		strings.Contains(sys, "in the Codex CLI, a terminal-based coding assistant.") {
		t.Errorf("wire body still contains codex fingerprint: %q", sys)
	}
	if !strings.Contains(sys, "running in the Codex CLI tool, a terminal-based") {
		t.Errorf("codex rewrite missing on wire: %q", sys)
	}
	user, _ := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if user != "say ok" {
		t.Errorf("user message altered: %q", user)
	}
}

// 出站边界：关闭脱敏后 wire body 原样保留指纹（验证开关真实有效）。
func TestChatStreamWireBodySanitizeDisabled(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(false)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	rc, status, _, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d", status)
	}
	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Error("sanitize disabled should preserve fingerprint on wire")
	}
}

// DSH 的身份句是短语级指纹：上游按逐字匹配拦（400 11-128 unapproved channel），
// 换一个词即绕过。钉住三条：命中改写、只含品牌词也走净化（特征预检不得漏）、
// 改写结果不再含原始短语（可重复处理）。
func TestDeepSeekHarnessBrandRewritten(t *testing.T) {
	// 品牌串从表中取：真实字面量与文档里的写法差一个空格，手写会静默对不上。
	brand := sanitizeIdentityFeatures[0]
	in := "You are an AI agent powered by " + brand + ".\n\nCheck the exit code."
	out := sanitizeTextIdentity(in)
	// 整句被中性句替换（身份句式规则）：只剩品牌改写时 `You are … powered by …` 的
	// 句子骨架仍是特征，所以这里是整句级断言。
	if strings.Contains(out, brand) || strings.Contains(out, "powered by") {
		t.Fatalf("identity sentence not neutralized: %q", out)
	}
	if !strings.HasPrefix(out, "You are a helpful AI assistant.") {
		t.Errorf("sentence not replaced with neutral one: %q", out)
	}
	if !strings.Contains(out, "\n\nCheck the exit code.") {
		t.Errorf("surrounding text changed: %q", out)
	}
	if again := sanitizeTextIdentity(out); again != out {
		t.Errorf("not idempotent: %q -> %q", out, again)
	}
}

// 身份句式的变体覆盖：动词表七种、名词可省、专名与冠词都认。
// 上游补一条新搭配就能让只认单个动词的实现重新 400，所以逐个钉住。
func TestIdentitySentenceVariants(t *testing.T) {
	const neutral = "You are a helpful AI assistant."
	for _, in := range []string{
		"You are an AI agent powered by AcmeRuntime. Next.",
		"You are a coding agent powered by the deepseek-v4.1-flash model. Next.",
		"You are an AI assistant built on DeepSeek. Next.",
		"You are Acme Agent, built by Acme Corp. Next.",
		"You are the FooBot assistant, created by someone. Next.",
		"You are a bot developed by someone. Next.",
		"You are a tool made by someone. Next.",
		"You are a model trained by someone. Next.",
	} {
		out := sanitizeTextIdentity(in)
		if !strings.HasPrefix(out, neutral) {
			t.Errorf("未中性化: %q -> %q", in, out)
		}
		if !strings.Contains(out, "Next.") {
			t.Errorf("句后文本丢失: %q", out)
		}
	}
}

// 误伤保护：「You are free to use tools built by …」这类分词修饰宾语的句子不是身份句，
// 靠正则窗口上界（形容词 0~3 个）挡住。预检也必须判 false，否则会白白 TrimSpace。
func TestIdentitySentenceFalsePositives(t *testing.T) {
	for _, in := range []string{
		"You are free to use tools built by the vendor.",
		"You are a helpful assistant that uses services developed by others. Ok.",
		"You are welcome to deploy packages made by third parties.",
		"You are right, that module was built by the other team. Fine.",
	} {
		if hasIdentityFingerprint(in) {
			t.Errorf("预检误判: %q", in)
		}
		if out := sanitizeTextIdentity(in); out != in {
			t.Errorf("误改写: %q -> %q", in, out)
		}
	}
}

// 作用域：身份句式属于身份类规则，sanitizeText（brand=false）不动，只有身份路径改。
// 这与品牌词规则的既有作用域一致（sanitize_brand_scope 控制）。
func TestIdentitySentenceScope(t *testing.T) {
	in := "You are an AI agent powered by AcmeRuntime. Go."
	if out := sanitizeText(in); out != in {
		t.Errorf("brand=false 不该改写: %q", out)
	}
	if out := sanitizeTextIdentity(in); out == in {
		t.Errorf("身份路径应该改写: %q", out)
	}
}

// 预检快路径：文本只含品牌词、不含任何其它指纹，也必须进入净化。
func TestDeepSeekHarnessDetected(t *testing.T) {
	in := "You are interacting through the DeepSeek Harness Web GUI."
	if hasFingerprint(in) {
		t.Error("brand word should NOT hit the global fast path（否则非身份消息会被 TrimSpace/改写）")
	}
	if !hasIdentityFingerprint(in) {
		t.Error("brand word should hit the identity fast path")
	}
}

// 走完 prepareBody 全链路：system 的身份句整句中性化，user 里的品牌词按作用域改写。
func TestPrepareBodyRewritesDeepSeekHarness(t *testing.T) {
	// 品牌串从表里取：真实字面量与文档写法差一个空格，手写会静默对不上而测试永远绿。
	brand := sanitizeIdentityFeatures[0]
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an AI agent powered by " + brand + "."},
			map[string]any{"role": "user", "content": "the " + brand + " checkout is here"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, _ := prepareBodyOpt(body, true, true, nil, nil, nil, "", nil, "", "", false, 0, "")

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	usr, _ := msgs[1].(map[string]any)["content"].(string)
	if sys != "You are a helpful AI assistant." {
		t.Errorf("system 身份句未中性化: %q", sys)
	}
	if strings.Contains(usr, brand) {
		t.Errorf("user 品牌词未改写: %q", usr)
	}
	if !strings.Contains(usr, "checkout is here") {
		t.Errorf("user 其余文本被破坏: %q", usr)
	}
}

// newTestUpstream 起一个假上游并捕获请求。
func newTestUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}

// TestBrandScopeAllRewritesUserMessages 默认作用域（all）：与上游既有行为一致，
// 所有消息角色的品牌词都改写（包括用户输入）。
func TestBrandScopeAllRewritesUserMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are DeepSeek Harness."},
		map[string]any{"role": "user", "content": "read DeepSeek Harness docs"},
	}
	if !sanitizeMessages(msgs, true) {
		t.Fatal("sanitizeMessages all: 未报告改动")
	}
	for i, m := range msgs {
		if got := m.(map[string]any)["content"].(string); strings.Contains(got, "DeepSeek Harness") {
			t.Errorf("msg[%d] 品牌词未改写: %q", i, got)
		}
	}
}

// TestBrandScopeIdentityLeavesUserMessages identity 作用域：只改 system/developer，
// 用户/工具/assistant 内容（含 tool_calls 参数与 reasoning）原样透传。
func TestBrandScopeIdentityLeavesUserMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are DeepSeek Harness."},
		map[string]any{"role": "user", "content": "  DeepSeek Harness  "},
		map[string]any{"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{
				"name": "Read", "arguments": `{"path":"DeepSeek Harness.md"}`,
			}}},
		},
	}
	if !sanitizeMessages(msgs, false) {
		t.Fatal("sanitizeMessages identity: system 未改写")
	}
	if got := msgs[0].(map[string]any)["content"].(string); !strings.Contains(got, "DeepSeekRuntime") {
		t.Errorf("system 品牌词未改写: %q", got)
	}
	if got := msgs[1].(map[string]any)["content"].(string); got != "  DeepSeek Harness  " {
		t.Errorf("user 内容被改动（应原样含首尾空白）: %q", got)
	}
	args := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, "DeepSeek Harness") {
		t.Errorf("tool_calls 参数被改动: %q", args)
	}
}

// TestBrandScopeDefaultGlobal 默认值：Client 未配置作用域时按 all 处理。
func TestBrandScopeDefaultGlobal(t *testing.T) {
	var c Client
	c.SanitizeFingerprints.Store(true)
	if !c.brandScopeGlobal() {
		t.Error("默认应为 all（brandScopeGlobal=true）")
	}
	c.SanitizeBrandScope.Store("identity")
	if c.brandScopeGlobal() {
		t.Error("identity 应返回 false")
	}
	c.SanitizeBrandScope.Store("")
	if !c.brandScopeGlobal() {
		t.Error("空值应回落 all")
	}
}

// DSH 0.3.x 的 @ 路径说明段是**唯一不含品牌词**的 DSH 指纹：品牌改写规则碰不到它，
// 原文发出即 400 11-128 unapproved channel。钉住四条：整段被改写、两种线上措辞都覆盖、
// 段外 @ 不动、改写可重复。
const (
	dshPathsNow = "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths point to " +
		"files or directories on the host. A trailing slash marks a directory, so " +
		"list it when its contents matter. Anything else is a file: open it with " +
		"the read tool when its contents are needed, and never claim to have " +
		"inspected it before reading. The @\"...\" form only quotes a path that " +
		"contains spaces."
	dshPathsOld = "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify " +
		"files or directories on the host. A trailing slash marks a directory: " +
		"list it when its contents matter. Anything else is a file: use the read " +
		"tool when its contents are needed, and do not claim to have inspected it " +
		"before reading. @\"...\" quotes a path containing spaces."
)

func TestDshPathsParagraphRewritten(t *testing.T) {
	if !hasFingerprint(dshPathsNow) {
		t.Fatal("段落未被特征预检命中：净化整条链都不会启动")
	}
	out := sanitizeText(dshPathsNow)
	if out != sanitizeDshPathsReplacement {
		t.Fatalf("整段未被替换:\n got=%q\nwant=%q", out, sanitizeDshPathsReplacement)
	}
	if strings.Contains(out, "Tokens prefixed with @") {
		t.Errorf("原文残留在出站文本里: %q", out)
	}
	// 语义必须保留：读提示词的 DSH 仍要知道 @ 路径怎么解析。
	for _, want := range []string{"project root", "trailing slash", "read tool", "@\"...\""} {
		if !strings.Contains(out, want) {
			t.Errorf("改写丢语义 %q: %q", want, out)
		}
	}
}

// 更早的 DSH 写法（identify / use the read tool / quotes a path containing spaces）
// 同一段语义、不同措辞：正则的结尾分支必须也吃下它。
func TestDshPathsOlderWordingRewritten(t *testing.T) {
	if out := sanitizeText(dshPathsOld); out != sanitizeDshPathsReplacement {
		t.Fatalf("旧措辞未覆盖:\n got=%q\nwant=%q", out, sanitizeDshPathsReplacement)
	}
}

// 段内本来就有换行，(?s) 没开的话 .*? 跨不过去，只改半段等于没改。
func TestDshPathsMultilineRewritten(t *testing.T) {
	in := strings.Replace(dshPathsNow, "Relative paths", "\n\nRelative paths", 1)
	out := sanitizeText(in)
	if strings.Contains(out, "Tokens prefixed with @") || !strings.Contains(out, "Paths beginning with @") {
		t.Fatalf("跨行形态未覆盖: %q", out)
	}
}

// 段落嵌在更长的 system 里时只换这一段，前后文原样。
func TestDshPathsEmbeddedKeepsSurroundings(t *testing.T) {
	in := "before\n\n" + dshPathsNow + "\n\nafter"
	if out := sanitizeText(in); out != "before\n\n"+sanitizeDshPathsReplacement+"\n\nafter" {
		t.Fatalf("前后文被动了: %q", out)
	}
}

func TestDshPathsIdempotent(t *testing.T) {
	once := sanitizeText(dshPathsNow)
	if again := sanitizeText(once); again != once {
		t.Fatalf("不幂等: %q -> %q", once, again)
	}
}

// 段外的 @ 是普通文本（邮箱、npm 包名、Windows 路径），不得改写也不得触发净化。
func TestDshPathsUnrelatedAtTextUntouched(t *testing.T) {
	for _, in := range []string{
		"Mail me at someone@example.com and open C:\\cache\\@tmp.",
		"npm i @deepseek-ai/dsh-llm",
		"Relative paths resolve from the workspace root, but this is not the paragraph.",
	} {
		if hasFingerprint(in) {
			t.Errorf("误触发预检: %q", in)
		}
		if out := sanitizeText(in); out != in {
			t.Errorf("误改写: %q -> %q", in, out)
		}
	}
}

// 走完 prepareBody 全链路：system 里的这段必须真正在出站 body 里被换掉。
func TestPrepareBodyRewritesDshPaths(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": dshPathsNow},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, _ := prepareBodyOpt(body, true, true, nil, nil, nil, "", nil, "", "", false, 0, "")
	if strings.Contains(string(out), "Tokens prefixed with @") {
		t.Fatalf("出站 body 仍带指纹: %s", out)
	}
	if !strings.Contains(string(out), "Paths beginning with @") {
		t.Fatalf("出站 body 未被改写: %s", out)
	}
}
