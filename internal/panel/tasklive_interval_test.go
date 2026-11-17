package panel

import "testing"

// 默认 5 分钟一轮（一轮 = 每个启用账号一次上游 GET），所有档位都不低于后端缓存 TTL；
// 轮询的 tick 不得改写用户选的间隔（上游把间隔抬到 TTL 却不重建定时器，只改了变量）。
func TestAppJSLiveTasksDefaultInterval(t *testing.T) {
	runLiveNode(t, "live-interval", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.deepStrictEqual([...timers.values()].map(t => t.ms), [120000], '默认 2 分钟一轮');
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120);
  const choices = vm.runInContext('LIVE_EVERY_CHOICES', ctx);
  assert(choices.every(s => s === 0 || s >= 60), '档位不低于后端缓存 TTL：' + choices);
  vm.runInContext("view = 'taskscenter'", ctx);
  [...timers.values()][0].fn(); await flush();
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120, 'tick 不得改写间隔');
`)
}
