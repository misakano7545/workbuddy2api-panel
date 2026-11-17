package panel

import "testing"

// 数据没变就不重画：账号多时一轮是几百个分组，每个间隔重建一遍 DOM 纯属浪费，还会
// 冲掉用户正在选中的文字。失败提示在恢复后要收起——包括「数据没变」直接早退的路径。
func TestAppJSLiveTasksSkipsUnchangedRedraw(t *testing.T) {
	runLiveNode(t, "live-redraw", `
  liveReply = {age_sec: 0, accounts: [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const writes = innerWrites;
  assert(writes > 0);
  liveReply = {age_sec: 30, accounts: liveReply.accounts};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes, '数据没变不该重画');
  assert(nodes.get('liveSummary').textContent.includes('30 秒前'), nodes.get('liveSummary').textContent);

  apiFail = true;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(nodes.get('liveNote').hidden, false);
  apiFail = false;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes);
  assert.strictEqual(nodes.get('liveNote').hidden, true, '数据没变时恢复后提示也要收起');
`)
}
