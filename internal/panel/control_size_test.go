package panel

import (
	"os"
	"strings"
	"testing"
)

// TestCompactControlsSameHeight 钉住「同一行里的按钮与下拉同高」这条视觉约定。
//
// 为什么需要：卡片头 / 筛选栏里的控件此前各写各的尺子——
//   - .xs 只作用于 button，<select class="xs"> 落到全局 input/select 的
//     7px 11px + 14px，账号池「全部分组」、余额趋势「近 30 天」比旁边的
//     按钮高出一整档；
//   - .sel-note select 是第三套 3px / 12px / 圆角 7px；
//   - 请求事件日志的筛选下拉是 32px / 圆角 7px；
//   - 余额趋势的 .seg（国内·N / 国际·N）根本没有样式，里面的 <button>
//     吃全局 7px 13px / 14px。
//
// 统一口径（以用量页的按钮 / 下拉为基准）：高 28px、圆角 6px、字号 12.5px。
// 这里逐个锁住声明，防止下次改样式时又漂回「一行三个高度」。
func TestCompactControlsSameHeight(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	css := string(raw)
	rules := []string{
		"button.xs, select.xs",          // 卡片头 / 筛选栏的紧凑按钮与下拉
		".sel-note select",              // 「排序」「并发」这类带标签的小下拉
		".fbar > input, .fbar > select", // 模型档位 / 请求记录 / 积分历史的筛选栏
		".trange select, .trange input", // 时间范围控件（用量 / 请求记录）
		".events-filter-group select",   // 请求事件日志的筛选下拉
	}
	for _, sel := range rules {
		body, ok := cssRuleBody(css, sel)
		if !ok {
			t.Errorf("找不到 CSS 规则 %q", sel)
			continue
		}
		if !strings.Contains(body, "height: 28px") {
			t.Errorf("规则 %q 未把高度锁定为 28px（会靠 padding 撑，和旁边的按钮不等高）：%s", sel, body)
		}
		if !strings.Contains(body, "border-radius: 6px") {
			t.Errorf("规则 %q 的圆角应为 6px（与紧凑按钮同一档）：%s", sel, body)
		}
	}

	// 账号池的分组筛选必须挂 .xs：它没有别的选择器兜底，漏了这个 class
	// 就会回落到全局 select 的 38px 高度。
	if !strings.Contains(css, `<select id="accGroupFilter" class="xs"`) {
		t.Error(`账号池的分组筛选 #accGroupFilter 缺 class="xs"，会回落成全局 38px 下拉`)
	}
	// 余额趋势的域切换分段控件以前完全没有样式，必须至少写到内层按钮。
	if _, ok := cssRuleBody(css, ".seg button"); !ok {
		t.Error("余额趋势的域切换 .seg 没有样式：内层 <button> 会吃全局 7px 13px / 14px")
	}
}

// cssRuleBody 返回选择器列表与 sel 完全一致的 CSS 规则的花括号内声明。
// 只做字面前缀匹配（不解析层叠），够用于「某条规则里是否写了某个值」这类断言。
func cssRuleBody(css, sel string) (string, bool) {
	needle := sel + " {"
	i := strings.Index(css, needle)
	if i < 0 {
		return "", false
	}
	rest := css[i+len(needle):]
	j := strings.Index(rest, "}")
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:j]), true
}
