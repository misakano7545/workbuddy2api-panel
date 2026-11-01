package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLogsHandlerSinceIncremental /panel/api/logs?since= 的增量语义：
// 只回该时刻之后的条目；不带 since 时回全量。面板「运行日志」页每 5 秒拉一次，
// 常态只需要新增行——全量是 500 行（~90KB），每轮重拉 + 重建 DOM 都在做无用功。
//
// 注意 Ring 的 TS 是**写入时刻**（不是日志行里的时间戳），所以增量判据用
// 「写入时刻晚于 since」——多条日志同一批写入时时间戳相同，此时 since 取的是
// 上一批最后一条的 ts，同批的条目靠「>」判定会被漏掉。
// 这正是前端用「已见到的最大 ts + 同一批兜底」策略的原因（见 app.js 的 logSinceTs）。
// 本测试用「分批写入 + 显式 sleep」构造可区分的时间戳。
func TestLogsHandlerSinceIncremental(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	// 第一批：2 行。
	p.logs.Write([]byte("2026/10/24 12:00:00 line-0\n2026/10/24 12:00:01 line-1\n"))
	first := p.logs.Snapshot()
	if len(first) != 2 {
		t.Fatalf("第一批应为 2 条，实际 %d", len(first))
	}

	// 隔开一点再写第二批，保证 TS 可区分。
	time.Sleep(5 * time.Millisecond)
	p.logs.Write([]byte("2026/10/24 12:00:02 line-2\n2026/10/24 12:00:03 line-3\n"))

	get := func(since string) []LogEntry {
		u := "/panel/api/logs"
		if since != "" {
			u += "?since=" + since
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Entries []LogEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Entries
	}

	all := get("")
	if len(all) != 4 {
		t.Fatalf("全量应为 4 条，实际 %d", len(all))
	}

	// since = 第一批最后一条的 ts → 只回第二批的 2 条。
	inc := get(first[len(first)-1].TS.Format(time.RFC3339Nano))
	if len(inc) != 2 {
		t.Fatalf("增量应为 2 条，实际 %d（%+v）", len(inc), inc)
	}
	if inc[0].Text != "line-2" || inc[1].Text != "line-3" {
		t.Fatalf("增量内容不对：%q %q", inc[0].Text, inc[1].Text)
	}

	// 边界：since 早于全部 → 全量；since 晚于全部 → 空。
	if got := get(time.Now().Add(-time.Hour).Format(time.RFC3339Nano)); len(got) != 4 {
		t.Fatalf("since 早于全部应为 4 条，实际 %d", len(got))
	}
	if got := get(time.Now().Add(time.Hour).Format(time.RFC3339Nano)); len(got) != 0 {
		t.Fatalf("since 晚于全部应为 0 条，实际 %d", len(got))
	}

	// 非法 since → 回落全量（不报错，也不返回空——宁可多给不可少给）。
	if got := get("not-a-time"); len(got) != 4 {
		t.Fatalf("非法 since 应回落全量（4 条），实际 %d", len(got))
	}
}

// TestParseSinceParamPlusDecodedAsSpace RFC3339 的正时区偏移用 "+"（+08:00），
// 而 query string 里的 "+" 会被解码成空格。客户端直接拼 URL（不编码）时
// since 会变成 "2026-...T08:59:32.05 08:00"——必须能解析，否则增量静默失效
// （回落全量，表现为「优化没生效」而不是报错，最难查）。
func TestParseSinceParamPlusDecodedAsSpace(t *testing.T) {
	ts := time.Date(2026, 10, 25, 8, 59, 32, 50751881, time.Local)
	std := ts.Format(time.RFC3339Nano)

	got, ok := parseSinceParam(std)
	if !ok {
		t.Fatalf("标准 RFC3339 应能解析: %q", std)
	}
	if !got.Equal(ts) {
		t.Fatalf("解析结果不等: %v vs %v", got, ts)
	}

	// 模拟 "+" 被解成空格。
	spaced := strings.Replace(std, "+", " ", 1)
	got2, ok2 := parseSinceParam(spaced)
	if !ok2 {
		t.Fatalf("「+」被解成空格的形态应能解析: %q", spaced)
	}
	if !got2.Equal(ts) {
		t.Fatalf("解析结果不等: %v vs %v", got2, ts)
	}

	// 真正非法的 → 失败（调用方回落全量）。
	if _, ok3 := parseSinceParam("not-a-time"); ok3 {
		t.Fatal("非法值不应解析成功")
	}
	if _, ok4 := parseSinceParam(""); ok4 {
		t.Fatal("空值不应解析成功")
	}
}

// TestRequestMetricsRecentOptOut /panel/api/request_metrics?recent=0 时不返回
// recent 数组（只回指标与归档状态）。面板在归档开启时用归档数据当请求记录表，
// recent 那 100 条（~35KB）用不上——每 5 秒白拉一次、白解析、白 GC。
func TestRequestMetricsRecentOptOut(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	// RequestLog 为 nil 时端点返回 501——本测试验证各参数组合都不会 500，
	// 且参数解析不因取值不同而改变响应码。真正的「有没有 recent」由 reqlog 层的
	// Snapshot 裁剪（服务端在 recent=0 时把 Recent 置 nil）。
	for _, q := range []string{"", "?recent=0", "?recent=false", "?recent=1"} {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/request_metrics"+q, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("q=%q 未装配 RequestLog 时应 501，实际 %d", q, rec.Code)
		}
	}
}

