package usage

import (
	"testing"
	"time"
)

// ModelWindow 是模型限流映射的口径入口：按 (域, 账号, 模型) 聚合窗口内的消耗。
// 这里锁死三件事——聚合维度、排序、nil 安全；任一条变了都会直接影响限流判定。

// 聚合维度：同一 (域, 账号, 模型) 的多个桶必须合并成一行。
// 特别注意倍率不同的两个桶：分桶 key 里含「请求时生效的倍率」，所以同一账号同一
// 模型在倍率变化前后落在**两个**桶里，聚合时必须求和而不是各占一行。
func TestModelWindowAggregatesByRealmUIDModel(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 100, HasTotal: true, Credit: 1.5, HasCredit: true, ModelRate: "1.0"}, true)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 50, HasTotal: true, Credit: 0.5, HasCredit: true, ModelRate: "2.0"}, true)
	// 失败尝试也要计入请求数与失败数（重试放大就是靠这一列看出来的）。
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 7, HasTotal: true}, false)
	r.Add(now, "cn", "u2", "m1", Delta{TotalTokens: 10, HasTotal: true}, true)
	r.Add(now, "global", "u1", "m1", Delta{TotalTokens: 20, HasTotal: true}, true)

	rows := r.ModelWindow(Window{Hours: 24})
	if len(rows) != 3 {
		t.Fatalf("应有 3 行（cn/u1、cn/u2、global/u1），得到 %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Realm != "cn" || got.UID != "u1" || got.Model != "m1" {
		t.Fatalf("首行口径不符: %+v", got)
	}
	if got.Tokens != 157 {
		t.Fatalf("token 应为 100+50+7=157，得到 %d", got.Tokens)
	}
	if got.Requests != 3 {
		t.Fatalf("请求数应为 3，得到 %d", got.Requests)
	}
	if got.Errors != 1 {
		t.Fatalf("失败数应为 1，得到 %d", got.Errors)
	}
	if got.Credits != 2 {
		t.Fatalf("积分应为 1.5+0.5=2，得到 %v", got.Credits)
	}
}

// 排序：realm → model → uid。页面与请求路径都读这份结果，顺序必须稳定，
// 否则同一份数据两次拉取会抖动，测试也只能靠「找某一行」而不是直接断言行序。
func TestModelWindowSortedStable(t *testing.T) {
	r := New("")
	now := time.Now()
	d := Delta{TotalTokens: 1, HasTotal: true}
	// 故意乱序写入。
	r.Add(now, "global", "u1", "b-model", d, true)
	r.Add(now, "cn", "u2", "b-model", d, true)
	r.Add(now, "cn", "u1", "b-model", d, true)
	r.Add(now, "cn", "u2", "a-model", d, true)
	r.Add(now, "cn", "u1", "a-model", d, true)

	rows := r.ModelWindow(Window{Hours: 24})
	want := []string{
		"cn/a-model/u1",
		"cn/a-model/u2",
		"cn/b-model/u1",
		"cn/b-model/u2",
		"global/b-model/u1",
	}
	if len(rows) != len(want) {
		t.Fatalf("应有 %d 行，得到 %d: %+v", len(want), len(rows), rows)
	}
	for i, row := range rows {
		got := row.Realm + "/" + row.Model + "/" + row.UID
		if got != want[i] {
			t.Fatalf("第 %d 行应为 %s，得到 %s（整表: %+v）", i, want[i], got, rows)
		}
	}
}

// 窗口边界：窗口外的桶不进结果。边界用显式 From/To 定死（Hours 口径依赖“现在”，
// 断言会随时钟漂移）。
func TestModelWindowRespectsWindowBounds(t *testing.T) {
	r := New("")
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.Local)
	in := Delta{TotalTokens: 10, HasTotal: true}
	r.Add(base, "cn", "u1", "m1", in, true)                  // 10:00 桶
	r.Add(base.Add(2*time.Hour), "cn", "u1", "m1", in, true) // 12:00 桶

	from := base.Add(time.Hour)
	to := base.Add(90 * time.Minute)
	rows := r.ModelWindow(Window{From: from, To: to})
	if len(rows) != 0 {
		t.Fatalf("窗口 [11:00, 11:30] 内没有整点桶（10:00 早于 from、12:00 晚于 to），应得 0 行，得到 %d: %+v", len(rows), rows)
	}
}

