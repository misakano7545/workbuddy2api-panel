package ratelimit

import "testing"

func TestChainManualWins(t *testing.T) {
	l := Normalize(ModelLimit{Chain: []string{"free-1", "cheap-1"}, ChainMode: ChainAuto})
	rates := map[string]string{"free-1": "0", "cheap-1": "0.5"}
	steps, src := Chain(l, "cn", "m1", rates, nil, map[string]string{"m1": "legacy"})
	if src != ChainSourceManual {
		t.Fatalf("人工链应压过 fallback 表，得到来源 %q", src)
	}
	if len(steps) != 2 || steps[0].Model != "free-1" || steps[1].Model != "cheap-1" {
		t.Fatalf("链顺序应与人工书写一致，得到 %+v", steps)
	}
	if !steps[0].ZeroRate {
		t.Error("零倍率步骤应标记 ZeroRate")
	}
	if steps[1].RateValue != 0.5 || !steps[1].HasRate {
		t.Errorf("倍率应解析进步骤，得到 %+v", steps[1])
	}
}

func TestChainSkipsSelf(t *testing.T) {
	l := Normalize(ModelLimit{Chain: []string{"m1"}})
	steps, src := Chain(l, "cn", "m1", nil, nil, nil)
	if len(steps) != 0 || src != ChainSourceNone {
		t.Fatalf("把自己当降级目标应被剔除（否则 6004 原地打转），得到 %+v / %q", steps, src)
	}
	steps2, src2 := Chain(l, "cn", "m1", nil, nil, map[string]string{"m1": "m2"})
	if src2 != ChainSourceFallback || len(steps2) != 1 || steps2[0].Model != "m2" {
		t.Fatalf("人工链全被剔除后应回落到 fallback 表，得到 %+v / %q", steps2, src2)
	}
}

func TestChainFallbackTable(t *testing.T) {
	fb := map[string]string{"cn:m1": "y1", "m1": "y2"}
	rates := map[string]string{"y1": "1", "y2": "2"}
	steps, src := Chain(Normalize(ModelLimit{ChainMode: ChainFallback}), "cn", "m1", rates, nil, fb)
	if src != ChainSourceFallback || steps[0].Model != "y1" {
		t.Fatalf("本域的前缀键应优先，得到 %+v / %q", steps, src)
	}
	steps2, src2 := Chain(Normalize(ModelLimit{ChainMode: ChainFallback}), "global", "m1", rates, nil, fb)
	if src2 != ChainSourceFallback || steps2[0].Model != "y2" {
		t.Fatalf("跨域只认裸名键，得到 %+v / %q", steps2, src2)
	}
	if _, src3 := Chain(Normalize(ModelLimit{ChainMode: ChainFallback}), "cn", "m1", nil, nil, map[string]string{"m1": "m1"}); src3 != ChainSourceNone {
		t.Errorf("自指的 fallback 配置应视为无链，得到 %q", src3)
	}
}

func TestChainOffKeepsFallbackTable(t *testing.T) {
	l := Normalize(ModelLimit{ChainMode: ChainOff})
	if steps, src := Chain(l, "cn", "m1", nil, nil, map[string]string{"m1": "m2"}); src != ChainSourceFallback || len(steps) != 1 {
		t.Fatalf("关掉自动链不该顺手废掉已有 fallback 配置，得到 %q", src)
	}
	// off 下即便有零倍率模型，也不自动推断
	if _, src := Chain(l, "cn", "m1", map[string]string{"free": "0"}, []string{"free", "m1"}, nil); src != ChainSourceNone {
		t.Errorf("off 模式不应自动推链，得到 %q", src)
	}
}