// TestLogsSinceSeqAdvancesWithinSameTick 同一时钟刻度内的多条日志，用 since_seq
// 增量必须能逐条推进、不重复、不漏行。
//
// 这是核心回归位：TS 来自 time.Now()，Windows 实测 ~15.6ms 粒度，一次 Write 的
// 多行、背靠背的多次 Write 全都拿到**完全相同**的 TS。按 TS 过滤时
// 「晚于最后一条的 ts」永远为空或永远为真——要么漏、要么重复刷最后一条
// （用户看到的现象）。seq 单调递增，不受时钟精度影响。
func TestLogsSinceSeqAdvancesWithinSameTick(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	// 一次 Write 多行：这些行必然共享同一个 TS（同一次 time.Now()）。
	p.logs.Write([]byte("a\nb\nc\n"))

	getSeq := func(seq uint64) []LogEntry {
		u := "/panel/api/logs?since_seq=" + strconv.FormatUint(seq, 10)
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		var out struct {
			Entries []LogEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Entries
	}

	all := getSeq(0)
	if len(all) != 3 {
		t.Fatalf("since_seq=0 应回全部 3 条，实际 %d", len(all))
	}
	// 三行 TS 相同（同一批写入）——正是 ts 游标失效的场景。
	if !all[0].TS.Equal(all[2].TS) {
		t.Skip("本机时钟足够精细，三行未共享 TS；该用例无法体现同刻度场景")
	}
	// seq 必须严格递增且唯一。
	if !(all[0].Seq < all[1].Seq && all[1].Seq < all[2].Seq) {
		t.Fatalf("seq 未单调递增: %d %d %d", all[0].Seq, all[1].Seq, all[2].Seq)
	}

	// 用第 1 条的 seq 推进 → 只应拿到后 2 条（不重复第 1 条）。
	rest := getSeq(all[0].Seq)
	if len(rest) != 2 || rest[0].Text != "b" || rest[1].Text != "c" {
		t.Fatalf("since_seq 推进后应得 b,c，实际 %+v", rest)
	}
	// 用最后一条的 seq 推进 → 空（游标真正到位，不再重复最后一条）。
	if last := getSeq(all[2].Seq); len(last) != 0 {
		t.Fatalf("游标到末尾后应返回 0 条，实际 %d 条（这就是「日志重复最后一条」的成因）", len(last))
	}
}

// TestLogsSinceTSStillDropsTailWithinSameTick 钉住 ?since=（旧接口）的已知局限：
// 同一刻度的多条无法增量区分，按 ts 推进会漏掉同刻度的后几行。保留 since 只为
// 兼容旧前端/手工 curl；新代码一律用 since_seq。
func TestLogsSinceTSStillDropsTailWithinSameTick(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	p.logs.Write([]byte("a\nb\nc\n"))

	all := p.logs.Snapshot()
	if !all[0].TS.Equal(all[2].TS) {
		t.Skip("本机时钟足够精细，无法构造同刻度场景")
	}
	u := "/panel/api/logs?since=" + url.QueryEscape(all[0].TS.Format(time.RFC3339Nano))
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out struct {
		Entries []LogEntry `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// 同一 TS → After 全为假 → 后两行被漏掉。这是 since 的固有限制。
	if len(out.Entries) != 0 {
		t.Fatalf("同刻度时 since 无法推进（应漏掉全部），实际返回 %d 条", len(out.Entries))
	}
}

// TestLogsSeqMonotonicAcrossRestart 重启后新环的 seq 必须大于旧环的 seq。
//
// 否则浏览器里残留的旧游标（重启前可能已到几万）会大于新进程的序号，
// ?since_seq= 一直返回空——「运行日志」页空白，最长要等游标老化（5 分钟）才恢复。
// 做法：seq 初值取启动时刻毫秒数（见 NewRing）。
func TestLogsSeqMonotonicAcrossRestart(t *testing.T) {
	old := NewRing(10)
	old.Write([]byte("before-restart\n"))
	oldTip := old.Snapshot()[0].Seq

	time.Sleep(2 * time.Millisecond)
	fresh := NewRing(10)
	fresh.Write([]byte("after-restart\n"))
	newTip := fresh.Snapshot()[0].Seq

	if newTip <= oldTip {
		t.Fatalf("重启后 seq 未保持单调：旧 %d → 新 %d（旧游标会把新日志全部过滤掉）", oldTip, newTip)
	}
}