// 窗口边界（含端点）：From 之前与 To 之后的桶都排除，端点上的桶保留。
func TestModelWindowKeepsBoundaryBucket(t *testing.T) {
	r := New("")
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.Local)
	in := Delta{TotalTokens: 10, HasTotal: true}
	r.Add(base, "cn", "u1", "m1", in, true)                  // 桶起点 10:00
	r.Add(base.Add(3*time.Hour), "cn", "u1", "m1", in, true) // 桶起点 13:00

	rows := r.ModelWindow(Window{From: base, To: base.Add(3 * time.Hour)})
	if len(rows) != 1 {
		t.Fatalf("From/To 端点上的桶都应保留，应得 1 行，得到 %d: %+v", len(rows), rows)
	}
	if rows[0].Tokens != 20 {
		t.Fatalf("两个桶应合并为 20 token，得到 %d", rows[0].Tokens)
	}
}

// nil 记录器必须安全：用量记录未启用时返回 nil（不是 panic）。
// 「没有记录」与「额度已用尽」是两件事，绝不能因为记录器缺席就把请求拦掉。
func TestModelWindowNilRecorder(t *testing.T) {
	var r *Recorder
	if rows := r.ModelWindow(Window{Hours: 24}); rows != nil {
		t.Fatalf("nil 记录器应返回 nil，得到 %+v", rows)
	}
}

// 零值窗口 = 全历史（与 Snapshot 同一口径），不能返回空表。
func TestModelWindowZeroWindowReturnsAll(t *testing.T) {
	r := New("")
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "m1", Delta{TotalTokens: 42, HasTotal: true}, true)

	rows := r.ModelWindow(Window{})
	if len(rows) != 1 || rows[0].Tokens != 42 {
		t.Fatalf("零值窗口应返回全部历史，得到 %+v", rows)
	}
}

// 域归一化与模型名前缀：桶里的域大小写不固定、模型名可能带 "cn:"/"global:" 前缀，
// 聚合前必须归一，否则同一个模型会散成好几行，限流按行统计就失真了。
func TestModelWindowNormalizesRealmAndModel(t *testing.T) {
	r := New("")
	now := time.Now()
	d := Delta{TotalTokens: 3, HasTotal: true}
	r.Add(now, "CN", "u1", "cn:m1", d, true)
	r.Add(now, "cn", "u1", "m1", d, true)

	rows := r.ModelWindow(Window{Hours: 24})
	if len(rows) != 1 {
		t.Fatalf("归一化后应合并为 1 行，得到 %d: %+v", len(rows), rows)
	}
	if rows[0].Realm != "cn" || rows[0].Model != "m1" {
		t.Fatalf("归一化口径不符: %+v", rows[0])
	}
	if rows[0].Tokens != 6 {
		t.Fatalf("token 应累加为 6，得到 %d", rows[0].Tokens)
	}
}

// FirstAt：窗口内最早一笔消耗所在的整点桶起点。限流页靠它推窗口边界，
// 所以必须取**最早**那个桶，而不是最后写入的那个桶。
func TestModelWindowFirstAtUsesEarliestBucket(t *testing.T) {
	r := New("")
	now := time.Now()
	// 故意后写早桶：写入顺序与桶的先后无关。
	r.Add(now.Add(-90*time.Minute), "cn", "u1", "m1", Delta{TotalTokens: 10, HasTotal: true}, true)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 20, HasTotal: true}, true)

	rows := r.ModelWindow(Window{Hours: 24})
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，得到 %d: %+v", len(rows), rows)
	}
	// 用与桶 scope 同一套格式换算，避免时区/整点截断方式不同带来的偏差。
	want, err := time.ParseInLocation(hourLayout, now.Add(-90*time.Minute).Format(hourLayout), time.Local)
	if err != nil {
		t.Fatalf("换算整点失败: %v", err)
	}
	if !rows[0].FirstAt.Equal(want) {
		t.Fatalf("FirstAt 应是最早那个整点桶 %v，得到 %v", want, rows[0].FirstAt)
	}
}
