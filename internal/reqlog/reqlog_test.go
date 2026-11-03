package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderMetricsAndRecent(t *testing.T) {
	r := New(Config{})
	r.Begin()
	r.Record(Event{RequestID: "ok", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 10})
	r.Begin()
	r.Record(Event{RequestID: "bad", Status: 429, OK: false, Outcome: OutcomeHTTPError, DurationMs: 30})

	s := r.Snapshot()
	if s.Completed != 2 || s.InFlight != 0 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if s.SuccessRate != 50 || s.HTTPSuccessRate != 50 || s.AvgDurationMs != 20 {
		t.Fatalf("rates = success:%v http:%v avg:%v", s.SuccessRate, s.HTTPSuccessRate, s.AvgDurationMs)
	}
	if len(s.Recent) != 2 || s.Recent[0].RequestID != "bad" || s.Recent[1].RequestID != "ok" {
		t.Fatalf("recent = %+v, want newest first", s.Recent)
	}
}

func TestArchiveRotationReadAndFilter(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 120, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 12; i++ {
		r.Record(Event{
			Time:       base.Add(time.Duration(i) * time.Second),
			RequestID:  "multi-" + string(rune('a'+i)),
			Model:      "glm-5.3",
			Account:    "账号(uid8)",
			Status:     200,
			OK:         true,
			Outcome:    OutcomeSuccess,
			DurationMs: int64(i + 1),
		})
	}
	r.Record(Event{
		Time:       base.Add(20 * time.Second),
		RequestID:  "other",
		Model:      "other-model",
		Status:     500,
		Outcome:    OutcomeHTTPError,
		DurationMs: 99,
	})
	r.Close()

	stats := r.Snapshot().Archive
	if !stats.Enabled || stats.Files < 2 || stats.Bytes == 0 || stats.DroppedWrites != 0 {
		t.Fatalf("archive stats = %+v", stats)
	}
	rows, err := r.ReadArchive(5, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].RequestID != "multi-l" || rows[4].RequestID != "multi-h" {
		t.Fatalf("filtered rows = %+v", rows)
	}
}