func TestChainAutoZeroOnly(t *testing.T) {
	l := Normalize(ModelLimit{ChainMode: ChainZero})
	rates := map[string]string{"free-b": "0", "free-a": "0", "paid": "0.5"}
	steps, src := Chain(l, "cn", "m1", rates, nil, nil)
	if src != ChainSourceAutoZero {
		t.Fatalf("省钱模式的链来源应是 auto-zero，得到 %q", src)
	}
	if len(steps) != 2 || steps[0].Model != "free-a" || steps[1].Model != "free-b" {
		t.Fatalf("零倍率模型应按名字排序，得到 %+v", steps)
	}
	for _, s := range steps {
		if !s.ZeroRate || s.RateValue != 0 {
			t.Errorf("省钱模式只收零倍率，得到 %+v", s)
		}
	}
	// 没有免费档时宁可没有链（省钱语义：绝不换到更贵的档）
	if _, src := Chain(l, "cn", "m1", map[string]string{"paid": "0.5"}, nil, nil); src != ChainSourceNone {
		t.Errorf("没有零倍率模型时省钱模式应无链，得到 %q", src)
	}
}

func TestChainAutoCheap(t *testing.T) {
	l := Normalize(ModelLimit{ChainMode: ChainAuto})
	rates := map[string]string{"free": "0", "mid": "1", "low": "0.2", "unknown": ""}
	steps, src := Chain(l, "cn", "m1", rates, nil, nil)
	if src != ChainSourceAutoZero {
		t.Fatalf("链首是零倍率时来源应为 auto-zero，得到 %q", src)
	}
	if len(steps) != 2 || steps[0].Model != "free" || steps[1].Model != "low" {
		t.Fatalf("应零倍率优先 + 一个最便宜付费兜底，得到 %+v", steps)
	}
	for _, s := range steps {
		if s.Model == "unknown" {
			t.Error("未知倍率的模型不该进自动链（无法为价格负责）")
		}
	}
}

// 自动链是「零倍率优先 + 一个付费兜底」，不是「凑满链长就停」：CN 目录里
// deepseek-v4.1-flash（付费）能看到 4 个零倍率模型，4 个坑全被免费档占满时
// 付费兜底就没了；其它模型自己占一个零倍率坑，反而还有兜底——表现成「只有
// 这个模型没有兜底」。零倍率最多占 maxChainSteps-1 个坑，给付费档留一个。
func TestChainAutoKeepsPaidFallbackAtCap(t *testing.T) {
	l := Normalize(ModelLimit{ChainMode: ChainAuto})
	rates := map[string]string{
		"free-a": "0", "free-b": "0", "free-c": "0", "free-d": "0", "free-e": "0",
		"deepseek-v4.1-flash": "0.03", "deepseek-v4-flash": "0.79",
	}
	steps, src := Chain(l, "cn", "deepseek-v4.1-flash", rates, nil, nil)
	if src != ChainSourceAutoZero {
		t.Fatalf("链首是零倍率时来源应为 auto-zero，得到 %q", src)
	}
	if len(steps) != maxChainSteps {
		t.Fatalf("链长应被截到 %d，得到 %d", maxChainSteps, len(steps))
	}
	last := steps[len(steps)-1]
	if last.Model != "deepseek-v4-flash" || last.ZeroRate {
		t.Fatalf("最后一个坑应留给最低倍率付费兜底，得到 %+v", last)
	}
}

func TestChainCap(t *testing.T) {
	rates := map[string]string{"f1": "0", "f2": "0", "f3": "0", "f4": "0", "f5": "0"}
	steps, _ := Chain(Normalize(ModelLimit{ChainMode: ChainZero}), "cn", "m1", rates, nil, nil)
	if len(steps) != maxChainSteps {
		t.Errorf("链长应被截到 %d，得到 %d", maxChainSteps, len(steps))
	}
}

func TestBareForCrossRealm(t *testing.T) {
	if got := bareFor("cn:m1", "cn"); got != "m1" {
		t.Errorf("同域前缀应剥掉，得到 %q", got)
	}
	if got := bareFor("global:m1", "cn"); got != "global:m1" {
		t.Errorf("跨域目标应保留全名（页面据此提示跨域降级），得到 %q", got)
	}
}
