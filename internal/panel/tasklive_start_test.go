package panel

import "testing"

// 实时任务卡只在进入页面时启动一次：reattachQueueView 会被 5s 全局轮询（refreshVisible）
// 反复调用，启动若挂在它里面，用户选的刷新间隔与「关」就形同虚设。
func TestAppJSLiveTasksStartsOnlyOnEntry(t *testing.T) {
	runLiveNode(t, "live-start", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.strictEqual(liveCalls(), 1, '进入本页拉一次');
  assert.strictEqual(timers.size, 1);
  const timerIds = [...timers.keys()].join();
  for (let i = 0; i < 12; i++) { vm.runInContext('reattachQueueView()', ctx); await flush(); }
  assert.strictEqual(liveCalls(), 1, '全局轮询不该驱动实时任务');
  assert.strictEqual([...timers.keys()].join(), timerIds, '全局轮询不该重置定时器');

  // 「关」：定时器撤掉，再进页面只拉一次、不再起定时器
  nodes.get('liveEvery').onchange({target: {value: '0'}});
  assert.strictEqual(timers.size, 0);
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.strictEqual(timers.size, 0, '关闭后进入本页不该起定时器');
  assert.strictEqual(liveCalls(), 2);
`)
}