// 来源字段随事件落盘并可被 client_ip / user_agent 过滤（包含匹配、大小写不敏感）。
func TestArchiveFilterByClientInfo(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	rows := []Event{
		{RequestID: "a", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "203.0.113.7", UserAgent: "python-requests/2.31.0"},
		{RequestID: "b", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "198.51.100.4", UserAgent: "Mozilla/5.0 Chrome/120.0.0.0"},
		{RequestID: "c", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess},
	}
	for i, e := range rows {
		e.Time = base.Add(time.Duration(i) * time.Second)
		r.Record(e)
	}
	r.Close()

	got, err := r.ReadArchive(10, Filter{ClientIP: "203.0.113."})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "a" {
		t.Fatalf("client ip filter = %+v", got)
	}
	if got[0].UserAgent != "python-requests/2.31.0" {
		t.Fatalf("user agent not persisted: %+v", got[0])
	}

	// UA 片段过滤大小写不敏感（面板搜索框直接传用户输入）。
	got, err = r.ReadArchive(10, Filter{UserAgent: "chrome/120"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("ua filter = %+v", got)
	}

	// 来源为空的旧事件不该被非空来源条件误命中。
	got, err = r.ReadArchive(10, Filter{ClientIP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unmatched ip filter = %+v", got)
	}
}

// 时间区间过滤（「今天」/「自定义」）：闭区间，任一侧为零值即该侧不设界。
func TestArchiveFilterByTimeRange(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 30})
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Minute)
	ids := []string{"d3", "d2", "d1", "now"}
	for i, id := range ids {
		r.Record(Event{
			Time: base.Add(time.Duration(i) * 24 * time.Hour), RequestID: id,
			Status: 200, OK: true, Outcome: OutcomeSuccess,
		})
	}
	r.Close()

	rows, err := r.ReadArchive(10, Filter{From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rows))
	for _, e := range rows {
		got = append(got, e.RequestID)
	}
	// 倒序返回，闭区间命中 d2、d1。
	if len(got) != 2 || got[0] != "d1" || got[1] != "d2" {
		t.Fatalf("时间区间过滤 = %v, want [d1 d2]", got)
	}

	onlyFrom, err := r.ReadArchive(10, Filter{From: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyFrom) != 2 || onlyFrom[0].RequestID != "now" {
		t.Fatalf("仅 From 过滤 = %+v, want 2 条（d1、now）", onlyFrom)
	}

	onlyTo, err := r.ReadArchive(10, Filter{To: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyTo) != 1 || onlyTo[0].RequestID != "d3" {
		t.Fatalf("仅 To 过滤 = %+v, want 仅 d3", onlyTo)
	}

	// 时间区间与其它条件是与关系。
	combined, err := r.ReadArchive(10, Filter{
		From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour), Outcome: OutcomeHTTPError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 0 {
		t.Fatalf("区间+结果组合应为空（这几条都是成功）: %+v", combined)
	}
}

func TestArchiveQueueDropCounter(t *testing.T) {
	w := &archiveWriter{
		cfg:  Config{Enabled: true, Dir: t.TempDir()},
		ch:   make(chan Event, 1),
		done: make(chan struct{}),
	}
	w.ch <- Event{RequestID: "occupied"}
	w.enqueue(Event{RequestID: "dropped"})
	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d want 1", got)
	}
}

func TestArchivePruneHonorsSize(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"requests-2026-09-20.jsonl", "requests-2026-09-21.jsonl", "requests-2026-09-22.jsonl"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().AddDate(0, 0, -10+i)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	w := &archiveWriter{cfg: Config{Enabled: true, Dir: dir, RetentionDays: 30, MaxBytes: 50}, done: make(chan struct{})}
	w.prune()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "requests-2026-09-22.jsonl" {
		t.Fatalf("remaining = %+v, want newest file only", entries)
	}
}

// 一条超长事件行不能把整个视图读废。
//
// 现场：开启 logging.request_archive_context 后，带 base64 图片的请求把 Event.Context
// 撑到 1,098,299 字节；旧实现用 bufio.Scanner（上限 1<<20），超限时 readFile 直接把
// bufio.ErrTooLong 上抛，面板「请求事件日志」整页 500，导出 JSON 也废。
//
// 现在：超长行只丢这一行，同文件里其它事件照常返回，并给出跳过条数。
func TestReadFileSkipsOversizedLine(t *testing.T) {
	old := eventLineMaxBytes
	eventLineMaxBytes = 4 << 10 // 测试用的小上限，免得造 16 MB 文件
	defer func() { eventLineMaxBytes = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-11-03.jsonl")
	base := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)

	var b strings.Builder
	appendEvent := func(e Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	appendEvent(Event{Time: base, RequestID: "before", Status: 200, OK: true, Outcome: OutcomeSuccess})
	// 模拟「开了上下文归档 + 请求带图」：Context 是 8 KB 的 base64 串，整行远超上限。
	appendEvent(Event{
		Time: base.Add(time.Second), RequestID: "huge", Status: 200, OK: true, Outcome: OutcomeSuccess,
		Context: json.RawMessage(`"` + strings.Repeat("A", 8<<10) + `"`),
	})
	appendEvent(Event{Time: base.Add(2 * time.Second), RequestID: "after", Status: 200, OK: true, Outcome: OutcomeSuccess})

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rows, skipped, err := readFile(path, Filter{})
	if err != nil {
		t.Fatalf("readFile 返回错误（整页会 500）：%v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(rows) != 2 || rows[0].RequestID != "before" || rows[1].RequestID != "after" {
		t.Fatalf("rows = %+v, want before + after（超长行只丢自己）", rows)
	}
}

// 没超上限的长行（这里 8 KB > 2 KB 上限之外的对照组）必须正常解析：
// 上限只是防病态输入，不是把「大一点但正常」的事件也丢掉。
func TestReadFileKeepsLargeButAllowedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-11-03.jsonl")
	ctx := json.RawMessage(`"` + strings.Repeat("A", 512<<10) + `"`)
	e := Event{
		Time:      time.Date(2026, 11, 3, 11, 0, 0, 0, time.Local),
		RequestID: "big", Status: 200, OK: true, Outcome: OutcomeSuccess, Context: ctx,
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rows, skipped, err := readFile(path, Filter{})
	if err != nil || skipped != 0 || len(rows) != 1 || rows[0].RequestID != "big" {
		t.Fatalf("rows=%d skipped=%d err=%v，512 KB 的行在 16 MB 上限下应照常读到", len(rows), skipped, err)
	}
}
