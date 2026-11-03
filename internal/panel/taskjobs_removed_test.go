package panel

import (
	"testing"
)

// 删号后任务中心不再显示该账号的作业（issue #155）。
//
// 作业表按 uid 存，删号只把账号移出池子，作业记录还留着，于是「任务中心」一直显示一个
// 已经不存在的账号。ListTaskJobs 是面板唯一的列表来源（taskJobsHandler 直接转发它），
// 在那里按池子里还在的账号过滤即可，也顺带盖住进程重启后从持久化文件读回来的旧作业。
func TestListTaskJobsSkipsRemovedAccounts(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, _, _ := newTaskJobTestPanel(t, "task-job-removed", "cn", state)

	if _, started, err := p.StartAccountTaskJob("task-job-removed", "manual"); err != nil || !started {
		t.Fatalf("start job: started=%v err=%v", started, err)
	}
	waitTaskJobRequest(t, state)
	if jobs := p.ListTaskJobs(); len(jobs) != 1 {
		t.Fatalf("账号还在时应看到 1 个作业，实际 %+v", jobs)
	}

	p.cfg.Pool.Remove("task-job-removed")
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("账号删除后不该再显示它的作业：%+v", jobs)
	}
}
