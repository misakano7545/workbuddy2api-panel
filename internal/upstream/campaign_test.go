package upstream

// campaign_test.go 钉住运营活动接口的解析与错误分类。
//
// 两个易回归点：
//   - progress 是按账号的独立数组，必须按 task_code 合并进任务表（不合并的话
//     所有任务都显示 0/N，看起来像「一个都没做」）；
//   - claim 的「条件不满足」（409/code 12304）是正常业务分支，不能当成故障。

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const sbuxTasksBody = `{"code":0,"msg":"OK","data":{
 "invite":{"count":0,"need":3,"open":false},
 "progress":[{"task_code":"ai_chat","period_key":"","progress":2,"viewed_at":"2026-11-12T09:16:12+08:00"}],
 "tasks":[
  {"id":5,"task_code":"ai_chat","title":"简单任务，AI 搞定","target_count":3,"reward_credit":100,"need_view":true,"sort":1,"enabled":1},
  {"id":7,"task_code":"expert","title":"复杂任务，专家帮忙","target_count":1,"reward_credit":100,"need_view":true,"sort":2,"enabled":1},
  {"id":3,"task_code":"invite","title":"邀请好友","target_count":1,"reward_credit":0,"need_view":false,"sort":0,"enabled":1,
   "stages":[{"code":"clicked","progress":0,"trigger":"click","qualify":false,"seq":1},
             {"code":"joined","progress":100,"trigger":"boost","qualify":true,"seq":3}]}
 ]}}`

const sbuxConfigBody = `{"code":0,"msg":"OK","data":{"config":{"activity_code":"campaign_starbucks_2026",
 "title":"星巴克协作","start_at":"2026-11-12T08:00:00+08:00","end_at":"2026-11-16T23:59:59+08:00",
 "in_period":true,"server_time":"2026-11-12T11:15:38+08:00"}}}`

const sbuxVoucherBody = `{"code":0,"msg":"OK","data":{"items":[
 {"prize_code":"sbux_americano","title":"美式券","status":"none","claimable":false,
  "today_remaining":0,"unused":60093,"stock_reset_at":1794528000}]}}`

func campaignTestClient(t *testing.T) *Client {
	t.Helper()
	return testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/config"):
			return jsonResp(200, sbuxConfigBody), nil
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			return jsonResp(200, sbuxTasksBody), nil
		case strings.HasSuffix(r.URL.Path, "/vouchers"):
			return jsonResp(200, sbuxVoucherBody), nil
		case strings.HasSuffix(r.URL.Path, "/viewed"):
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"ok":true}}`), nil
		case strings.HasSuffix(r.URL.Path, "/claim"):
			return jsonResp(409, `{"code":12304,"msg":"条件不满足"}`), nil
		}
		t.Errorf("unexpected path %q", r.URL.Path)
		return jsonResp(404, `{"code":404}`), nil
	})
}

func TestCampaignTasksMergesProgress(t *testing.T) {
	c := campaignTestClient(t)
	snap, err := c.CampaignTasks(&auth.Auth{UID: "u1", AccessToken: "at"}, "campaign", "campaign_starbucks_2026")
	if err != nil {
		t.Fatalf("CampaignTasks: %v", err)
	}
	if !snap.InPeriod || snap.Title != "星巴克协作" {
		t.Fatalf("config 未合并: %+v", snap)
	}
	if snap.Invite.Need != 3 || snap.Invite.Count != 0 {
		t.Fatalf("invite 未解析: %+v", snap.Invite)
	}
	if len(snap.Tasks) != 3 {
		t.Fatalf("tasks=%d want 3", len(snap.Tasks))
	}
	// ai_chat 的进度来自 progress 数组，必须合并进来
	var ai *CampaignTask
	for i := range snap.Tasks {
		if snap.Tasks[i].Code == "ai_chat" {
			ai = &snap.Tasks[i]
		}
	}
	if ai == nil || ai.Progress != 2 || ai.Target != 3 {
		t.Fatalf("ai_chat 进度未合并: %+v", ai)
	}
	if ai.Credit != 100 || !ai.NeedView {
		t.Fatalf("ai_chat 字段丢失: %+v", ai)
	}
	if CampaignTaskReady(*ai) {
		t.Fatal("2/3 不该判定为达标")
	}
	// 没有 progress 条目的任务保持 0（不能因为缺条目就漏掉）
	for _, tk := range snap.Tasks {
		if tk.Code == "expert" && (tk.Progress != 0 || tk.Target != 1) {
			t.Fatalf("expert 不该有进度: %+v", tk)
		}
	}
	if len(snap.Vouchers) != 1 || snap.Vouchers[0].Unused != 60093 {
		t.Fatalf("vouchers 未解析: %+v", snap.Vouchers)
	}
}

func TestCampaignTaskReady(t *testing.T) {
	cases := []struct {
		progress, target int
		want             bool
	}{
		{3, 3, true},
		{2, 3, false},
		{0, 1, false},
		{1, 1, true},
		{5, 0, false}, // target 缺失（上游没下发）不算达标
	}
	for _, c := range cases {
		got := CampaignTaskReady(CampaignTask{Progress: c.progress, Target: c.target})
		if got != c.want {
			t.Fatalf("ready(%d/%d)=%v want %v", c.progress, c.target, got, c.want)
		}
	}
}

func TestCampaignClaimNotMetIsBranchNotFault(t *testing.T) {
	c := campaignTestClient(t)
	err := c.CampaignClaimTask(&auth.Auth{UID: "u1", AccessToken: "at"}, "campaign", "campaign_starbucks_2026", "ai_chat")
	if err == nil {
		t.Fatal("未达标应返回错误")
	}
	if !IsCampaignNotMet(err) {
		t.Fatalf("12304 应识别为「未达标」而非故障: %v", err)
	}
	var ce *CampaignError
	if !errors.As(err, &ce) || ce.Code != 12304 || ce.Status != 409 {
		t.Fatalf("错误未保留 code/status: %+v", ce)
	}
}

func TestCampaignTaskCodeIsPathEscaped(t *testing.T) {
	var gotPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.EscapedPath()
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"ok":true}}`), nil
	})
	if err := c.CampaignViewTask(&auth.Auth{UID: "u1", AccessToken: "at"}, "campaign", "c1", "a/b c"); err != nil {
		t.Fatalf("viewed: %v", err)
	}
	if !strings.Contains(gotPath, "a%2Fb%20c") {
		t.Fatalf("任务 code 未转义，可能拼出越界路径: %q", gotPath)
	}
}
