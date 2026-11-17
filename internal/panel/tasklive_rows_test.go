package panel

import "testing"

// 任务行必须真的渲染出内容：liveTaskRow 曾返回 qrowHTML 的 HTML，渲染时又被当作
// 行对象再喂一次 qrowHTML，整行变成空白与 "undefined"。
func TestAppJSLiveTaskRowsRender(t *testing.T) {
	runLiveNode(t, "live-rows", `
  liveReply = {age_sec: 0, accounts: [
    {uid: 'cn-1', nickname: 'n1', pending: 1, claimable: 0, claimed: 1, tasks: [
      {task_code: 'chat_5', title: '发起 5 次对话', progress_known: true, current: 2, target: 5, credit: 100},
      {task_code: 'first_buddy', title: '领取一只 Buddy', claimed: true}]},
    {uid: 'cn-2', nickname: 'n2', error: 'token expired'}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const html = nodes.get('liveList').innerHTML;
  assert(html.includes('chat_5') && html.includes('发起 5 次对话') && html.includes('2/5'), '任务行要有内容：' + html);
  assert(!html.includes('undefined'), '任务行不应出现 undefined：' + html);
  assert(html.includes('data-live-uid="cn-1"') && html.includes('token expired'), html);
`)
}
