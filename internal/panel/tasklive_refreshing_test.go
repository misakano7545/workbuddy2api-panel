package panel

import "testing"

// 后端那一轮还没跑完时返回的是上一份快照：页面要几秒后自动再取，否则卡片总是比数据
// 慢一个轮询间隔（默认 5 分钟）。这只是读服务端缓存；一轮结束（refreshing=false）就停。
func TestAppJSLiveTasksFollowsUpWhileRefreshing(t *testing.T) {
	runLiveNode(t, "live-refreshing", `
  const rows = [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}];
  liveReply = {refreshing: true, accounts: rows};
  vm.runInContext("view = 'taskscenter'", ctx);
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 1, '还在刷新就该续约一次再取');
  const before = liveCalls();
  [...timeouts.values()][0].fn(); await flush();
  assert.strictEqual(liveCalls(), before + 1);

  liveReply = {refreshing: false, accounts: rows};
  timeouts.clear();
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 0, '一轮结束不再续约');
`)
}
