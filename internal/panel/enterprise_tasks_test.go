package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestTaskJobsSkipNonPersonalAccounts 企业版 / 静态 API Key 无个人成长体系：
// 新账号自动任务、每日成长排程、手动 auto_all 都不得排作业或发起上游请求。
func TestTaskJobsSkipNonPersonalAccounts(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, _, _ := newTaskJobTestPanel(t, "tmp-personal", "cn", state)
	p.cfg.Pool.Remove("tmp-personal")

	ent := &auth.Auth{UID: "ent-1", Nickname: "enterprise", AccessToken: "token", EnterpriseID: "e1"}
	key := &auth.Auth{UID: "key-1", Nickname: "api key", AccessToken: "ck_demo.secret"}
	p.cfg.Pool.Add(ent)
	p.cfg.Pool.Add(key)

	// 登录 / 导入后的新账号自动触发。
	p.StartNewAccountTasks(ent.UID)
	p.StartNewAccountTasks(key.UID)
	// 每日成长排程触发。
	p.RunGrowthQueueNow()

	for _, uid := range []string{ent.UID, key.UID} {
		if job, ok := p.GetTaskJob(uid); ok {
			t.Fatalf("无个人成长体系账号 %s 不应产生后台任务：%+v", uid, job)
		}
		if _, _, err := p.StartAccountTaskJob(uid, "manual"); !errors.Is(err, errTaskJobNoGrowth) {
			t.Fatalf("手动启动 %s 的 error=%v，want errTaskJobNoGrowth", uid, err)
		}
	}

	// HTTP 入口同样明确拒绝（422），而不是静默排队后白跑。
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/ent-1/tasks/auto_all", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("auto_all status=%d body=%s, want 422", w.Code, w.Body.String())
	}

	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("无关账号不应创建任务作业：%+v", jobs)
	}
	if got := state.count(); got != 0 {
		t.Fatalf("门控账号不应发起任何上游请求，实际 %d 次", got)
	}
}

// TestRunAutoAllSkipsNonPersonalAccountsWithoutUpstream 兜底门控：即使绕过作业入口
// 直接调用全量执行 / 单项执行，也不得对任一无个人成长体系的账号发上游请求。
func TestRunAutoAllSkipsNonPersonalAccountsWithoutUpstream(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP = srv.Client()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL

	for _, a := range []*auth.Auth{
		{UID: "ent-1", AccessToken: "token", EnterpriseID: "e1"},
		{UID: "key-1", AccessToken: "ck_demo.secret"},
	} {
		p := panelWithTestAccount(a, up)
		results := p.runAutoAll(a)
		if len(results) != 1 || results[0]["status"] != "skipped" {
			t.Fatalf("runAutoAll(%s)=%#v, want one skipped result", a.UID, results)
		}
		if out := p.executeAutoTask(a, &autoActions[0]); out.Status != "skipped" {
			t.Fatalf("executeAutoTask(%s)=%+v, want skipped", a.UID, out)
		}
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("门控账号不应发起任何上游请求，实际 %d 次", got)
	}
}

// TestCockpitImportKeepsEnterpriseAndSkipsCheckin 导入路径必须保留 enterprise_id /
// enterpriseId（否则企业号会以个人号形态落盘，之后自动签到 / 任务全会跑），并让
// 导入后的签到同样走企业门控：只查余额，不发 daily-checkin。
func TestCockpitImportKeepsEnterpriseAndSkipsCheckin(t *testing.T) {
	e := upgNewEnv(t)
	expires := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	e.fake.registerEnterprise("ent-snake", 100, 500)
	e.fake.registerEnterprise("ent-camel", 100, 500)

	code, body := e.importCockpit([]map[string]any{
		{
			"uid": "ent-snake", "nickname": "snake", "access_token": "at-ent-snake",
			"refresh_token": "rt-ent-snake", "expires_at": expires, "enterprise_id": "e-snake",
		},
		{
			"uid": "ent-camel", "nickname": "camel", "access_token": "at-ent-camel",
			"refresh_token": "rt-ent-camel", "expires_at": expires, "enterpriseId": "e-camel",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", code, body)
	}

	for _, uid := range []string{"ent-snake", "ent-camel"} {
		a := e.pool.AuthByUID(uid)
		if a == nil || !a.IsEnterprise() {
			t.Fatalf("导入后 %s 应保留企业身份：%+v", uid, a)
		}
		// 只有一次余额查询；若还发了 daily-checkin，计数会是 2。
		if got := e.fake.callCount(uid); got != 1 {
			t.Fatalf("%s 导入后上游请求=%d，want 1（仅企业额度查询，不含签到）", uid, got)
		}
	}
}
