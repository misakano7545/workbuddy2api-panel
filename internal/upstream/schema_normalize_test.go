package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// normalizeParamsJSON 把一段 parameters JSON 走一遍 normalizeToolParameters，返回结果。
func normalizeParamsJSON(t *testing.T, src string) map[string]any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}
	out, ok := normalizeToolParameters(v).(map[string]any)
	if !ok {
		t.Fatalf("结果不是 object: %#v", out)
	}
	return out
}

func mustJSON(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}
	return v
}

func propOf(t *testing.T, params map[string]any, name string) map[string]any {
	t.Helper()
	props, _ := params["properties"].(map[string]any)
	p, ok := props[name].(map[string]any)
	if !ok {
		t.Fatalf("属性 %s 丢了: %#v", name, props)
	}
	return p
}

// TestNormalizeSchemaKeepsDataKeywords default / const / enum / example(s) 的值是数据，
// 不是 schema：里面出现 type/items/required/null 这些键也必须原样保留，否则改的是
// 客户端声明的取值本身（模型看到的默认值、常量都变了）。
func TestNormalizeSchemaKeepsDataKeywords(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object","properties":{
		"mode":{"type":"object","default":{"type":"str","items":["a"],"required":"x","note":null}},
		"tags":{"type":"array","items":{"type":"string"},"examples":[{"required":[null,""]}]},
		"kind":{"const":{"type":"int"}},
		"shape":{"enum":[{"type":"list","items":[1]}]}
	}}`)
	cases := map[string]struct{ key, want string }{
		"mode":  {"default", `{"type":"str","items":["a"],"required":"x","note":null}`},
		"tags":  {"examples", `[{"required":[null,""]}]`},
		"kind":  {"const", `{"type":"int"}`},
		"shape": {"enum", `[{"type":"list","items":[1]}]`},
	}
	for prop, c := range cases {
		got := propOf(t, params, prop)[c.key]
		if want := mustJSON(t, c.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s 被改写: got %#v want %#v", prop, c.key, got, want)
		}
	}
}

// TestNormalizeSchemaDependencyContainers dependentRequired / dependencies 的键是属性名
// （数据），值是属性名数组或子 schema：键叫 items/type 也不能当 schema 关键字处理，
// 子 schema 照常规整。
func TestNormalizeSchemaDependencyContainers(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object",
		"properties":{"items":{"type":"string"},"type":{"type":"string"},"z":{"type":"string"}},
		"dependentRequired":{"items":["type"]},
		"dependencies":{"type":["items"],"z":{"type":"str"}}
	}`)
	if got, want := params["dependentRequired"], mustJSON(t, `{"items":["type"]}`); !reflect.DeepEqual(got, want) {
		t.Errorf("dependentRequired 被改写: got %#v want %#v", got, want)
	}
	deps, _ := params["dependencies"].(map[string]any)
	if got, want := deps["type"], mustJSON(t, `["items"]`); !reflect.DeepEqual(got, want) {
		t.Errorf("dependencies.type 被改写: got %#v want %#v", got, want)
	}
	if z, _ := deps["z"].(map[string]any); z["type"] != "string" {
		t.Errorf("dependencies 里的子 schema 没有规整: %#v", deps["z"])
	}
}

// TestNormalizeSchemaKeepsNullValues const:null / default:null 是有意义的取值（「只能是
// null」「缺省为 null」），不是缺失的元数据；description:null 这类元数据照旧剔除。
func TestNormalizeSchemaKeepsNullValues(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object","properties":{
		"x":{"const":null,"description":null},
		"y":{"type":["string","null"],"default":null,"title":null}
	}}`)
	x := propOf(t, params, "x")
	if v, ok := x["const"]; !ok || v != nil {
		t.Errorf("const:null 被删: %#v", x)
	}
	if _, ok := x["description"]; ok {
		t.Errorf("description:null 应剔除: %#v", x)
	}
	y := propOf(t, params, "y")
	if v, ok := y["default"]; !ok || v != nil {
		t.Errorf("default:null 被删: %#v", y)
	}
	if _, ok := y["title"]; ok {
		t.Errorf("title:null 应剔除: %#v", y)
	}
}
