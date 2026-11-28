package ratelimit

import (
	"strings"
	"testing"
	"time"
)

// 导出包是「提交上游」的载体，格式坏掉没人会发现（它只在页面上下载一次）。
// 这里锁住几处形状：口径依据、逐行额度、降级链、以及配置片段的代码围栏。
//
// 围栏那一条是踩过的坑：早先把三个反引号写成了「反引号 + ~」，渲染出来是一行
// 「反引号~yaml」，上游拿到的配置片段根本不是 YAML。
func TestBundleMarkdownShape(t *testing.T) {
	p := Policy{Preset: "balanced", Default: Normalize(ModelLimit{
		Source: SourceFollow, ChainMode: ChainAuto, OnHit: OnHitDegrade,
	})}
	p.Overrides = []ModelLimit{Normalize(ModelLimit{
		Realm: "cn", Model: "m1", Source: SourceManual,
		TokenQuotaPerAccount: 50_000_000, OnHit: OnHitDegrade,
		Chain: []string{"free-1"},
	})}
	v := Build(p, testDeps(time.Now()))
	b := BuildBundle(p, v)

	for _, want := range []string{
		"### 口径依据",
		"### 生效额度（按域）",
		"### 降级链",
		"```yaml\npool:\n  model_fallback:",
		"```\n",
	} {
		if !strings.Contains(b.Markdown, want) {
			t.Errorf("导出 markdown 缺少 %q:\n%s", want, b.Markdown)
		}
	}
	if len(b.Limits) != len(v.Rows) {
		t.Errorf("逐行口径应覆盖全部行：%d != %d", len(b.Limits), len(v.Rows))
	}
	if len(b.Basis) == 0 {
		t.Error("导出包必须带口径依据")
	}
}
