package upstream

import (
	"encoding/json"
	"testing"
)

// hint 层此前完全没有测试（这正是 supportsImages 丢 ok 的 bug 能活下来的原因）。
// 这里守住两条：能力断言只在目录**显式声明**时才给，以及各 Kind 的中性回落。

func TestGatewayHintModelParamInvalidImageTriState(t *testing.T) {
	const msg = `{"code":11133,"msg":"the request parameters were rejected by the model provider"}`
	const neutral = "request parameters were rejected by the model provider; check message format and model capabilities"

	// 目录声明了 false（显式）：才敢断言「不支持图片」。
	got := GatewayHint(ErrModelParamInvalid, msg, HintContext{
		Model: "glm-x", HasImage: true, ModelInCatalog: true,
		ModelSupportsImagesKnown: true, ModelSupportsImages: false,
	})
	if got != "model glm-x does not support images; pick one with supports_images=true from /v1/models" {
		t.Errorf("显式 false 应给出换模型指向，got=%q", got)
	}

	// 目录**没声明**该能力（缺键）：无从判断 → 中性文案，不得断言不支持。
	got = GatewayHint(ErrModelParamInvalid, msg, HintContext{
		Model: "auto-chat", HasImage: true, ModelInCatalog: true,
		ModelSupportsImagesKnown: false, ModelSupportsImages: false,
	})
	if got != neutral {
		t.Errorf("缺声明时必须退中性文案（不得编造能力事实），got=%q", got)
	}

	// 声明支持图片、但请求确实带图且参数被拒 → 中性（问题不在图片）。
	got = GatewayHint(ErrModelParamInvalid, msg, HintContext{
		Model: "glm-x", HasImage: true, ModelInCatalog: true,
		ModelSupportsImagesKnown: true, ModelSupportsImages: true,
	})
	if got != neutral {
		t.Errorf("支持图片的模型被拒参数时应中性，got=%q", got)
	}

	// 未带图的 11133：中性（可能是任意参数问题，不点名图片）。
	if got = GatewayHint(ErrModelParamInvalid, msg, HintContext{Model: "glm-x", ModelInCatalog: true}); got != neutral {
		t.Errorf("未带图应中性，got=%q", got)
	}
}

func TestGatewayHintKindsAndFallback(t *testing.T) {
	if got := GatewayHint(ErrNone, "", HintContext{}); got != "" {
		t.Errorf("未覆盖形态应返回空串（不带字段），got=%q", got)
	}
	if got := GatewayHint(ErrPromptTooLong, "too long", HintContext{}); got == "" {
		t.Error("ErrPromptTooLong 应给 hint")
	}
	// 11135 先于 Kind 表判定（上游业务码优先）。
	if got := GatewayHint(ErrClient, `{"code":11135,"msg":"invalid_image_data"}`, HintContext{}); got == "" {
		t.Error("11135 形态应给 hint（业务码先于 Kind 表）")
	}
}

// TestParseGlobalModelLooseSupportsImagesTriState 两个解析入口都必须把「缺键」与
// 「显式 false」分开——否则 /v1/models 与 11133 的 hint 都无法区分「不支持」与「未知」。
func TestParseGlobalModelLooseSupportsImagesTriState(t *testing.T) {
	for _, tc := range []struct {
		name               string
		raw                string
		wantVal, wantKnown bool
	}{
		{"显式 true", `{"id":"m","supportsImages":true}`, true, true},
		{"显式 false", `{"id":"m","supportsImages":false}`, false, true},
		{"缺键", `{"id":"m"}`, false, false},
	} {
		var obj map[string]any
		if err := json.Unmarshal([]byte(tc.raw), &obj); err != nil {
			t.Fatal(err)
		}
		mi, ok := parseGlobalModelLoose(obj)
		if !ok {
			t.Fatalf("%s: 解析失败", tc.name)
		}
		if mi.SupportsImages != tc.wantVal || mi.SupportsImagesSet != tc.wantKnown {
			t.Errorf("%s: SupportsImages=%v(set=%v) want %v(set=%v)",
				tc.name, mi.SupportsImages, mi.SupportsImagesSet, tc.wantVal, tc.wantKnown)
		}
	}
}

// TestDynModelEntrySupportsImagesTriState CN 目录走 typed 解析（dynModelEntry），
// 缺键同样必须与显式 false 分开。
func TestDynModelEntrySupportsImagesTriState(t *testing.T) {
	for _, tc := range []struct {
		name               string
		raw                string
		wantVal, wantKnown bool
	}{
		{"显式 true", `{"id":"m","supportsImages":true}`, true, true},
		{"显式 false", `{"id":"m","supportsImages":false}`, false, true},
		{"缺键", `{"id":"m"}`, false, false},
	} {
		var e dynModelEntry
		if err := json.Unmarshal([]byte(tc.raw), &e); err != nil {
			t.Fatal(err)
		}
		mi := e.modelInfo()
		if mi.SupportsImages != tc.wantVal || mi.SupportsImagesSet != tc.wantKnown {
			t.Errorf("%s: SupportsImages=%v(set=%v) want %v(set=%v)",
				tc.name, mi.SupportsImages, mi.SupportsImagesSet, tc.wantVal, tc.wantKnown)
		}
	}
}
