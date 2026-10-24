'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
/* 账号备注的本机键（模块级声明放顶部，避开 TDZ）。 */
const LS_REMARKS = 'wb2api.remarks.v1';
let remarksCache = null;
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null, updateData = null, updateBusy = false;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
/* 收纳行（「其余 N 个账号，展开」）的展开状态。必须存在 DOM 之外：账号池/任务中心视图
   每 5s 轮询重建 tbody，只把状态放在 aria-expanded / hidden 里会被下一轮渲染收回
   （表现为点「展开」后什么都不点又自动收起）。键 = tbody id。 */
const restOpen = Object.create(null);
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let mdLocks = [], mdRecent = [];
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 手机抽屉导航（≤760px；桌面按钮隐藏、类名无副作用） ─────────────── */
const navEl = document.querySelector('.nav'), navScrim = $('navScrim'), btnNav = $('btnNav');
function navSet(open) {
  if (!navEl || !navScrim) return;
  navEl.classList.toggle('open', open);
  navScrim.classList.toggle('on', open);
  document.body.classList.toggle('nav-open', open);
  if (btnNav) btnNav.setAttribute('aria-expanded', String(open));
}
if (btnNav && navScrim) {
  btnNav.onclick = () => navSet(!navEl.classList.contains('open'));
  navScrim.onclick = () => navSet(false);
}
addEventListener('keydown', e => { if (e.key === 'Escape') navSet(false); });

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  // status 挂到错误上：调用方靠它区分「可重试的抖动」与「重试无意义的终态」
  // （登录轮询据此决定继续等 OAuth 还是提示重新发起）。
  if (r.status === 401) { openKey(); const e = new Error('密钥无效或未填写'); e.status = 401; throw e; }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(d.error || ('HTTP ' + r.status)); e.status = r.status; throw e; }
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）。
// 「全部历史」：**用量**必须显式发 hours=0 —— 服务端「不给参数」的默认是 72 小时，
// 空查询会让「全部历史」静默变成「近 3 天」（issue #121 的形态：全部历史 8903 次请求
// 与近 3 天一模一样，而真全部历史是 24817 次）。归档接口不认 hours，保持空查询
// （它的默认是「最近 N 条」，本身就是全量日志的最新一段）。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at); // 上游文案时间（仅被 soft_rate_max 截断时才与 until 不同）
  const until = parseAPITime(row && row.until);      // 网关实际解锁时刻（黄框消失即它）
  // 倒计时与「剩余」一律跟 until：跟上游文案走会出现「显示还剩 50 分钟却已经掉框」，
  // 再发一次请求黄框又弹回来的错觉（issue #171-2）。模型与档位页的锁池表也是 until 口径。
  const deadline = until || resetAt;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail;
  if (resetAt && until && resetAt > until) {
    // 双口径：上游文案晚于网关封顶解锁时刻——两个都写出来，剩余跟网关口径。
    detail = '上游称 ' + fmtLocalDateTime(resetAt) + ' 恢复 · 网关 ' + fmtLocalDateTime(until) +
      ' 起重试' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '');
  } else if (resetAt) {
    detail = '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '');
  } else if (until) {
    detail = '预计 ' + fmtLocalDateTime(until) + ' 恢复' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '');
  } else {
    detail = '预计解封时间未知';
  }
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && (!resetAt || until !== resetAt)) {
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
// disabledKind 把禁用 reason 归成「类别 + 语气 + 建议动作」。
//
// 为什么要分类而不是直接显示 reason：四种禁用成因的**处置方式完全不同** ——
// 上游封禁要申诉、登录失效要重登、手动禁用要人来判断、12153 也是重登。
// 只甩一行原始文本，运维得先读懂才敢动手；分成标签后「该做什么」一眼可见。
// 判据按**优先级**匹配：11140/封禁类先判（最严重且处置唯一），再判登录失效。
function disabledKind(reason) {
  const r = String(reason || '').toLowerCase();
  if (/11140|banned|restricted|request illegal/.test(r)) {
    return { label: '上游封禁', tone: 'bad', action: '需在官方申诉，revive 无效' };
  }
  if (/refresh.*失败|refresh 验证失败/.test(r)) {
    return { label: '登录失效', tone: 'bad', action: '需重新登录' };
  }
  if (/12153/.test(r)) {
    return { label: '登录失效', tone: 'bad', action: '需重新登录' };
  }
  if (/401/.test(r)) {
    return { label: '鉴权失败', tone: 'warn', action: '确认后重登或恢复' };
  }
  if (/manual|手动/.test(r)) {
    return { label: '手动禁用', tone: 'warn', action: '随时可恢复' };
  }
  return { label: '已禁用', tone: 'warn', action: '' };
}

// acctStateOf 按 uid 从 overview 快照里取账号状态（禁用/暂停）。
//
// 为什么余额趋势要标：这张表每一行是一条**历史余额曲线**，它不会因为账号被封
// 而消失——但读者不知道「这条线为什么从某天起不再更新」，会把「号被封了」误读成
// 「这段时间没怎么用」。标出来才对得上。
function acctStateOf(uid) {
  const list = (overviewData && overviewData.accounts) || [];
  return list.find(a => a && a.uid === uid) || null;
}

// disabledAge 禁用时长（「刚禁”和「禁了一周」处置优先级差很远）。
function disabledAge(at) {
  if (!at) return '';
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return '';
  const sec = Math.max(0, (Date.now() - t) / 1000);
  if (sec < 60) return '刚刚';
  if (sec < 3600) return Math.floor(sec / 60) + ' 分钟前';
  if (sec < 86400) return Math.floor(sec / 3600) + ' 小时前';
  return Math.floor(sec / 86400) + ' 天前';
}

function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  // 降级路由表（overview 的 model_fallback）：限流的模型若配了等价模型，标注
  // 「⤳ 已路由至 X」—— 否则用户看到「v4.1 限流」会以为服务断了，实际请求已被
  // 自动改道到等价模型继续服务。这条是用户实测反馈要求可见的。
  const fb = (overviewData && overviewData.model_fallback) || {};
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    // 双重查找：上游台账里的 model 可能带 realm 前缀（cn:x），配置里是裸名。
    const bare = String(m.model || '').replace(/^[a-z]+:/, '');
    const alt = fb[row.model] || fb[bare] || '';
    const route = alt ? '<span class="fb">⤳ 已路由至 ' + esc(alt) + '</span>' : '';
    const title = alt ? m.title + '；已自动改道到 ' + alt : m.title;
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span>' +
      route + '</div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

// 登出：api_key 是唯一凭据（服务端只做 VerifyBearer 比对，无会话无 cookie 可失效），
// 所以"登出"= 清掉本机保存的密钥并重载——重载把内存里的定时器与已渲染数据一并清空，
// 无密钥时 start() 的首次请求 401 会自动弹回密钥门（data-lock="1"，不可绕过）。
$('btnLogout').onclick = () => {
  if (!confirm('登出将清空本机保存的 API 密钥，需要重新输入才能进入面板。确认登出？')) return;
  localStorage.removeItem(LS_KEY);
  location.reload();
};

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', usage: '用量', packages: '积分构成', balhist: '余额趋势', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  navSet(false);
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'balhist') loadBalance();
  // 补偿状态按需拉取：compData 为空才打上游（失败留空 → 下次进入重试）。
  if (v === 'taskscenter') { reattachQueueView(); if (!compData) loadCompensation(); }
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
function renderAccounts(list) {
  const tb = $('accBody');
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    // 暂停选号：与禁用同属「不可选」，但照常保号（吸收上游 fd835df）
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    }
    else if (s.reserve_blocked) { cls = 'cool'; tag = '<span class="tag warn">保留积分</span>'; }
    // 会话疑似失效（12153 连续计数已 >0、还没到禁用阈值）：上游已判死、号还留在池里。
    // 此前这种中间态完全不可见，面板照样打「可用」（issue #215 复盘）。
    else if (s.session_dead_fails > 0) { cls = 'cool'; tag = '<span class="tag warn">疑似登录失效 · 待重登</span>'; }
    else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    // 禁用原因分两行：类别+时长（谁来处置、多久了）/ 原始 reason（细节）。
    // 不分行的话「上游封禁 · 3 天前」和一段长 reason 挤在同一行，扫视时读不出重点。
    const dk = s.disabled ? disabledKind(s.reason) : null;
    const dAge = s.disabled ? disabledAge(s.disabled_at) : '';
    const note = s.reason
      ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' +
        (dk ? '<span class="tag ' + dk.tone + '" style="margin-right:6px">' + esc(dk.label) + '</span>' : '') +
        (dAge ? '<span style="margin-right:6px">' + esc(dAge) + '</span>' : '') +
        (dk && dk.action ? '<span>' + esc(dk.action) + '</span>' : '') +
        '<div style="margin-top:2px;opacity:.85">' + esc(s.reason) + '</div></div>'
      : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    // 无个人成长体系的账号（企业版 / 静态 API Key）：签到与成长任务端点在上游对自己
    // 恒失败，按钮点了只会报错，故不渲染；「额度」按钮走企业口径端点照常可用。
    const personal = !(s.enterprise || s.api_key);
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + remarkName(s.uid, s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + (s.api_key ? ' <span class="realm-tag">API Key</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts"><div class="acts-inner">' + [
        // 无个人成长体系的账号（企业版 / 静态 API Key）不渲染「签到」「任务」：前者
        // 上游 400「企业账号不支持该操作」，后者 403 growth-only；只留「额度」，点它走
        // /balance，额度由 upstream 的企业口径端点（get-enterprise-user-usage）填充。
        // 七个动作按固定槽位输出，缺位留空 span —— CSS 用 grid 固定列宽，逐行对齐。
        // 「探活」单独占一槽（仅封禁态渲染）：它与「解冻」是两个独立动作，若挤在同一
        // 槽里输出两个按钮，冻结行的列数就与其它行不同，整列又会错位。
        (personal ?
          '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>' : ''),
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>',
        (personal ?
          '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>' : ''),
        (frozen ? '<button class="xs ghost" data-a="probe_ban" data-u="' + esc(s.uid) + '" title="只读探测上游是否已解封：不 revive、不参与选号、不改任何状态">探活</button>' : ''),
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
                            : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (personal ? '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额' : '退出选号，但照常保活 / 刷新额度') + '">暂停选号</button>')),
        (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>'),
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>',
      ].map(html => html || '<span class="slot" aria-hidden="true"></span>').join('') +
      '</div></td></tr>';
  }).join('');
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。后端已按
// 「整池不可用 → 此刻没号 → 部分限流」排好序，这里只做展示（移植上游 PR #120）。
const ML_STATE = { locked: ['bad', '全池锁定'], starved: ['warn', '此刻无号'], partial: ['ok', '部分限流'] };
function renderModelLocks(rows) {
  const tb = $('mlBody');
  const list = Array.isArray(rows) ? rows : [];
  $('mlNote').textContent = list.length ? list.length + ' 个模型受限' : '';
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    return;
  }
  tb.innerHTML = list.map(r => {
    const [cls, label] = ML_STATE[r.state] || ['', r.state || '—'];
    const unlock = r.unlock_at && !/^0001-/.test(r.unlock_at) ? fmtLocalDateTime(r.unlock_at) : '—';
    const fully = r.fully_unlock_at && !/^0001-/.test(r.fully_unlock_at) ? fmtLocalDateTime(r.fully_unlock_at) : '—';
    return '<tr>' +
      '<td><b>' + esc(r.model) + '</b></td>' +
      '<td>' + (r.realm === 'global' ? '<span class="realm-tag">国际版</span>' : '国内版') + '</td>' +
      '<td><span class="tag ' + cls + '">' + label + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + esc(unlock) + '</td>' +
      '<td class="num">' + esc(fully) + '</td>' +
      '<td title="' + esc(r.reason || '') + '">' + esc(r.reason || '—') + '</td>' +
      '</tr>';
  }).join('');
}

function displayVersion(version) {
  const value = String(version || 'dev').replace(/^v/, '');
  return value === 'dev' ? 'dev' : 'v' + value;
}

function renderUpdateStatus(update) {
  if (!update) return;
  updateData = update;
  const local = displayVersion(update.current_version);
  $('navSub').textContent = local;
  $('navVer').textContent = local;
  $('navUpdate').hidden = !update.update_available;
  $('navUpdate').textContent = '有更新';
  $('navUpdate').title = update.update_available ? '发现 v' + update.latest_version + '，点击查看更新操作' : '';
  const note = $('updateNote');
  if (update.installing) {
    note.textContent = '正在准备更新…';
  } else if (update.last_error) {
    note.textContent = '更新失败：' + update.last_error;
  } else if (update.update_available) {
    note.textContent = '发现新版本 v' + update.latest_version + (update.can_install ? '' : ' · 请手动更新');
  } else if (update.latest_version) {
    note.textContent = '已是最新版本 · v' + update.latest_version;
  } else {
    note.textContent = '尚未检查';
  }
  $('btnUpdateInstall').hidden = !update.update_available || !update.can_install;
  $('btnUpdateInstall').disabled = updateBusy || !!update.installing;
  $('btnUpdateRelease').hidden = !update.update_available || !!update.can_install || !update.release_url;
  $('btnUpdateCheck').disabled = updateBusy || !!update.installing;
}

async function checkForUpdates() {
  if (updateBusy || updateData?.installing) return;
  updateBusy = true;
  const button = $('btnUpdateCheck');
  button.disabled = true;
  button.textContent = '检查中…';
  try {
    const update = await api('update/check', { method: 'POST' });
    renderUpdateStatus(update);
    toast(update.update_available ? '发现新版本 v' + update.latest_version : '当前已是最新版本', update.update_available ? 'ok' : '');
  } catch (e) {
    try { renderUpdateStatus(await api('update')); } catch (_) {}
    toast('检查更新失败：' + e.message, 'err');
  } finally {
    updateBusy = false;
    if (updateData) renderUpdateStatus(updateData);
    button.textContent = '检查更新';
  }
}

async function installUpdate() {
  if (updateBusy || !updateData || updateData.installing || !updateData.can_install || !updateData.update_available) return;
  if (!confirm('将安装 v' + updateData.latest_version + ' 并重启服务，期间服务会短暂中断。请避开正在处理的请求，确认继续？')) return;
  const button = $('btnUpdateInstall');
  updateBusy = true;
  button.disabled = true;
  button.textContent = '准备更新…';
  try {
    const result = await api('update/install', { method: 'POST' });
    renderUpdateStatus(result.status);
    toast('更新已准备，服务即将重启', 'ok');
  } catch (e) {
    toast('更新失败：' + e.message, 'err');
  } finally {
    updateBusy = false;
    if (updateData) renderUpdateStatus(updateData);
    button.textContent = '立即更新';
  }
}

function openLatestRelease() {
  if (updateData && updateData.release_url) open(updateData.release_url, '_blank', 'noopener');
}

if ($('btnUpdateCheck')) $('btnUpdateCheck').onclick = checkForUpdates;
if ($('btnUpdateInstall')) $('btnUpdateInstall').onclick = installUpdate;
if ($('btnUpdateRelease')) $('btnUpdateRelease').onclick = openLatestRelease;
if ($('navUpdate')) $('navUpdate').onclick = () => {
  go('config');
  $('updateNote').scrollIntoView({ block: 'center', behavior: 'smooth' });
};

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    const previousUpdate = typeof updateData !== 'undefined' ? updateData : null;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
  $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    if (typeof renderUpdateStatus === 'function') renderUpdateStatus(d.update || { current_version: d.version });
    if (d.update && d.update.update_available &&
      (!previousUpdate || !previousUpdate.update_available || previousUpdate.latest_version !== d.update.latest_version)) {
      toast('发现新版本 v' + d.update.latest_version, 'ok');
    }
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    // 到期提醒随首页统计一起刷新：只用响应里已有的最早到期快照，零上游请求。
    renderExpirySnapshot(d);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      const note = checkinResultNote(r);
      toast(note.message, note.severity);
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'probe_ban') {
      // 只读探测：后端不改任何池状态，所以这里**不刷新** overview（没有变化）。
      // 结论用 toast 说清处置动作，而不是只报一句「仍被封」——运维要知道下一步做什么。
      const r = await api('accounts/' + encodeURIComponent(u) + '/probe_ban', { method: 'POST' });
      const map = {
        lifted: ['已解封，可以点「解冻」恢复使用', 'ok'],
        still_banned: ['仍被封禁，继续等待（勿反复 revive，可能加重风控）', 'warn'],
        blocked: ['会话仍有效，但被上游风控拦截（可能仍是封禁）—— 继续观察', 'warn'],
        session_dead: ['不是封禁问题 —— 登录态失效，需重新登录', 'warn'],
        inconclusive: ['探测无明确结论（网络/限流等），稍后再试', 'warn'],
        unknown: ['无法探测：上游客户端未配置', 'err'],
      };
      const m = map[r.verdict] || [r.verdict + (r.detail ? '：' + r.detail : ''), 'warn'];
      toast(m[0], m[1]);
      return; // 探测不改变状态，跳过下面的 loadOverview
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally {
    b.disabled = false;
    if (a !== 'tasks') invalidatePackages();
    loadOverview(true);
  }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。

   出图模型单独一套口径（2026-10-22 复测，见 /panel/api/models 的 credit_source）：
   目录对出图模型已不报 credits（CN 混元条目只剩 id/name/tags），这一列基本只剩
   「实测」一种来源：
   - CN hunyuan-image-alpha：实扣每张固定 5.71，回执 output_image_coefficient=1，
     与 size/quality/n 全无关（1024/768/512 + n=2 恒等）；
   - global hy-image-3.5：实扣每张固定 0.27，回执恒 15000 output_image_tokens，
     但 low/medium/high 与 512/1024/1536 六档实测同价——token 数不代表单价，
     按 token 反推价格是错的，只能如实显示回执 credit。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  if (m.image_generation) return imageRateCell(m);
  if (!m.credits && m.measured_credit) {
    return '<span title="上游未报倍率；此值来自本网关的实际扣费记录（credits/张）" style="cursor:help;color:var(--ink-2)">实测 ' +
      esc(m.measured_credit) + '/张</span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

/* imageRateCell 出图模型倍率格：牌价与实扣并列，档位取自 charge_variants。
   数据来源：credits = 上游目录牌价；measured_credit = 账本均值（落盘）；
   charge_last / charge_variants = 上游回执 usage.credit（本次运行，按尺寸×画质分档）。 */
function imageRateCell(m) {
  const last = m.charge_last;
  const per = last && last.credit != null ? last.credit : null;
  const avg = m.measured_credit;
  const tier = last ? [last.size, last.quality].filter(Boolean).join(' ') : '';
  const used = per != null ? per : (avg != null ? Number(avg) : null);
  if (used == null) {
    // 还没出过图：有牌价就显示牌价并说明「实扣未知」，没牌价就明说「未观测」。
    if (m.credits) {
      return '<span title="上游目录牌价；本网关还没有出图记录，实扣未知" style="cursor:help;color:var(--ink-2)">' +
        esc(m.credits) + ' <span class="note">牌价</span></span>';
    }
    return '<span title="上游目录不报倍率；本网关还没有出图记录，实扣未知" style="cursor:help;color:var(--ink-3)">未观测</span>';
  }
  let tip;
  if (m.credits) {
    // 有牌价（CN 混元）：说明牌价与实扣的关系，别让运维以为牌价就是账单。
    tip = '上游目录牌价 ' + m.credits + '；实扣取上游出图回执 usage.credit' +
      (tier ? '（' + tier + '）' : '') + '。两者不一致时以实扣为准。';
  } else {
    // 无牌价（global）：按 token 计费，单张价随画质变——把 token 也报出来便于复核。
    tip = '上游目录不报倍率；此值为上游出图回执 usage.credit' +
      (tier ? '（' + tier + '）' : '') +
      (last && last.output_image_tokens ? '，输出图像 ' + last.output_image_tokens + ' token' : '') +
      (last && last.output_image_coefficient ? '，回执倍率 ' + last.output_image_coefficient + 'x' : '') +
      '。实测该模型按张固定计价，画质/尺寸不影响价格。';
  }
  const shown = Number(used).toFixed(2);
  const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
  const tierNote = tier ? '<div class="note">' + esc(tier) + '</div>' : '';
  return '<span title="' + esc(tip) + '" style="cursor:help;color:var(--ink-1)">实扣 ' +
    esc(shown) + '/张' + base + tierNote + '</span>';
}

async function loadModels(force) {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测、模型锁与最近请求都是本地可选增强：任一失败不影响模型目录本身。
    // 可用性标签不额外发推理请求，只用模型锁和最近真实请求结果合成。
    // force=true（「重新获取」按钮）：带 refresh=1 让后端清掉 global 目录 1h 缓存，
    // 真正重新问一次上游，否则点多少次都是同一份缓存（issue #216 现象 3）。
    const [d, pr, st, rm] = await Promise.all([
      api('models' + (force ? '?refresh=1' : '')),
      api('model_probes').catch(() => ({})),
      api('status').catch(() => ({})),
      api('request_metrics').catch(() => ({})),
    ]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    mdLocks = Array.isArray(st.model_locks) ? st.model_locks : [];
    mdRecent = Array.isArray(rm.recent) ? rm.recent : [];
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    // 目录来源：桌面端族是官方 picker 口径；union/enterprise 说明 v3 探测部分或全部失败，
    // 拿到的是回落目录（数量偏少、倍率可能是旧值），必须在界面上说清楚。
    const srcNote = { 'v3-desktop': '', 'v3-union': ' · ⚠ 国际版目录来自回落（IDE+CLI 并集）', 'enterprise': ' · ⚠ 国际版目录已降级到企业端点（窄目录/旧倍率）' }[d.global_source] || '';
    $('mdNote').textContent = mdAll.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '') + srcNote;
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdAvailabilityOf 合成 Global 模型当前可用性：模型锁优先于最近请求结果。
// 只返回有真实本地证据的状态；目录中存在但从未请求过的模型标为“待验证”，
// 避免把“上游列出了模型”误当成“当前账号有权限”。
function mdAvailabilityOf(m, locks, recent) {
  const id = String((m && m.id) || '');
  if (!id.startsWith('global:')) return null;
  locks = Array.isArray(locks) ? locks : mdLocks;
  recent = Array.isArray(recent) ? recent : mdRecent;
  const bare = id.slice('global:'.length);
  const lock = locks.find(r => r && r.realm === 'global' && r.model === bare);
  if (lock) {
    const reason = String(lock.reason || '').trim();
    const lower = reason.toLowerCase();
    if (lock.state === 'partial') {
      return { key: 'partial', label: '部分可用', cls: 'warn',
        tip: '仍有 ' + Number(lock.servable || 0) + ' 个 global 账号可服务；已锁 ' + Number(lock.locked || 0) + ' 个' + (reason ? '；' + reason : '') };
    }
    if (lock.state === 'starved') {
      return { key: 'starved', label: '账号暂不可用', cls: 'mute',
        tip: '没有账号能服务该模型，但并非该模型自身被锁；请检查账号冷却/在途占用' };
    }
    if (lower.startsWith('11158') || lower.includes('not allowed for user type')) {
      return { key: 'denied', label: '无权限', cls: 'bad',
        tip: '当前 global 账号类型不支持该模型' + (reason ? '；' + reason : '') };
    }
    if (lower.startsWith('11102') || lower.includes('service info not found')) {
      return { key: 'missing', label: '上游无此模型', cls: 'bad',
        tip: '上游目录残留，实际请求返回模型不存在' + (reason ? '；' + reason : '') };
    }
    return { key: 'limited', label: '限流/不可用', cls: 'warn',
      tip: '模型级冷却中' + (reason ? '；' + reason : '') };
  }
  const ev = recent.find(e => e && e.model === id);
  if (ev) {
    const status = Number(ev.status || 0);
    const when = ev.time ? new Date(ev.time).toLocaleString('zh-CN', { hour12: false }) : '';
    if (ev.outcome === 'success' || (status >= 200 && status < 300)) {
      return { key: 'ok', label: '可用', cls: 'ok',
        tip: '最近一次真实请求成功' + (when ? ' · ' + when : '') };
    }
    if (status === 429) {
      return { key: 'rate', label: '限流', cls: 'warn',
        tip: '最近一次请求被限流' + (when ? ' · ' + when : '') };
    }
    if (status >= 500) {
      return { key: 'upstream', label: '上游错误', cls: 'bad',
        tip: '最近一次请求上游错误 HTTP ' + status + (when ? ' · ' + when : '') };
    }
    return { key: 'failed', label: '最近失败', cls: 'bad',
      tip: '最近一次请求失败 HTTP ' + (status || '—') + (when ? ' · ' + when : '') };
  }
  return { key: 'unknown', label: '待验证', cls: 'mute',
    tip: '尚无模型锁或近期真实请求记录；目录存在不代表当前账号有权限' };
}

function mdAvailabilityTag(m) {
  const av = mdAvailabilityOf(m);
  if (!av) return '';
  return ' <span class="tag ' + av.cls + ' md-avail" title="' + esc(av.tip) + '">' + esc(av.label) + '</span>';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
// 上游模型繁忙度（/v2/chat/busy）文案：只标需要避让的三档，idle/normal 不打扰。
const BUSY_WORDS = { busy: '繁忙', congested: '拥堵', unavailable: '不可用' };

function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  // 出图模型没有思考档位（上游目录里它本就不带 reasoning 字段）：显示 —，不写"不支持思考"。
  const effs = m.image_generation ? '<span style="color:var(--ink-3)">—</span>'
    : eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.tier) caps.push('<span class="tag warn" title="上游付费层级（' + esc(m.tier) + '）：切换模型前留意，免费账号可能调不通或排队靠后">' + esc(m.tier) + '</span>');
  if (m.image_generation) caps.push('<span class="tag ok">出图</span>');
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  if (BUSY_WORDS[m.busy]) {
    const bad = m.busy === 'congested' || m.busy === 'unavailable';
    caps.push('<span class="tag ' + (bad ? 'bad' : 'warn') + '" title="上游繁忙度 ' + esc(m.busy) + '（/v2/chat/busy 实时；服务端偶发无数据，同一批重查可能变）">' + BUSY_WORDS[m.busy] + '</span>');
  }
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm"><span class="mid">' + esc(m.id) + '</span>' + mdAvailabilityTag(m) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort && !m.image_generation ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = () => loadModels(true);

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
// logLevel 日志行的颜色级别。先抹掉「空赋值」字段再判：
// `claim_error=""`（值为空 = 这一项没有错误）里带 error 字样，裸匹配会把成功的任务行
// 整行标红；非空赋值（`claim_error="task not completed"`）仍然照常标红。
function logLevel(text) {
  const t = String(text == null ? '' : text).replace(/\b[a-z_]+=(?:""|'')/g, '');
  if (/error|失败|错误/.test(t)) return ' e';
  if (/warn|冷却|熔断/.test(t)) return ' w';
  return '';
}

async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows, credit, errSum] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      api('credit_history?limit=' + creditLimitValue()).catch(() => null),
      api('error_summary').catch(() => null),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = logLevel(e.text);
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
    renderCreditHistory(credit);
    renderErrorSummary(errSum);
  } catch (e) { /* 概览已提示 */ }
}

/* 错误汇总（后端 /panel/api/error_summary 已聚合+排序）：只渲染，不在这里再算。 */
function renderErrorSummary(d) {
  const rows = (d && d.rows) || [];
  $('errBody').innerHTML = rows.length ? rows.map(r =>
    '<tr class="' + (r.sev === 'bad' ? 'lv-e' : r.sev === 'warn' ? 'lv-w' : '') + '">' +
    '<td>' + esc(r.account || '—') + '</td>' +
    '<td>' + esc(r.kind) + '</td>' +
    '<td>' + esc(r.code || '—') + '</td>' +
    '<td>' + esc(r.model || '—') + '</td>' +
    '<td class="num">' + r.count + '</td>' +
    '<td>' + esc(r.last || '—') + '</td>' +
    '<td>' + esc(r.until || '—') + '</td>' +
    '<td class="sample" title="' + esc(r.sample_full || r.sample || '') + '">' + esc(r.sample || '') + '</td></tr>'
  ).join('') : '<tr><td colspan="8" style="color:var(--ink-3)">窗口内没有 WARN/ERR</td></tr>';
  $('errNote').textContent = d
    ? '扫描 ' + (d.scanned || 0) + ' 行 · ' + rows.length + ' 类 · 截至 ' + (d.updated_at || '—')
    : '—';
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqStats').innerHTML =
    usStat(fmtTok(m.completed), '已完成') +
    usStat(m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%', '完成成功率') +
    usStat(m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%', 'HTTP 成功率') +
    usStat(fmtMs(m.avg_duration_ms), '平均耗时') +
    usStat(String(m.in_flight || 0), '进行中') +
    usStat(fmtTok(a.files), '归档文件');
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* ── 积分历史（每次真实查到余额与上次比对，变动即留痕）─────────────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212，
   与请求记录一致），余额用千分位。 */
function creditNum(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + creditNum(d);
  if (d < 0) return '−' + creditNum(-d);
  return '0';
}
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 的 time.Time 序列化，恒为 RFC3339；这里挡的是手工改文件/
  // 将来换格式等脏数据——toLocaleTimeString 遇到非法日期不抛异常，只会渲染出
  // "Invalid Date" 这种没意义的字样。
  return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString('zh-CN', { hour12: false });
}
function creditAccountText(e) {
  e = e || {};
  return e.account ? String(e.account) : (e.uid || '—');
}
function creditBalanceText(e) {
  return creditNum(e && e.after);
}
function creditDescText(e) {
  return '余额 ' + creditNum(e && e.before) + ' → ' + creditNum(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + creditAccountText(e) + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + creditBalanceText(e) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分变动记录';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + creditNum(Math.abs(net));
}
function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!body) return;
  if (!d) {
    if (note) note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5" style="color:var(--ink-3)">积分历史不可用</td></tr>';
    return;
  }
  const entries = d.entries || [];
  if (note) note.textContent = creditHistoryNote(entries);
  body.innerHTML = entries.length
    ? entries.map(e => '<tr>' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(creditAccountText(e)) + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e && e.delta)) + '</td>' +
      '<td class="num">' + esc(creditBalanceText(e)) + '</td>' +
      '<td>' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5" style="color:var(--ink-3)">暂无积分变动记录</td></tr>';
}
function creditLimitValue() {
  const el = $('creditLimit');
  const n = el ? Number(el.value) : 100;
  return n === 300 || n === 1000 ? n : 100;
}
async function loadCreditHistory() {
  let d = null;
  try { d = await api('credit_history?limit=' + creditLimitValue()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');
if ($('creditLimit')) $('creditLimit').onchange = loadCreditHistory;
if ($('btnCreditReload')) $('btnCreditReload').onclick = loadCreditHistory;

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92 同步）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  update_auto_enabled: ['update', 'auto_enabled'], update_auto_time: ['update', 'auto_time'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  growth_concurrency: ['schedule', 'growth_concurrency'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  blackcat_hours: ['schedule', 'blackcat_hours'], blackcat_enabled: ['schedule', 'blackcat_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  keepalive_refresh_window: ['schedule', 'keepalive_refresh_window'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  account_spread_minutes: ['schedule', 'account_spread_minutes'], account_spread_min_gap_seconds: ['schedule', 'account_spread_min_gap_seconds'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'], credit_floor_max_rate: ['pool', 'credit_floor_max_rate'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  reserve_credits: ['pool', 'reserve_credits'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  sanitize_brand_scope: ['features', 'sanitize_brand_scope'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
  models_hidden: ['models', 'hidden'],
  models_aliases: ['models', 'aliases'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（吸收上游 10e17ef：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    const managed = d.environment_managed_fields || [];
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      el.disabled = managed.includes(path.join('.'));
      el.title = el.disabled ? '此项由启动环境变量设置，请在部署配置中修改。' : '';
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgKey').disabled = !!d.api_key_env_managed;
    $('cfgKey').title = d.api_key_env_managed ? '访问密钥由启动环境变量设置，请在部署配置中修改。' : '';
    $('cfgNote').textContent = managed.length ? '由启动环境管理：' + managed.join('、') : '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 逗号/空白分隔的列表字段：空 = 显式清空黑名单（空数组照发，否则删不掉条目）。
      if (name === 'models_hidden') v = raw.split(/[,，\s]+/).filter(Boolean);
      // 模型映射（issue #30）："客户端名=真实名" 条目数组，同列表语义；空数组照发 = 清空。
      else if (name === 'models_aliases') v = raw.split(/[,，]+/).map(s => s.trim()).filter(Boolean);
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl',
  'keepalive_refresh_window'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const fields = r.restart_required || [];
    const restartNote = fields.length ? '需重启生效：' + fields.join('、') : '';
    toast(fields.length ? '配置已保存，其中 ' + fields.length + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    // 由启动环境变量托管时不写入本地（避免把无效值当会话密钥）。
    const k = $('cfgKey').value.trim();
    if (k && !r.api_key_env_managed) localStorage.setItem(LS_KEY, k);
    await loadConfig();
    $('cfgNote').textContent = [restartNote, (r.environment_managed_fields || []).length ? '由启动环境管理：' + r.environment_managed_fields.join('、') : ''].filter(Boolean).join('；');
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('apiKeyDone').hidden = true; $('apiKeyErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  $('btnAddApiKey').hidden = true;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
  $('addTabApiKey').hidden = tab !== 'apikey';
  // 页脚按钮随标签切：登录页「获取授权链接」、API Key 页「添加 API Key」，导入页无页脚按钮。
  $('btnAddApiKey').hidden = tab !== 'apikey';
  $('btnStartLogin').hidden = tab !== 'login' || loginState != null;
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  // 站点（官方域表 cn/intl/workbuddy）：面板只传 site，realm 由后端按站点表决定，
  // 避免前端自己维护一份域表。
  const site = (document.querySelector('input[name="addSite"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ site }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    pollFails = 0;
  loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
// pollFails 连续查询失败计数：网络抖动不该让用户以为「重新登录没用」。OAuth 可能已经在
// 浏览器侧完成，只是这次没收到结果——停掉轮询账号就永远不入池。
let pollFails = 0;
const POLL_MAX_FAILS = 20;
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    pollFails = 0;
    if (r.fatal) {
      // 上游许可类终态（席位满 / 许可证过期 / 试用到期）：重试无意义，直接把上游
      // 原话给用户（官方 SignLicenseError 行为）。
      stopPoll();
      $('addReady').hidden = true;
      $('addErr').hidden = false;
      $('addErr').textContent = r.message || '登录被上游拒绝';
      return;
    }
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      const warn = r.balance_error ? '；但余额校验失败：' + r.balance_error + '（若持续 401，请在浏览器退出腾讯账号后重登）' : '';
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中' + warn;
      invalidatePackages(); // 凭据换新：缓存里的旧 401 行必须作废
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    if (e.status === 404 || e.status === 410) {
      stopPoll();
      $('addReady').hidden = true;
      $('addErr').hidden = false;
      $('addErr').textContent = '登录会话已过期，请关闭后重新添加';
      return;
    }
    pollFails++;
    if (pollFails >= POLL_MAX_FAILS) {
      stopPoll();
      $('addReady').hidden = true;
      $('addErr').hidden = false;
      $('addErr').textContent = '连续 ' + POLL_MAX_FAILS + ' 次查询失败，已停止轮询：' + e.message + '（授权可能已完成，可关闭后重新添加）';
      return;
    }
    $('addErr').hidden = false;
    $('addErr').textContent = '网络中断，自动重试中（' + pollFails + '/' + POLL_MAX_FAILS + '）：' + e.message;
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;

$('btnAddApiKey').onclick = async () => {
  const token = ($('apiKeyInput').value || '').trim();
  $('apiKeyDone').hidden = true; $('apiKeyErr').hidden = true;
  if (!token) { $('apiKeyErr').hidden = false; $('apiKeyErr').textContent = '请填写 API Key'; return; }
  const btn = $('btnAddApiKey');
  btn.disabled = true;
  try {
    const r = await api('accounts/add_apikey', {
      method: 'POST',
      body: JSON.stringify({
        access_token: token,
        nickname: ($('apiKeyNickname').value || '').trim(),
        uid: ($('apiKeyUid').value || '').trim(),
      }),
    });
    $('apiKeyDone').hidden = false;
    $('apiKeyDone').textContent = '已添加 ' + (r.nickname || r.uid) + '（API Key 账号），账号已载入池中';
    $('apiKeyInput').value = '';
    setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
  } catch (e) {
    $('apiKeyErr').hidden = false;
    $('apiKeyErr').textContent = '添加失败：' + e.message;
  } finally { btn.disabled = false; }
};
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    invalidatePackages();
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.title = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.title = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') {
    loadOverview(true);
  }
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') {
    reattachQueueView();
    // 补偿状态只在进本视图/手动点击时取数（无 overview 快照，5s 拉一次就是打上游）；
    // 这里只对缓存做本地重渲染，让截止倒计时继续走动。
    renderCompensation();
  }
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null, taskCapabilities = { accept: false, claim: false, automate: false };
let taskJobTimer = null, taskJobPolling = null, taskJobID = null, taskJobGeneration = 0;
let taskJobsLoading = false;

// 服务端状态与界面反馈共用的纯函数，避免把已提交/失败当成已完成。
function checkinResultNote(r) {
  if (r.skipped) return { message: r.skip_reason || '该账号不适用签到', severity: '' };
  const balance = r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '';
  if (!r.checkin_done) return { message: '签到未完成：' + (r.checkin_error || r.checkin_message || '请稍后重试') + balance, severity: 'err' };
  if (r.balance_error) return { message: '签到已完成；余额刷新失败：' + r.balance_error, severity: 'err' };
  return { message: '签到已完成' + balance, severity: 'ok' };
}

function taskOutcomeStatus(item) {
  if (item.claim_error) return 'claim_pending';
  return item.status || (item.claimed ? 'done' : item.skipped ? 'skipped' : 'awaiting_progress');
}

function taskResultSummary(results) {
  const counts = { done: 0, skipped: 0, error: 0, awaiting_progress: 0, claim_pending: 0, accepted: 0 };
  for (const item of results || []) {
    const state = taskOutcomeStatus(item);
    if (Object.prototype.hasOwnProperty.call(counts, state)) counts[state]++;
    else counts.error++;
  }
  const parts = ['已完成 ' + counts.done + ' 项'];
  if (counts.accepted) parts.push('报名已提交 ' + counts.accepted + ' 批');
  if (counts.awaiting_progress) parts.push('等待计分 ' + counts.awaiting_progress + ' 项');
  if (counts.claim_pending) parts.push('领奖待重试 ' + counts.claim_pending + ' 项');
  if (counts.skipped) parts.push('跳过 ' + counts.skipped + ' 项');
  if (counts.error) parts.push('失败 ' + counts.error + ' 项');
  return { message: parts.join('，'), severity: counts.error || counts.claim_pending ? 'err' : counts.awaiting_progress || counts.skipped || !counts.done ? '' : 'ok' };
}

function taskRowPresentation(t, caps) {
  const code = t.task_code || t.code || '';
  const cur = t.current ?? 0, tgt = t.target ?? 0;
  const progress = t.progress_known === false ? '未提供' : tgt ? cur + ' / ' + tgt : cur > 0 ? String(cur) : '—';
  const rewards = [];
  if (t.credit) rewards.push('+' + t.credit + ' 分');
  if (t.energy) rewards.push('+' + t.energy + ' 能');
  if (t.reward_buddy) rewards.push('Buddy');
  const reward = t.reward_known === false ? '未提供' : rewards.join(' ') || '—';
  const state = t.claimed ? '已领取' : t.claimable ? '可领取' : t.locked ? '未解锁'
    : t.accept_status === 'accepted' ? '进行中' : t.status === 'completed' || t.status === 'complete' ? '已完成'
    : t.status === 'available' ? '可在客户端完成' : '未接受';
  // first_buddy 特例（与后端一致）：进度满≠可领，按钮仍走"一键完成"动作链。
  const action = !code || t.claimed || t.locked ? '' : t.claimable && caps.claim && code !== 'first_buddy' ? 'claim'
    : caps.automate && AUTO_TASKS[code] ? 'auto' : t.accept_status !== 'accepted' && caps.accept ? 'accept' : '';
  return { code, progress, reward, state, action };
}

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'WorkBuddy_Valorant': '真实「无畏契约拼豆专家」召唤+使用链（真实对话 requestId，截止 11-15）',
  'expert_use_maidehao': '「中国电商运营专家」召唤+使用链（专家页搜索口径，截止 11-20，+50 分）',
  'expert_use_douyinyin': '「抖音策略师」召唤+使用链（专家页搜索口径，截止 11-20，+50 分）',
  'expert_use_shangjia_xuanchuan': '「商家宣传页与海报设计师」召唤+使用链（专家页搜索口径，截止 11-20，+50 分）',
  'expert_use_bowangwang': '「直播电商教练」召唤+使用链（专家页搜索口径，截止 11-20，+50 分）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskJobGeneration++;
  if (taskJobTimer) clearInterval(taskJobTimer);
  taskJobTimer = null;
  taskJobID = null;
  taskUID = uid;
  taskCapabilities = { accept: false, claim: false, automate: false };
  $('btnTaskAcceptAll').hidden = true;
  $('btnTaskAutoAll').hidden = true;
  $('taskNote').hidden = true;
  $('taskJobView').hidden = true;
  $('btnTaskAutoAll').disabled = false;
  $('btnTaskAutoAll').textContent = '后台完成可自动任务';
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
  loadAccountTaskJob(uid);
}
function closeTasks() {
  taskJobGeneration++;
  $('taskVeil').classList.remove('on'); taskUID = null; taskJobID = null;
  if (taskJobTimer) clearInterval(taskJobTimer);
  taskJobTimer = null;
}
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = () => { loadTasks(); if (taskUID) loadAccountTaskJob(taskUID); };

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 后台执行：请求仅启动任务，页面轮询进度；关闭弹窗不影响服务端执行。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID || !taskCapabilities.automate) return;
  const uid = taskUID;
  const btn = $('btnTaskAutoAll');
  btn.disabled = true; btn.textContent = '启动中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(uid) + '/tasks/auto_all', { method: 'POST' });
    toast(r.started ? '后台任务已启动，关闭页面后仍会继续执行' : '该账号已有后台任务，已恢复进度显示', 'ok');
    if (taskUID === uid) { taskJobGeneration++; renderAccountTaskJob(r.job); startAccountTaskJobPolling(uid); }
    if (view === 'taskscenter') loadTaskJobs();
  } catch (e) {
    toast(e.message, 'err');
    if (taskUID === uid) { btn.disabled = false; btn.textContent = '后台完成可自动任务'; }
  }
};

function taskJobPresentation(job) {
  const active = ['pending', 'running'].includes(job.status);
  const labels = { pending: '排队中', running: '执行中', finished: '已结束', interrupted: '已中断', error: '执行失败' };
  const trigger = { new_account: '新增账号', daily: '每日定时', manual: '手动启动', resume: '重启续跑' }[job.trigger] || job.trigger || '';
  const progress = '处理 ' + (job.completed || 0) + ' / ' + (job.total || 0) + ' 项';
  const summary = taskResultSummary(job.results || []);
  const current = active && job.current_task ? '正在处理：' + job.current_task : '';
  return { active, trigger, progress, label: labels[job.status] || job.status,
    message: [labels[job.status] || job.status, progress, current, job.error || job.message, summary.message].filter(Boolean).join(' · '),
    severity: job.status === 'error' || job.status === 'interrupted' ? 'err' : summary.severity };
}

function taskJobRows(job) {
  return (job.results || []).map(item => qrowHTML({ code: item.task_code || '', title: item.title, desc: item.desc,
    status: taskOutcomeStatus(item), message: item.message || item.claim_error,
    prog: item.progress_after || item.progress_before || '' })).join('');
}

function renderAccountTaskJob(job) {
  $('taskJobView').hidden = !job;
  if (!job) return;
  taskJobID = job.id;
  const info = taskJobPresentation(job);
  $('taskJobState').className = 'state ' + info.severity;
  $('taskJobState').textContent = info.message;
  $('taskJobResults').innerHTML = taskJobRows(job) || '<div class="hint">等待后台任务开始处理</div>';
  $('btnTaskAutoAll').disabled = info.active;
  $('btnTaskAutoAll').textContent = info.active ? '后台执行中…' : '后台完成可自动任务';
}

async function loadAccountTaskJob(uid) {
  if (taskJobPolling === uid) return;
  taskJobPolling = uid;
  const generation = taskJobGeneration;
  try {
    const d = await api('accounts/' + encodeURIComponent(uid) + '/tasks/job');
    if (taskUID !== uid || taskJobGeneration !== generation) return;
    const previous = taskJobID;
    renderAccountTaskJob(d.job);
    if (d.job && taskJobPresentation(d.job).active) startAccountTaskJobPolling(uid);
    else {
      if (taskJobTimer) clearInterval(taskJobTimer);
      taskJobTimer = null;
      if (previous && d.job && previous === d.job.id) loadTasks();
    }
  } catch (e) {
    if (taskUID === uid && taskJobGeneration === generation) { $('taskJobView').hidden = false; $('taskJobState').className = 'state err'; $('taskJobState').textContent = '后台进度查询失败：' + e.message; }
  } finally {
    if (taskJobPolling === uid) taskJobPolling = null;
    if (taskUID && (taskUID !== uid || taskJobGeneration !== generation) && !taskJobTimer) loadAccountTaskJob(taskUID);
  }
}

function startAccountTaskJobPolling(uid) {
  if (taskJobTimer) clearInterval(taskJobTimer);
  taskJobTimer = setInterval(() => { if (taskUID === uid) loadAccountTaskJob(uid); }, 3000);
}

/* 任务中心：一张卡、一个列表，来源两处——常驻的「后台自动任务」(tasks/jobs) 与
   一次性的「待办扫描/队列」(tasks/scan_all、tasks/queue)。两者都是"某账号的任务
   进展"，行形态也一致（同走 qrowHTML），因此不各渲染一块，而是共用 renderQueue。
   taskCenterOwner 决定列表当前显示谁：queue=扫描结果或本页启动的队列（实时优先），
   jobs=后台自动任务（默认；点「刷新进度」也回到它）。 */
let taskCenterOwner = 'jobs';
async function loadTaskJobs() {
  // go() 顶层调用也会进入此函数，首次 await 前不读取后声明的状态。
  await Promise.resolve();
  if (taskJobsLoading) return;
  taskJobsLoading = true;
  try {
    const d = await api('tasks/jobs');
    const jobs = d.jobs || [];
    $('taskJobsNote').hidden = !!jobs.length && d.auto_enabled !== false;
    $('taskJobsNote').textContent = d.auto_enabled === false ? '自动成长任务已关闭，可在账号任务中手动启动后台执行。' : '新增国区账号后自动执行，每日按成长任务排程继续推进；可在账号任务中手动启动。';
    if (taskCenterOwner !== 'jobs') return; // 队列/扫描结果正占着列表
    const groups = groupsFromJobs(jobs);
    renderQueue(groups, null, jobs.length ? '后台任务还没有结果' : '还没有后台任务记录',
      jobs.length ? '任务刚开始执行，结果出来后会显示在这里。' : '新增国区账号后自动执行；也可以在上面扫描待办并执行。');
  } catch (e) {
    if (taskCenterOwner === 'jobs') renderQueue([], null, '后台任务查询失败', e.message);
  } finally { taskJobsLoading = false; }
}
// 后台任务 → 与扫描/队列同形的分组（共用 qrowHTML 渲染）
function groupsFromJobs(jobs) {
  return jobs.map(job => {
    const info = taskJobPresentation(job);
    return {
      uid: job.uid, nick: job.nickname,
      cnt: info.trigger + ' · ' + info.label + (info.progress ? ' · ' + info.progress : ''),
      tip: info.message, // 明细挂 title（原「后台自动任务」卡的状态行）
      rows: (job.results || []).map(item => ({
        kind: 'job', code: item.task_code || '', title: item.title, desc: item.desc,
        status: taskOutcomeStatus(item), message: item.message || item.claim_error,
        prog: item.progress_after || item.progress_before || '',
      })),
    };
  }).filter(g => g.rows.length);
}
$('btnTaskJobsReload').onclick = () => { taskCenterOwner = 'jobs'; loadTaskJobs(); };

async function loadTasks() {
  if (!taskUID) return;
  const uid = taskUID;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(uid) + '/tasks');
    if (taskUID !== uid) return;
    taskCapabilities = d.capabilities || { accept: d.realm !== 'global', claim: d.realm !== 'global', automate: d.realm !== 'global' };
    $('btnTaskAcceptAll').hidden = !taskCapabilities.accept;
    $('btnTaskAutoAll').hidden = !taskCapabilities.automate;
    $('taskNote').textContent = d.message || (d.realm === 'global' ? '国际区任务当前仅支持查询，请在官方客户端完成。' : '');
    $('taskNote').hidden = !$('taskNote').textContent;
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度/奖励/状态/动作统一走 taskRowPresentation（与后端任务结果口径一致）
      const row = taskRowPresentation(t, taskCapabilities);
      const badge = '<span class="tag ' + (t.claimed ? 'ok' : t.claimable ? 'warn' : 'mute') + '">' + esc(row.state) + '</span>';
      const acted = row.action ? '<button class="xs' + (row.action === 'accept' ? '' : ' primary') + '" data-t="' + row.action + '" data-c="' + esc(row.code) + '"' + (row.action === 'auto' && AUTO_TASKS[row.code] ? ' title="' + esc(AUTO_TASKS[row.code]) + '"' : '') + '>' + ({ claim: '领取', auto: '一键完成', accept: '接受' }[row.action]) + '</button>' : '';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || row.code) + '</div><div class="id">' + esc(row.code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(row.progress) + '</td>' +
        '<td class="num">' + esc(row.reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    if (taskUID !== uid) return;
    taskCapabilities = { accept: false, claim: false, automate: false };
    $('btnTaskAcceptAll').hidden = true;
    $('btnTaskAutoAll').hidden = true;
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  if (!code || !(kind === 'auto' ? taskCapabilities.automate : taskCapabilities[kind])) return;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      const status = taskOutcomeStatus(r);
      const severity = status === 'done' ? 'ok' : status === 'error' || status === 'claim_pending' ? 'err' : '';
      toast(r.message || taskResultSummary([r]).message, severity);
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  taskCenterOwner = 'queue';
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '未发现可自动执行的待办', '当前扫描范围没有可自动执行的任务；跳过和扫描失败的账号会单独列出。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  taskCenterOwner = 'queue';
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) {
      if (r.scan_accounts) renderQueue(groupItems({ accounts: r.scan_accounts }), null, '未启动任务队列', r.message || '没有可自动执行的待办');
      toast(r.message || '没有可自动执行的待办', r.scan_error_count ? 'err' : '');
      return;
    }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (a.growth_error) rows.push({ kind: 'account', code: '', title: '扫描失败', status: 'error', message: a.growth_error });
    if (a.skipped) rows.push({ kind: 'account', code: '', title: '已跳过', status: 'skipped', message: a.skip_reason || '该账号不适用当前自动任务' });
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行', awaiting_progress: '等待计分', claim_pending: '领奖待重试', accepted: '已接受' };
function taskProgressLabel(value) {
  const progress = String(value ?? '').trim();
  return /^\d+(?:\s*\/\s*\d+)?$/.test(progress) ? progress : '—';
}
// 展示名：上游短中文名 > 扫描缓存 > 本仓中文说明 > 代号兜底。
// 后台任务结果行只带 task_code/desc（实测 24 个码全无 title），不接 desc 就会
// 一路退回代号，与左边那列代号同名。
function qrowHTML(it) {
  const title = it.title || GROWTH_TITLES[it.code] || it.desc || it.code;
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  const progress = taskProgressLabel(it.prog);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span></span>' +
    '<span class="prog" title="' + esc(progress) + '">' + esc(progress) + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
/* 任务中心筛选：默认收起「已完成」与「该账号没有此任务」两类噪音行。
   开关只影响渲染，不重新打上游；状态存本机，下次打开沿用。 */
const LS_TC_HIDE_DONE = 'wb2api.tc.hideDone', LS_TC_HIDE_NA = 'wb2api.tc.hideNa';
let tcHideDone = localStorage.getItem(LS_TC_HIDE_DONE) !== '0';
let tcHideNa = localStorage.getItem(LS_TC_HIDE_NA) !== '0';
let tcLastRender = null; // 最近一次 renderQueue 的原始入参，供切换开关时原地重绘
const TC_NA_MESSAGE = '该账号没有此任务';
function tcRowHidden(row) {
  if (tcHideDone && row.status === 'done') return true;
  if (tcHideNa && String(row.message || '').trim() === TC_NA_MESSAGE) return true;
  return false;
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  tcLastRender = { groups, progress, emptyTitle, emptyDesc };
  const rawRows = groups.reduce((n, g) => n + g.rows.length, 0);
  groups = groups
    .map(g => Object.assign({}, g, { rows: g.rows.filter(row => !tcRowHidden(row)) }))
    .filter(g => g.rows.length);
  const hiddenRows = rawRows - groups.reduce((n, g) => n + g.rows.length, 0);
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (rawRows) {
      empty.querySelector('.t').textContent = '当前筛选下没有任务';
      empty.querySelector('.d').textContent = '已隐藏 ' + rawRows + ' 行（已完成 / 该账号没有此任务），取消上方勾选即可查看。';
    } else {
      if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
      if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    }
    list.innerHTML = '';
    $('qProg').hidden = true; $('tcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    const tasks = g.rows.filter(row => row.kind !== 'account' && row.kind !== 'job').length;
    // cnt 给后台任务用（"每日定时 · 执行中"）；扫描/队列按任务项数自己算
    const cnt = g.cnt ? g.cnt : (tasks ? tasks + ' 项任务' : '账号状态');
    return '<div class="qgroup" title="' + esc(g.tip || '') + '"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + esc(cnt) + '</span><span class="grow"></span>' +
      '<button class="xs" data-task-uid="' + esc(g.uid) + '">查看任务</button></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  const taskCount = groups.reduce((sum, group) => sum + group.rows.filter(row => row.kind !== 'account').length, 0);
  $('tcSummary').textContent = taskCount + ' 项任务' + (total > taskCount ? ' · ' + (total - taskCount) + ' 条账号状态' : '') +
    (hiddenRows ? ' · 已隐藏 ' + hiddenRows : '');
  updateProgress(progress);
}
function syncTcFilter(kind) {
  if (kind === 'done') localStorage.setItem(LS_TC_HIDE_DONE, tcHideDone ? '1' : '0');
  if (kind === 'na') localStorage.setItem(LS_TC_HIDE_NA, tcHideNa ? '1' : '0');
  if (tcLastRender) renderQueue(tcLastRender.groups, tcLastRender.progress, tcLastRender.emptyTitle, tcLastRender.emptyDesc);
}
$('tcHideDone').checked = tcHideDone;
$('tcHideNa').checked = tcHideNa;
$('tcHideDone').onchange = ev => { tcHideDone = ev.target.checked; syncTcFilter('done'); };
$('tcHideNa').onchange = ev => { tcHideNa = ev.target.checked; syncTcFilter('na'); };
$('qcList').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-task-uid]');
  if (b) openTasks(b.dataset.taskUid);
});
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => ['done', 'error', 'skipped', 'awaiting_progress', 'claim_pending'].includes(it.status)).length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code, title: it.title || '',
      prog: '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    const summary = taskResultSummary(q.items || []);
    toast('任务队列已结束：' + summary.message, summary.severity);
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 扫描/队列结果占着列表时不抢视图（同 loadTaskJobs 里的归属闸门）。本函数也被
  // 5s 定时刷新调用，无条件改 owner + 渲染后台任务，会把用户刚查出来的扫描结果
  // 冲成空白（表现为「查完就自己消失」）。切回「后台任务」仍是显式动作：刷新进度
  // 按钮、或本页队列确实在跑时由下面的尾部接管。
  if (taskCenterOwner !== 'queue') {
    taskCenterOwner = 'jobs';
    loadTaskJobs();
  }
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) { taskCenterOwner = 'queue'; startQueuePolling(); }
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
function usRow(name, sub, a, mid, withPerf, nameHtml) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + (nameHtml || esc(name)) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtHit(a) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 11 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 8 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 8 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">缓存命中率</th><th class="num">Completion</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x => {
    const isAcct = usDim === 'account';
    const key = isAcct ? String(x.key || '').slice(0, 8) : x.key;
    return usRow(key,
      isAcct ? (x.extra || '') : '',
      x,
      isAcct ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf,
      isAcct ? remarkName(x.key, esc(key)) : '');
  }).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + remarkName(uid, esc(account)) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  renderUsageDim();
  renderCreditDim();
  renderUsageChart(usageData.series || []);
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}


function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               ch: Number(p.cache_hit_tokens || 0), cm: Number(p.cache_miss_tokens || 0),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  // 缓存命中率 = 命中/(命中+未命中)。没这两个字段的点（旧桶/上游没给）留 null，
  // 曲线在那里断开，不拿 0 顶替——0% 和「没观测到」是两件事。
  let hasHit = false;
  for (const p of pts) {
    p.rate = (p.ch + p.cm) > 0 ? p.ch / (p.ch + p.cm) : null;
    if (p.rate !== null) hasHit = true;
  }

  const W = 1200, H = 200, PL = 58, PR = hasHit ? 34 : 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  $('usChartNote').textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    // 每根柱子一个 <g>，<title> 作为它的**子元素**：
    //   - <title> 只有作为元素的子节点才是那个元素的悬停提示；挂在 <svg> 根下时浏览器
    //     把它当整图的说明，于是**所有柱子都显示最后一条数据**（issue #128 的
    //     「细节数据好像所有柱状图都相同」）。
    //   - 两个 rect（prompt/completion 堆叠）共用一个 <title>，悬停任一段都是同一份明细。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  // 命中率曲线：右轴 0-100%，断开处不连线（见上）。
  if (hasHit) {
    for (const pct of [0, 50, 100]) {
      const y = PT + ih - ih * (pct / 100);
      out += '<text class="tk" x="' + (W - PR + 4) + '" y="' + (y + 3.5).toFixed(1) + '">' + pct + '%</text>';
    }
    let seg = [];
    const flush = () => {
      if (seg.length > 1) out += '<polyline class="hitline" points="' + seg.join(' ') + '"/>';
      seg = [];
    };
    for (const p of pts) {
      if (p.rate === null) { flush(); continue; }
      const x = xOf(p.t).toFixed(1), y = (PT + ih - ih * p.rate).toFixed(1);
      out += '<circle class="hitdot" cx="' + x + '" cy="' + y + '" r="2"/>';
      seg.push(x + ',' + y);
    }
    flush();
  }

  out += '</svg>';
  host.innerHTML = out;
}

// fmtTokTip 图表 tooltip 的数值格式（与柱体标签同口径；前端测试以此为切片标记）。
function fmtTokTip(v) { return fmtTok(v); }

/* 缓存命中率 = 命中/(命中+未命中)，颜色分级（issue #92 同步）：≥90% 绿 /
   80–90% 黄 / <80% 红；样本不足灰（—）。title 带绝对量，供逐项核对。 */
function fmtHit(a) {
  const h = Number(a && a.cache_hit_tokens || 0), m = Number(a && a.cache_miss_tokens || 0);
  return cacheRateCell(h, m);
}

/* cacheRateCell 缓存命中率单元格（积分表等按 (hit, miss) 直传的调用点）。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

/* warmUsageModelRates 预热模型目录缓存（倍率来源）。旧桶缺倍率时后端只在
   这次请求里能拿到当前倍率做展示回填；失败不阻塞用量统计，10 分钟后再试。 */
let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + q.toString());
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="11" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 积分构成 ─────────────────────────────────────────────────────── */
// 套餐标签（后端 pool.ClassifyPlan 判定，unknown 不显示）：一眼看出该指望哪个号。
const PLAN_LABELS = { free: '免费版', trial: '体验版', activity: '活动号', standard: '标准版' };
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }


// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DAY_MS = 24 * 3600 * 1000;

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

// pkExpiryMs 包到期毫秒时间戳：到期分布（#59）与明细排序（#61）共用这一份。
function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

// pkDetailGroups 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%' +
      (PLAN_LABELS[a.plan] ? ' · ' + PLAN_LABELS[a.plan] : '') + '</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，包的**面额**列是重点
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* 到期提醒快照（零上游请求）：吃 /overview 里已有的 credits_expiry_buckets ——
   由签到与余额后台刷新按自然日聚合后写进 state.json，随首页统计一起到。
   此前快照只有「最早一批」，同一天多个包会被少算，扣穿第一批后整行消失；
   现在批次由后端聚合（date/expires_at/remain），与「积分构成」的逐包按日汇总同口径。

   旧 state（升级后尚未跑过一轮余额刷新，或 Redis 快照来自旧版本）没有 buckets，
   退回 credits_earliest_expiry / credits_earliest_remaining 只显示最早一批，保证卡片
   不空白；下一轮刷新后自动变成完整视图。

   剩余天数按自然日算（不再对精确时间戳 ceil——后者会把明天/今天多算一天，
   日均需耗随之偏小）。默认只展开最近到期的 3 个账号，其余收纳。 */
// expCSTDate 把时间戳格式化成 UTC+8 的自然日（YYYY-MM-DD）。上游到期字段与后端
// credits_expiry_buckets.date 都是 +08 口径，前端不能拿浏览器本地时区去截日期。
function expCSTDate(value) {
  const d = value instanceof Date ? value : new Date(value);
  if (isNaN(d.getTime())) return '';
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(d);
  const part = t => (parts.find(x => x.type === t) || {}).value || '';
  return part('year') + '-' + part('month') + '-' + part('day');
}

function expBucketList(a) {
  const raw = Array.isArray(a && a.credits_expiry_buckets) ? a.credits_expiry_buckets : [];
  const out = [];
  for (const b of raw) {
    const date = String((b && b.date) || '').slice(0, 10);
    const remain = Number((b && b.remain) || 0);
    if (date && remain > 0) out.push({ date, remain });
  }
  if (out.length) {
    out.sort((x, y) => (x.date < y.date ? -1 : x.date > y.date ? 1 : 0));
    return out;
  }
  // 旧 state 兜底：只有最早一批。
  const ms = a && a.credits_earliest_expiry ? new Date(a.credits_earliest_expiry).getTime() : NaN;
  const remain = Number((a && a.credits_earliest_remaining) || 0);
  if (Number.isFinite(ms) && remain > 0) {
    return [{ date: expCSTDate(ms), remain }];
  }
  return [];
}

// expDayDiff 按 UTC+8 自然日算剩余天数：今天=0、明天=1。不拿精确时间戳 ceil——
// 那会把明天/今天多算一天，日均需耗随之偏小（旧快照的口径问题）。
function expDayDiff(dateStr) {
  const ms = Date.parse(dateStr + 'T00:00:00+08:00');
  const base = Date.parse(expCSTDate(Date.now()) + 'T00:00:00+08:00');
  if (!Number.isFinite(ms) || !Number.isFinite(base)) return null;
  return Math.round((ms - base) / PK_DAY_MS);
}

function renderExpirySnapshot(d) {
  const rows = [];
  for (const a of (d && d.accounts) || []) {
    const bs = expBucketList(a).filter(b => {
      const days = expDayDiff(b.date);
      return days !== null && days >= 0;
    });
    if (!bs.length) continue;  // 无未来批次（长期包/已清空）不入表
    rows.push({ a, bs });
  }
  // 按最早到期日升序；同一天按昵称/uid 兜底，保证多次刷新顺序稳定。
  rows.sort((x, y) => {
    if (x.bs[0].date !== y.bs[0].date) return x.bs[0].date < y.bs[0].date ? -1 : 1;
    const ax = String(x.a.nickname || x.a.uid || '');
    const ay = String(y.a.nickname || y.a.uid || '');
    return ax < ay ? -1 : ax > ay ? 1 : 0;
  });
  const EXP_HEAD = 3;  // 默认只展开最近到期的 3 个账号，其余收纳成一行
  const rowOf = ({ a, bs }, hidden) => {
    const first = bs[0];
    const days = expDayDiff(first.date);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDayDiff(b.date) <= 7).reduce((s, b) => s + b.remain, 0);
    const color = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    const sub = (rest ? '<div class="note">' + rest + '</div>' : '') +
      (week > first.remain ? '<div class="note">7 天内合计 ' + fmtTok(week) + '</div>' : '');
    return '<tr' + (hidden ? ' data-exp-row="rest" hidden' : '') + '>' +
      '<td>' + esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</td>' +
      '<td>' + esc(first.date) + sub + '</td>' +
      '<td class="num" style="color:' + color + '">' +
        (days === 0 ? '今天' : days === 1 ? '明天' : days + ' 天') + '</td>' +
      '<td class="num">' + fmtTok(first.remain) + '</td>' +
      '<td class="num">' + fmtTok(daily) + '</td>' +
      '</tr>';
  };
  // 收纳行沿用「积分构成」页的分组行样式（pk-group-summary / pk-group-toggle）。
  const rest = rows.slice(EXP_HEAD);
  $('expBody').innerHTML = !rows.length ? '<tr><td colspan="5">暂无未来到期批次</td></tr>'
    : rows.slice(0, EXP_HEAD).map(r => rowOf(r, false)).join('') +
      (rest.length ? '<tr class="pk-group-summary"><td colspan="5">' +
        '<button type="button" class="pk-group-toggle" data-exp-rest="' + rest.length +
        '" aria-expanded="false">其余 ' + rest.length + ' 个账号，展开</button></td></tr>' +
        rest.map(r => rowOf(r, true)).join('') : '');
  // 重建 tbody 后恢复用户上次的展开选择（否则下一轮 5s 轮询会把展开收回）。
  applyRestToggle($('expBody'), restOpen.expBody);
  // 数据节奏透明化：界面 5 秒轮询，但卡片只随余额后台刷新更新（默认每 5 分钟）。
  const iv = Number(d && d.balance_refresh_sec) || 0;
  const at = d && d.balance_refreshed_at ? new Date(d.balance_refreshed_at).getTime() : 0;
  let cadence = '未开启后台刷新';
  if (iv > 0) {
    cadence = '每 ' + (iv % 60 === 0 ? iv / 60 + ' 分钟' : iv + ' 秒') + '后台刷新';
    if (Number.isFinite(at) && at > 0) {
      const min = Math.floor((Date.now() - at) / 60000);
      cadence += '，' + (min < 1 ? '刚刚更新' : min + ' 分钟前更新');
    }
  }
  $('expNote').textContent = rows.length
    ? rows.length + ' 个账号有未到期批次 · ' + cadence + ' · 明细见「积分构成」'
    : '暂无未来到期批次';
}

// 按 restOpen 把指定 tbody 的收纳行落到「展开/收起」状态。渲染重建 innerHTML 后必须调用一次，
// 否则用户上一次的展开选择会丢（5s 轮询每次都会重建）。
function applyRestToggle(body, expanded) {
  if (!body) return;
  const btn = body.querySelector('button[data-exp-rest]');
  if (!btn) return;
  body.querySelectorAll('tr[data-exp-row="rest"]').forEach(row => { row.hidden = !expanded; });
  btn.setAttribute('aria-expanded', String(!!expanded));
  btn.textContent = (expanded ? '收起其余 ' : '其余 ') + btn.dataset.expRest + ' 个账号' +
    (expanded ? '' : '，展开');
}

// 收纳行展开/收起（与「积分构成」页 pkDetail 同款：查 data 行 + aria-expanded + 改按钮文案）。
// 监听挂在 tbody 上（渲染替换 innerHTML 不会丢）；到期提醒与官方补偿共用本函数。
// 用户选择写进 restOpen，渲染函数重建 tbody 后调 applyRestToggle 恢复。
function bindRestToggle(id) {
  const body = $(id);
  if (!body) return;
  body.addEventListener('click', ev => {
    const btn = ev.target.closest('button[data-exp-rest]');
    if (!btn) return;
    const next = btn.getAttribute('aria-expanded') !== 'true';
    restOpen[id] = next;
    applyRestToggle(body, next);
  });
}
bindRestToggle('expBody');
bindRestToggle('compBody');

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证
   （强制 byte 模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);

  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}

// 按需拉取（移植上游 8391a05）：packages 是**逐账号实时查上游**，切进本视图就打一次，
// 账号一多既慢又压上游。有缓存先直接渲染（标注数据年龄），只有首次进入或显式刷新生效。
/* ══ 官方补偿（上游 PR #196）：先查状态再领、带截止倒计时 ═══════════════════
   取数与呈现分离：compData 只在进页面 / 点「检查」 / 点领取后刷新（补偿没有 overview
   快照，挂进 5s 的 refreshVisible 就是每 5 秒打上游）；倒计时走动靠本地重渲染。 */
let compData = null;
const COMP_SOON_MS = 48 * 3600 * 1000;  // ≤48h 标红：补偿过期就真没了

// 上游 end_time 是 "2006-01-02 15:04:05" 无时区字符串，按 +08:00 解释（同 pkExpiryMs）。
function compEndMs(text) {
  const t = String(text || '').trim();
  if (!t) return null;
  const ms = Date.parse(t.replace(' ', 'T') + '+08:00');
  return Number.isFinite(ms) ? ms : null;
}

function compPending(r) {
  // 布尔化：&& 链对缺字段的行会返回 undefined，进 Number() 变 NaN —— 排序比较器收到
  // NaN 时 V8 的结果未定义（实测「待领优先」会静默失效），故这里收成 true/false。
  return Boolean(!r.skip && !r.error && r.active && !r.claimed && (r.credit || 0) > 0);
}

function compRemainText(ms) {
  if (ms == null) return '—';
  const diff = ms - Date.now();
  if (diff <= 0) return '已截止';
  const hours = Math.floor(diff / 3600000);
  if (hours < 48) return hours + ' 小时';
  return Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function renderCompensation() {
  const body = $('compBody');
  if (!body) return;
  const rows = (compData && compData.accounts) || [];
  const now = Date.now();
  // 待领优先（有操作价值的先露），其余保持池顺序；20 个号一屏铺不下，只展开前 3 行。
  rows.sort((a, b) => Number(compPending(b)) - Number(compPending(a)));
  const COMP_HEAD = 3;
  const rowOf = (r, hidden) => {
      const who = esc(r.nickname || String(r.uid || '').slice(0, 8));
      let state = '<span class="tag ok">无活动</span>';
      if (r.skip) state = '<span class="tag">跳过：' + esc(r.skip_reason || '不适用') + '</span>';
      else if (r.error) state = '<span class="tag bad" title="' + esc(r.error) + '">查询/领取失败</span>';
      else if (r.claimed) state = '<span class="tag ok">已领</span>';
      else if (compPending(r)) state = '<span class="tag warn">待领</span>';
      const ms = compEndMs(r.end_time);
      const soon = ms != null && ms - now <= COMP_SOON_MS;
      const remain = r.end_time
        ? '<span style="color:' + (soon ? 'var(--bad)' : 'var(--ink-3)') + '">' + compRemainText(ms) + '</span>'
        : '—';
      const act = compPending(r)
        ? '<button class="xs" data-comp="' + esc(r.uid) + '">领取</button>' : '';
      return '<tr' + (hidden ? ' data-exp-row="rest" hidden' : '') + '><td>' + who + '</td><td>' + state + '</td>' +
        '<td class="num">' + (r.credit ? fmtTok(r.credit) : '—') + '</td>' +
        '<td class="num">' + remain + '</td>' +
        '<td class="c-acts">' + act + '</td></tr>';
  };
  const rest = rows.slice(COMP_HEAD);
  body.innerHTML = !rows.length ? '<tr><td colspan="5">未查询（点「检查」）</td></tr>'
    : rows.slice(0, COMP_HEAD).map(r => rowOf(r, false)).join('') +
      (rest.length ? '<tr class="pk-group-summary"><td colspan="5">' +
        '<button type="button" class="pk-group-toggle" data-exp-rest="' + rest.length +
        '" aria-expanded="false">其余 ' + rest.length + ' 个账号，展开</button></td></tr>' +
        rest.map(r => rowOf(r, true)).join('') : '');
  // 重建 tbody 后恢复展开选择（本函数随 5s 轮询重渲染倒计时）。
  applyRestToggle(body, restOpen.compBody);
  const pend = rows.filter(compPending);
  const sum = pend.reduce((n, r) => n + (r.credit || 0), 0);
  const erred = Number((compData && compData.errored) || 0);
  // 失败数必须显形：全失败时若只报「没有待领」，等于把故障说成「没活动」。
  $('compNote').innerHTML = !compData ? ''
    : (pend.length ? '待领 ' + pend.length + ' 个 · 合计 ' + fmtTok(sum) + ' 积分' : '无待领补偿')
      + (erred ? ' · <span style="color:var(--bad)">' + erred + ' 个查询失败</span>' : '');
}

async function loadCompensation() {
  if (!$('compBody')) return;
  try {
    compData = await api('compensation');
  } catch (e) {
    compData = null;
    $('compNote').innerHTML = '<span style="color:var(--bad)">读取失败：' + esc(e.message) + '</span>';
    return;
  }
  renderCompensation();
}

async function claimCompensation(uid) {
  try {
    const r = await api('accounts/' + encodeURIComponent(uid) + '/compensation', { method: 'POST' });
    if (r && r.error) toast('领取失败：' + (r.skip_reason || r.error), 'bad');
    else if (r && r.skip) toast('跳过：' + (r.skip_reason || '不适用'));
    else toast('补偿已领取' + (r && r.credit ? ' +' + r.credit : ''));
  } catch (e) {
    toast('领取失败：' + e.message, 'bad');
  }
  await loadCompensation();
}

async function claimCompensationAll() {
  const btn = $('btnCompAll');
  if (btn) btn.disabled = true;
  try {
    const d = await api('compensation_all', { method: 'POST' });
    const s = (d && d.summary) || {};
    // 先看 errored 再看 claimed：上游全挂时 claimed 恒 0，只看 claimed 会把故障说成「没活动」。
    if (s.errored) toast('补偿：' + s.errored + ' 个账号失败，已领 ' + (s.claimed || 0) + ' 个（+'
      + (s.credit || 0) + ' 积分）', 'bad');
    else toast('补偿：已领 ' + (s.claimed || 0) + ' 个（+' + (s.credit || 0) + ' 积分），其余无可领');
  } catch (e) {
    toast('全部领取失败：' + e.message, 'bad');
  } finally {
    if (btn) btn.disabled = false;
  }
  await loadCompensation();
}

if ($('btnCompRefresh')) $('btnCompRefresh').onclick = () => loadCompensation();
if ($('btnCompAll')) $('btnCompAll').onclick = claimCompensationAll;
if ($('compBody')) $('compBody').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-comp]');
  if (btn) claimCompensation(btn.getAttribute('data-comp'));
});

let lastPackages = null, lastPackagesAt = 0, lastPkLimit = 0;

// invalidatePackages 账号/凭据/余额变更后作废积分构成与到期明细缓存。
// 缓存没有 TTL，不加这一处会把旧 401 行一直顶到用户手点刷新——重新登录后
// 「点检查还是 401」的观感就是这么来的。
function invalidatePackages() { lastPackages = null; lastPackagesAt = 0; }

async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastPkLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;
    lastPackagesAt = Date.now();
    lastPkLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastPkLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

// 「刷新」始终实时查：必须显式 force —— 直接 `onclick = loadPackages` 会把事件对象当真值传进去，恒等于强刷。
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);

/* ══ 账号备注（只写本机 localStorage；账号池 / 用量两个视图共用同一份映射）══════
   键取 uid 前 8 位——与 renderUsageDim 账号维度的 key 和积分扣除表的 uid 前缀同源，
   三个表格因此都能直接 join。展示层只替换文本并挂 .rm-ghost 虚体，账号标识本身
   仍由服务端下发，不写回后端；备注是纯展示层信息，不参与选号 / 计费 / 鉴权。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function remarkOf(uid) {
  const k = remarkKey(uid);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* 编辑器的行来自 overview 缓存（账号池加载过就有），另外补上「已有备注但当前列表里
   没有」的键，避免账号被移除后备注看起来凭空消失。 */
function remarkFormRows() {
  const rows = ((overviewData && overviewData.accounts) || [])
    .map(s => ({ uid: remarkKey(s.uid), who: s.nickname || remarkKey(s.uid) }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  if ($('remarkVeil')) $('remarkVeil').classList.add('on');
}
function closeRemarks() { if ($('remarkVeil')) $('remarkVeil').classList.remove('on'); }
function saveRemarkForm() {
  const map = {};
  document.querySelectorAll('#remarkBody input[data-uid]').forEach(inp => {
    const v = inp.value.trim();
    if (v) map[inp.getAttribute('data-uid')] = v;
  });
  storeRemarks(map);
  closeRemarks();
  // 备注只影响本机展示：重渲染当前视图即可，无需后端参与。
  if (view === 'accounts') loadOverview(true);
  else if (view === 'usage' && usageData) renderUsage(usageData);
  toast('备注已保存（仅本机浏览器）');
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = () => { storeRemarks({}); renderRemarkBody(); };
if ($('btnRemarkSave')) $('btnRemarkSave').onclick = saveRemarkForm;

/* 账号身份色板（8 槽）。槽位 0 = var(--accent)（**主题主色**，浅色主题下
   自动跟随；刻意保留，因为它承载的是"第一条线=当前主题强调色"的既有语义，
   不是与状态语义撞车），槽位 1~7 走 CSS 变量 --bh-1..--bh-7，两套主题各一份。

   ── 各槽色相与选它的理由（色相为实测值） ──────────────────────────
   槽0 227°  var(--accent)   主题主色，非新增取舍
   槽1  58°  黄                语义色的 38°(warn) 与 154°(ok) 之间唯一能放的点
   槽2  96°  黄绿              离三语义色都远（≥46°），最"安全"的槽位
   槽3 135°  绿                在 warn/ok 之间的另一侧，与 58° 相距 77°
   槽4 191°  青                冷色区，与 accent(227°) 相距 36°
   槽5 247°  蓝紫              与 accent 相距 19°，靠明度/饱和度区分（见下）
   槽6 295°  品红              远离所有语义色（≥66°）
   槽7 343°  玫红              与 bad(2.6°) 相距 20°，靠低饱和暖粉区分
   相邻间距：77/40/42/42/42/42/40 度（槽1→槽7 内部链），最小 40°。

   ── 为何避开语义色 ────────────────────────────────────────────────
   绿/红/黄已被状态语义占用（--ok 可用 / --bad 失败 / --warn 限流告警）。
   账号身份色若与它们同色相，同一颜色就有两个互斥含义（"某账号" vs "状态"），
   读者必然误读 —— 这是 §十一"一个视觉通道只承载一种含义"的直接推论。
   故 1~7 槽都把色相放在距 warn/ok/bad 至少 20° 的位置（浅色主题下
   warn 37°、ok 155°、bad 3°，均已核验）。

   ⚠️ 这里有一个**已实测的取舍**，勿轻改：要求「8 色 + 相邻相差距 ≥40° +
   同时避开 全部 4 个语义色(含 accent) ±20°」三者**数学上不可同时成立** ——
   4 个禁区共占 160°，而 8 槽相邻 ≥40° 需要 320° 的弧，160+320 > 360。
   穷举求解证实：若同时避开 accent，最大可实现的相邻最小间距只有 38.5°。
   因此本色板选择：**槽位 0 复用 accent（不视为撞车）**，1~7 槽避开 warn/ok/bad
   且相邻间距 ≥40°。槽 5(247°) 与 accent 相距仅 19° 是这一取舍的唯一代价 ——
   它靠**明显不同的明度/饱和度**（accent 是高饱和靛蓝、槽5 是亮紫）来区分，
   这是 20° 色相判据之外的第二条判据，符合任务"或明度/饱和度明显不同"的允许。

   ── 两套主题（硬编码 hex 必须两套，见 index.html） ────────────────
   对比度实测（深底 #14171f / 浅底 #ffffff，WCAG 对比度）：
     槽1 13.3/4.6  槽2 14.1/4.5  槽3 13.5/4.5  槽4 11.8/4.5
     槽5  6.4/11.8 槽6  7.1/5.9  槽7  6.7/6.4   槽0(var(--accent)) 4.9/5.4
   全部达到非文本对比度 3:1 的要求，且多数 ≥4.5:1。 */
const BH_COLORS = [
  'var(--accent)', 'var(--bh-1)', 'var(--bh-2)', 'var(--bh-3)',
  'var(--bh-4)', 'var(--bh-5)', 'var(--bh-6)', 'var(--bh-7)',
];
/* 域元数据：cn = 国内（CodeBuddy），global = 国际（WorkBuddy）。
   两域的积分不同源、不可互换（连上游 base 与模型目录都不同），
   因此**跨域求和语义不成立** —— 余额趋势与模型页都必须分域展示，
   不做"国内 + 国际"的全局总计。 */
const BH_REALMS = [
  { key: 'cn', label: '国内' },
  { key: 'global', label: '国际' },
];
/* bhRealm 当前选中的域。null = 尚未初始化（首次加载时按数据自动选一个非空域）。 */
let bhRealm = null;
/* realmOf 归一化账号域：空/未知归入 'other'（不静默丢弃，让它可见）。 */
function realmOf(s) {
  const r = (s && s.realm) || '';
  return r === 'cn' || r === 'global' ? r : 'other';
}
/* pickRealm 选择默认域：优先记住的选择，其次第一个有数据的已知域。 */
function pickRealm(rows) {
  const have = {};
  rows.forEach(s => { have[realmOf(s)] = true; });
  const remembered = (function () {
    try { return localStorage.getItem('bhRealm'); } catch (e) { return null; }
  })();
  if (remembered && have[remembered]) return remembered;
  const first = BH_REALMS.find(r => have[r.key]);
  if (first) return first.key;
  return have.other ? 'other' : (BH_REALMS[0] && BH_REALMS[0].key) || 'cn';
}
/* filterRealm 取当前域的账号行。 */
function filterRealm(rows, realm) {
  return rows.filter(s => realmOf(s) === realm);
}
/* renderBalanceChart 画多账号余额折线。
   x 轴按**真实时间比例**排布（30 分钟等间隔），而非数据序号等距——
   因为"什么时候到账/掉分"本身就是这张图要回答的问题。 */
function renderBalanceChart(series) {
  const host = $('bhChart');
  const rows = (series || []).filter(s => (s.points || []).length >= 1);
  if (!rows.length) {
    host.innerHTML = '<div class="us-empty">暂无余额采样。服务每 30 分钟自动记录一点，' +
      '余额刷新后即出现（也可点上方「刷新」立即采样一次）。</div>';
    return;
  }
  let t0 = Infinity, t1 = -Infinity, lo = Infinity, hi = -Infinity;
  rows.forEach(s => s.points.forEach(p => {
    const t = Number(p.t) * 1000;
    if (t < t0) t0 = t;
    if (t > t1) t1 = t;
    const c = Number(p.c);
    if (c < lo) lo = c;
    if (c > hi) hi = c;
  }));
  if (t1 <= t0) t1 = t0 + 1;                  // 单点：给最小跨度，避免除零
  if (hi <= lo) hi = lo + 1;                  // 水平线：撑开一点，否则贴边
  // y 轴下界（20260918 用户反馈"丑"的真实根因）。
  //
  // 现象：坐标轴 1854~7499，但两条线都在 2400 以上 —— 底部一大片空白。
  // 根因：`lo = Math.max(0, lo - pad)` 把下界夹到 0。夹 0 本身是对的
  //       （§十：余额非负，不能画负刻度），但代价是最低值离 0 很远时底部全空。
  //
  // 关键区分（我一开始诊断错了）：所谓"中间空档"实测只有 440/7833 = 5.6%，
  // 不是问题；真正的浪费是 0 ~ 最低值 这段（30.6%）。
  //
  // 修法：下界取 max(0, 最低值-留白)，仅当最低值本身贴近 0（< 轴高 15%）时
  // 才贴 0（那时更符合"余额从 0 起"的直觉）。
  // 不违反 §十：下界仍是真实值域下沿，轴标签是真余额，不产出不可能的刻度。
  const pad = (hi - lo) * 0.12;
  const rawLo = Math.max(0, lo - pad);
  const dataLo = lo;
  hi += pad;
  const AXIS_ZERO_SNAP = 0.15;
  lo = (dataLo <= (hi - rawLo) * AXIS_ZERO_SNAP) ? 0 : rawLo;

  // 坐标系 1:1：W 取容器实际宽度，文字/线宽不再被 CSS 缩放（见 measureWidth 注释）。
  const W = measureWidth(host), H = 200, PL = 52, PR = 12, PT = 12, PB = 26;
  const iw = W - PL - PR, ih = H - PT - PB;
  const X = t => PL + iw * ((t - t0) / (t1 - t0));
  // Y 映射：单段线性（不压缩空档 —— 实测空档仅 5.6%，压缩反而放大它）。
  const Y = c => PT + ih - ih * ((c - lo) / (hi - lo));
  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';
  for (let i = 0; i <= 4; i++) {              // y 轴 4 档刻度
    const v = lo + (hi - lo) * i / 4;
    const y = Y(v);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' +
      fmtNum(Math.round(v)) + '</text>';
  }
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) + '" y2="' + (PT + ih) + '"/>';

  // 轴不从 0 起时**必须显式标注**（§十一：口径不标注会被误读）。
  // 不标的话，读者会把"两线贴得很近/很远"按 0 基线理解，从而**高估相对差距**。
  // 这里画一个标准的"截断轴"符号（轴左下两道斜杠）+ 文字说明。
  if (lo > 0) {
    const yb = PT + ih;
    out += '<path class="axbreak" d="M' + (PL - 4) + ' ' + (yb - 3) + ' l4 -6 M' + (PL + 1) + ' ' +
      (yb - 3) + ' l4 -6" fill="none" stroke-width="1.4"/>';
    out += '<text class="tk" x="' + (PL + 9) + '" y="' + (yb - 4) + '" text-anchor="start">轴不从 0 起</text>';
  }

  const spanDays = (t1 - t0) / 86400000;      // x 轴标签：首/中/末三点
  const fmtT = t => {
    const d = new Date(t);
    const mm = String(d.getMonth() + 1).padStart(2, '0');
    const dd = String(d.getDate()).padStart(2, '0');
    if (spanDays <= 2) return mm + '-' + dd + ' ' + String(d.getHours()).padStart(2, '0') + ':00';
    return mm + '-' + dd;
  };
  // x 轴刻度：与下方「每日变化」图**用同一套刻度密度**（都按天数 / 10 取整）。
  // 为什么必须一致（设计心理学 · 格式塔相似律）：两张图上下排列、共享同一个时间窗，
  // 读者想回答「10-15 那根大绿柱对应上图的哪一段」时靠的是纵向对齐刻度；
  // 旧实现上图固定 3 点、下图按天算 8 点，两张图刻度错位，无法对照。
  const tickCount = Math.max(2, Math.min(10, Math.ceil(spanDays / 3) + 1));
  for (let i = 0; i < tickCount; i++) {
    const t = t0 + (t1 - t0) * (tickCount === 1 ? 0 : i / (tickCount - 1));
    out += '<text class="tk" x="' + X(t).toFixed(1) + '" y="' + (H - 8) +
      '" text-anchor="' + (i === 0 ? 'start' : i === tickCount - 1 ? 'end' : 'middle') + '">' + esc(fmtT(t)) + '</text>';
  }

  rows.forEach((s, si) => {
    const color = BH_COLORS[si % BH_COLORS.length];
    const pts = s.points.map(p => ({ x: X(Number(p.t) * 1000), y: Y(Number(p.c)), c: Number(p.c), t: Number(p.t) * 1000, g: p.d }));
    // 平滑 <path>（monotone，不过冲 —— 见 smoothPath 注释）+ 统一线宽/线帽。
    // 线宽按**本图系列数**取档，两处折线共用同一函数，风格不会各走各的。
    out += '<path fill="none" stroke="' + color + '" stroke-width="' + lineWidthFor(rows.length) +
      '" stroke-linecap="round" stroke-linejoin="round" d="' + smoothPath(pts) + '"/>';
    // 点标记：**只标"显著变化"**，不是每个点都画圈。
    //
    // 为何改（20260918 用户反馈"丑"）：原先 delta !== 0 就画圈，151 个采样点
    // × 2 账号 ≈ 300 个绿/红圆点。折线本身已表达趋势，圆点是**重复编码**；
    // 而鲜艳小圆点是人眼**前注意加工**最易捕捉的特征 —— 结果注意力被 300 个点
    // 抓走，反而看不见"线"（想看的趋势）。且相邻点间距 ~7px、圆点直径 6px，
    // 已经糊成一条花带，退化为噪音。
    //
    // 阈值取该账号**自身跨度**的 3%（而非固定值）：水位 4000 的号与 400 的号，
    // "显著"的绝对量不同，用相对值才等价可比。跨度极小时保底 1，避免永不标记。
    const span = Math.max(1, hi - lo);
    const markMin = Math.max(1, span * 0.03);
    // 点数自适应：点数 > POINT_MARK_MAX 时不再逐点画实心小圆（见其注释），
    // 显著变化点仍然标记 —— 两者是不同用途：前者是"采样点"，后者是"事件"。
    const markAll = shouldMarkPoints(pts.length);
    pts.forEach((p, i) => {
      const prev = i > 0 ? pts[i - 1] : null;
      const delta = prev ? p.c - prev.c : 0;
      const sig = prev && Math.abs(delta) >= markMin;
      const up = delta > 0;
      const when = new Date(p.t).toLocaleString('zh-CN', { hour12: false });
      const head = (s.name || s.uid.slice(0, 8)) + '  ' + when + (p.g ? '（日粒度）' : '');
      const tip = head + '\n余额 ' + fmtNum(p.c) +
        (prev ? '\n较上一点 ' + (delta > 0 ? '+' : '') + fmtNum(delta) : '');
      if (sig) {
        // 显著变化：涨=绿、跌=红（唯一允许"借用"语义色的地方 —— 因为它
        // 表达的**就是**涨跌语义，不是账号身份）。
        out += '<circle cx="' + p.x.toFixed(1) + '" cy="' + p.y.toFixed(1) + '" r="2.6" ' +
          'fill="' + (up ? 'var(--ok)' : 'var(--bad)') + '"><title>' + esc(tip) + '</title></circle>';
      } else if (markAll) {
        out += drawPointMarker(p.x, p.y, true, color, tip);
      } else {
        // 点太密：不画圆点，但**保留透明热区**，悬停仍可查任意点。
        // 减少的是视觉噪音，不是信息量 —— 这是"降噪"与"减信息"的关键区别。
        out += drawPointMarker(p.x, p.y, false, color, tip);
      }
    });
    const last = pts[pts.length - 1];          // 末点加大命中圈，便于悬停
    out += '<circle cx="' + last.x.toFixed(1) + '" cy="' + last.y.toFixed(1) + '" r="9" fill="transparent">' +
      '<title>' + esc(s.name || s.uid.slice(0, 8)) + '  当前 ' + fmtNum(last.c) + '</title></circle>';
  });
  out += '</svg>';
  host.innerHTML = out;

  const lg = $('bhLegend');
  // 账号名加 truncate 包装：超长名（如「汪羽杰（工作相关内容不要发v）」）会挤压卡头，
  // 按 12em 截断 + 省略号，完整名放 title 悬浮可见（识别优先、完整信息可及）。
  if (lg) lg.innerHTML = rows.map((s, i) => {
    const nm = s.name || s.uid.slice(0, 8);
    return '<i class="sw" style="background:' + BH_COLORS[i % BH_COLORS.length] + '"></i>' +
      '<span class="lg-name" title="' + esc(nm) + '">' + esc(nm) + '</span>';
  }).join(' ');
}
/* renderDailyBars 画「每日变化」分组柱状图：x = 日期，每账号一根。
   与余额折线的区别：折线看"水位随时间"，这里看"每天的净变化量"（涨跌方向 + 幅度）。
   不完整日（今日 / 服务中途启用那天）用低透明度 + 虚线描边标出 —— 否则
   「今天只涨了一点」会被误读成「今天赚得少」，其实是当天还没采完。 */
function renderDailyBars(series) {
  const host = $('bhDaily');
  if (!host) return;
  // 收集所有日期（并集，升序）
  const daySet = new Set();
  series.forEach(s => (s.daily || []).forEach(d => daySet.add(d.day)));
  const days = [...daySet].sort();
  if (!days.length) {
    host.innerHTML = '<div class="us-empty">暂无按日数据（需跨过零点后才出现第一天）。</div>';
    const lg = $('bhDailyLegend');
    if (lg) lg.innerHTML = '';
    return;
  }
  // 每个账号每天的变化值，以及是否有任一不完整
  const map = new Map(); // uid -> {day: {change, full}}
  const fullMap = new Map();
  series.forEach(s => {
    const m = {};
    (s.daily || []).forEach(d => { m[d.day] = d.change; if (!d.full) fullMap.set(s.uid + '|' + d.day, true); });
    map.set(s.uid, m);
  });
  // y 轴范围：正负分开，零轴居中
  let hi = 0, lo = 0;
  series.forEach(s => (s.daily || []).forEach(d => {
    if (d.change > hi) hi = d.change;
    if (d.change < lo) lo = d.change;
  }));
  // ⭐ 离群点裁剪（20261021）：纵轴用 P95 分位而非极值，超出者单独标记。
  // 为什么：一次 +4414 的到账（如新手礼包）会把轴拉到 ±4414，其余日常 ±500
  // 的波动被压成 2px 短线 —— 韦伯-费希纳定律：人读的是**比例**，轴被离群值
  // 撑开后，真正想看的「今天比昨天多赚多少」在视觉上消失，80% 的像素在
  // 表达「接近零」（数据墨水比极低）。
  // 做法：把 |change| 排序取 P95 作为轴范围，超出者画成贴顶的三角标记 +
  // title 标注真实值 —— 既保留离群点的存在感，又让主体数据可读。
  const absVals = [];
  series.forEach(sr => (sr.daily || []).forEach(d => {
    if (!d || typeof d.change !== 'number') return;
    if (!days.includes(d.day)) return;
    absVals.push(Math.abs(d.change));
  }));
  if (absVals.length >= 8) {
    absVals.sort((a, b) => a - b);
    const p95 = absVals[Math.floor(absVals.length * 0.95)];
    // 只用 P95 收窄「明显比主体大一个量级」的离群点；不无条件裁剪，
    // 否则数据本身就在高位时会被误裁。判据：极值 > P95 的 3 倍才启用。
    const curMax = Math.max(Math.abs(hi), Math.abs(lo));
    if (p95 > 0 && curMax > p95 * 3) {
      hi = Math.min(hi, p95 * 1.12);
      lo = Math.max(lo, -p95 * 1.12);
    }
  }

  // 上下独立缩放 + 各留 12% 余量（不再强制对称）。
  // 为什么：强制对称（hi=lo=-span）在"几乎只涨"的数据上会把下半区留成
  // 5 倍空白（实测 +1998/-393 → 轴 ±1998），视觉上又空又怪，且正柱贴顶。
  // 独立缩放让柱高比例真实反映数值比例。
  // 边界断言（§十：不得产出物理上不可能的值）：日变化可正可负，故只在
  // 该侧确有数据时才外扩；两侧都无数据时兜底 ±1 避免除零。
  if (hi > 0) hi *= 1.12;
  if (lo < 0) lo *= 1.12;
  // 单侧有数据时，给另一侧留一点呼吸空间，否则零轴贴边、柱子像被裁掉。
  if (hi > 0 && lo === 0) lo = -Math.max(1, hi * 0.15);
  if (lo < 0 && hi === 0) hi = Math.max(1, -lo * 0.15);
  if (hi === 0 && lo === 0) { hi = 1; lo = -1; }

  // 坐标系 1:1（同余额折线，见 measureWidth 注释）。
  const W = measureWidth(host), H = 180, PL = 56, PR = 12, PT = 14, PB = 34;
  const iw = W - PL - PR, ih = H - PT - PB;
  const Y = v => PT + ih - ih * ((v - lo) / (hi - lo));
  const zeroY = Y(0);

  const nAcc = Math.max(1, series.length);
  const groupW = iw / days.length;
  const bw = Math.max(2, Math.min(14, (groupW - 8 - Math.max(0, nAcc - 1)) / nAcc));

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';
  // 网格（4 档）+ 零轴加粗
  for (let i = 0; i <= 4; i++) {
    const v = lo + (hi - lo) * i / 4;
    const y = Y(v);
    const isZero = Math.abs(v) < Math.max(hi, -lo) / 8;
    out += '<line class="' + (isZero ? 'ax' : 'gl') + '" x1="' + PL + '" y1="' + y.toFixed(1) +
      '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' +
      (v > 0 ? '+' : '') + fmtNum(Math.round(v)) + '</text>';
  }
  // 每日一组柱
  days.forEach((day, di) => {
    const gx = PL + di * groupW;
    series.forEach((s, si) => {
      const v = (map.get(s.uid) || {})[day];
      if (v === undefined) return;
      const color = BH_COLORS[si % BH_COLORS.length];
      // 组内柱之间留 1px 缝：无缝隙时相邻两柱会被知觉成一个整体
      // （格式塔共边界效应），看不出"两根独立的柱"。
      const gap = nAcc > 1 ? 1 : 0;
      const x = gx + (groupW - (bw * nAcc + gap * (nAcc - 1))) / 2 + si * (bw + gap);
      const y0 = Math.min(zeroY, Y(v)), h = Math.max(1, Math.abs(Y(v) - zeroY));
      const incomplete = fullMap.get(s.uid + '|' + day);
      // 零值画一条中性底痕（与 buka 规范一致：0 值也要可见）
      if (v === 0) {
        out += '<rect x="' + x.toFixed(1) + '" y="' + (zeroY - 1).toFixed(1) + '" width="' + bw.toFixed(1) +
          '" height="2" fill="var(--line)" rx="1"/>';
      } else {
        // 离群点（超出裁剪后的轴范围）：柱子贴边 + 顶部三角标记，
        // 让读者一眼看出「这根比轴还高」，而不是误以为它只有这么高。
        const clippedHi = v > hi, clippedLo = v < lo;
        out += '<rect x="' + x.toFixed(1) + '" y="' + y0.toFixed(1) + '" width="' + bw.toFixed(1) +
          '" height="' + h.toFixed(1) + '" fill="' + (v > 0 ? 'var(--ok)' : 'var(--bad)') + '"' +
          (incomplete ? ' opacity=".38" stroke="' + color + '" stroke-width="1" stroke-dasharray="2 2"' : '') +
          ' rx="1.5"/>';
        if (clippedHi || clippedLo) {
          const ty = clippedHi ? PT : PT + ih;
          out += '<path d="M' + (x + bw / 2).toFixed(1) + ' ' + (clippedHi ? ty : ty - 5).toFixed(1) +
            ' l-3.5 ' + (clippedHi ? 5 : -5) + ' h7 z" fill="var(--ink-2)" opacity=".9">' +
            '<title>超出纵轴范围（已裁剪），实际 ' + (v > 0 ? '+' : '') + fmtNum(v) + '</title></path>';
        }
      }
      out += '<title>' + esc(s.name || s.uid.slice(0, 8)) + '  ' + esc(day) + '\n' +
        (v > 0 ? '+' : '') + fmtNum(v) + (incomplete ? '\n⚠️ 该日采样不完整（当天未结束或中途启用）' : '') + '</title>';
    });
    // x 轴日期标签（稀疏）
    // 与上方「积分水位」图同一口径（天数 / 3 + 1，夹在 2~10 之间），保证可纵向对照。
    const every = Math.max(1, Math.ceil(days.length / Math.max(2, Math.min(10, Math.ceil(days.length / 3) + 1))));
    if (di % every === 0) {
      out += '<text class="tk" x="' + (gx + groupW / 2).toFixed(1) + '" y="' + (H - 12) +
        '" text-anchor="middle">' + esc(day.slice(5)) + '</text>';
    }
  });
  out += '</svg>';
  host.innerHTML = out;

  // 图例：柱色表示**涨跌**（绿=净增、红=净减），不表示账号——账号由组内
  // 左→右的位置区分（与下方表格行序一致）。故这里只说明颜色语义，
  // 再按位置列出账号顺序，避免"颜色=账号"的错误暗示。
  const lg = $('bhDailyLegend');
  if (lg) lg.innerHTML =
    '<span class="lg-key"><i class="sw" style="background:var(--ok)"></i>净增</span>' +
    '<span class="lg-key"><i class="sw" style="background:var(--bad)"></i>净减</span>' +
    '<span class="lg-sep"></span>' +
    '<span class="lg-order">组内左→右：' +
    series.map((s, i) => (i + 1) + '. ' + esc(s.name || s.uid.slice(0, 8))).join('　') +
    '</span>';
}
/* 取某账号的「今日/昨日」变化（用于表格列）。 */
function dayChangeOf(s, which) {
  const d = s.daily || [];
  if (!d.length) return null;
  if (which === 'today') return d[d.length - 1];
  return d.length >= 2 ? d[d.length - 2] : null;
}
function fmtChange(v, full) {
  if (v === null || v === undefined) return '—';
  const txt = (v > 0 ? '+' : '') + fmtNum(v);
  const color = v > 0 ? 'var(--ok)' : v < 0 ? 'var(--bad)' : 'inherit';
  // 不完整日用虚线底 + 提示，避免与完整日直接比较
  return '<span style="color:' + color + (full ? '' : '; opacity:.6') + '"' +
    (full ? '' : ' title="该日采样不完整（当天未结束或中途启用），不宜与完整日直接比较"') + '>' + txt + '</span>';
}
/* monthFlowOf 取某账号**当月**的消耗/到账合计，以及这份合计是否覆盖了整月。
   返回 { consumed, gained, covered, firstDay, today }：
     - consumed/gained：本月已发生部分的下界估计（口径同 Daily，相邻采样差值累加）；
     - covered=false 表示 s.daily 里没有当月 1 号及更早的点 → 窗口没覆盖整月，
       此时数值只是「窗口内那几天」的和，必须淡化并标注，否则会被当成整月值；
     - firstDay 是当月最早有数据的那天，用于标题说明到底从哪天算起。

   为什么不做「全年」：balance 折叠成日粒度后每账号每天只剩 1 个点，虽然
   折叠时已把当日流量写在点上（Point.Consumed/Gained），但 30 天窗口以外的
   日仍受 dayKeep 约束，且 balance_history 的 days 被钳在 30（panel.go）。
   超出窗口的月份这里拿不到数据，故只算本月、且显式标注覆盖范围。 */
function monthFlowOf(s, now) {
  const d = s.daily || [];
  const today = new Date(now || Date.now());
  const ym = today.getFullYear() + '-' + String(today.getMonth() + 1).padStart(2, '0');
  const firstOfMonth = ym + '-01';
  let consumed = 0, gained = 0, n = 0, firstDay = null;
  d.forEach(x => {
    if (!x || typeof x.day !== 'string' || x.day.slice(0, 7) !== ym) return;
    if (typeof x.consumed === 'number') consumed += x.consumed;
    if (typeof x.gained === 'number') gained += x.gained;
    n++;
    if (!firstDay || x.day < firstDay) firstDay = x.day;
  });
  return {
    consumed: consumed,
    gained: gained,
    covered: !!firstDay && firstDay <= firstOfMonth,
    firstDay: firstDay,
    days: n,
  };
}
/* fmtFlow 渲染「消耗/到账」这类恒非负的流量值。
   与 fmtChange 的区别：这些量不表示方向（消耗恒 >=0），故不用涨跌色，
   用中性色 + 弱化不完整日。0 显示为 "–"（无变化比 "0" 更易扫读）。 */
/* fmtFlow 渲染「消耗/到账」单个数值（今日或本月的一行）。
   「无数据」与「零」必须用**不同符号**（§十一 口径标注）。

   事故：原先零值用 en-dash「–」(U+2013)、无数据用 em-dash「—」(U+2014) ——
   两者都是一条横线，12px 下仅差 6px 宽，扫视时无法区分（用户看到「今日消耗 –」
   以为是"没数据"，实际是当天消耗 0）。故：零值显式写 0（灰化），无数据才「—」。
   dim=true 表示该数口径受限（不完整日 / 本月未覆盖整月），淡化 + title 说明。 */
function fmtFlow(v, dim, dimTitle) {
  if (v === null || v === undefined) return '<span class="fl-na">—</span>';
  const n = Number(v);
  if (!n) {
    return '<span class="fl-zero" title="' + (dim ? esc(dimTitle || "该日采样不完整") :
      "该区间无消耗记录（余额未下降过）") + '">0</span>';
  }
  return '<span class="fl-v' + (dim ? ' fl-dim' : '') + '"' +
    (dim ? ' title="' + esc(dimTitle || "该区间采样不完整，为下界估计") + '"' : '') + '>' +
    fmtNum(n) + '</span>';
}
/* fmtFlowCell 把「今日 / 本月」两行合成一个单元格（方案 A：不增列）。
   上行 = 今日（完整度口径同「今日」列），下行 = 本月。
   本月行有两种受限情形，都必须标注而不是静默给数字：
     - 窗口未覆盖整月（monthFlowOf().covered === false）→ 淡化 + 说明起算日；
     - bhWindow 选了「近 24 小时」→ 本月合计几乎等于今日，等于没意义，直接说明。 */
function fmtFlowCell(today, month, key, winDays) {
  const t = fmtFlow(today && today[key], !!today && !today.full,
    '该日采样不完整（当天未结束或中途启用），消耗/到账均为下界估计');
  let m;
  if (!month || !month.days) {
    m = fmtFlow(null); // 本月无任何采样
  } else if (Number(winDays) < 28) {
    // 窗口短于 28 天时「本月」必然不含全月，数字虽真但会误导 —— 只标下标不显示值。
    m = '<span class="fl-na" title="当前窗口不足以覆盖整月，切到「近 30 天」才可看本月值">—</span>';
  } else if (!month.covered) {
    m = fmtFlow(month[key], true,
      '窗口只覆盖本月 ' + String(month.firstDay || '').slice(5) + ' 起，本月值为该区间之和（下界）');
  } else {
    m = fmtFlow(month[key], false);
  }
  return '<td class="num fl" title="上行 今日 / 下行 本月">' +
    '<span class="fl-row">' + t + '</span>' +
    '<span class="fl-row fl-sub">' + m + '</span></td>';
}
function bhCard(k, v, tone) {
  return '<div class="st"><span class="k">' + esc(k) + '</span><span class="v' +
    (tone ? ' ' + tone : '') + '">' + esc(v) + '</span></div>';
}
/* renderRealmSeg 画域切换分段控件。只列出**有数据的域**（避免点了空白页），
   并在按钮上标明账号数，让"这个域有几个号"一眼可见。 */
function renderRealmSeg(all) {
  const seg = $('bhRealmSeg');
  if (!seg) return;
  const count = {};
  all.forEach(s => { const r = realmOf(s); count[r] = (count[r] || 0) + 1; });
  const items = BH_REALMS.filter(r => count[r.key]).concat(
    count.other ? [{ key: 'other', label: '其他' }] : []);
  if (!items.length) { seg.innerHTML = ''; return; }
  seg.innerHTML = items.map(r =>
    '<button type="button" aria-pressed="' + (r.key === bhRealm ? 'true' : 'false') + '" data-realm="' + esc(r.key) + '">' +
    esc(r.label) + ' · ' + count[r.key] + '</button>').join('');
  Array.prototype.forEach.call(seg.querySelectorAll('button'), function (b) {
    b.onclick = function () {
      bhRealm = b.getAttribute('data-realm');
      try { localStorage.setItem('bhRealm', bhRealm); } catch (e) { /* 隐私模式：忽略 */ }
      loadBalance();
    };
  });
}
/* renderBalanceFoot 画「按账号」表的合计行（**域内小计**）。
   口径纪律（§十：不得产出语义错误的数）：
     - 当前 / 净变化 / 今日 / 昨日 / 采样点 —— 可加，给出合计；
     - 最低 / 最高 —— **不可加**！各账号的低谷/高峰不在同一时刻，
       Σ最低 没有任何物理含义（会得出一个从未存在过的水位），故填 "—"。
   不做跨域总计：cn 与 global 的积分不同源、不可互换（见 BH_REALMS）。 */
function renderBalanceFoot(rows, now, winDays) {
  const host = $('bhAccFoot');
  if (!host) return;
  if (!rows.length) { host.innerHTML = ''; return; }
  // 合计行的本月口径与明细行必须**同源**（同一个 now、同一个窗口），
  // 否则行与合计会在"覆盖了几天"上打架。
  const footNow = now, footDays = winDays;
  let cur = 0, chg = 0, pts = 0;
  let todaySum = 0, todayHas = false, ydaySum = 0, ydayHas = false;
  let consSum = 0, consHas = false, gainSum = 0, gainHas = false;
  // 本月合计：逐账号求和（同域绝对量可加），覆盖范围按「是否所有账号都覆盖整月」
  // 取严 —— 只要有一个账号的本月不全，合计就不全，不能当成整月值展示。
  let mcSum = 0, mgSum = 0, mHas = false, mCovered = true, mFirstDay = null;
  rows.forEach(s => {
    cur += Number(s.current || 0);
    chg += Number(s.change || 0);
    pts += (s.points || []).length;
    const t = dayChangeOf(s, 'today'), y = dayChangeOf(s, 'yesterday');
    if (t && typeof t.change === 'number') { todaySum += t.change; todayHas = true; }
    if (y && typeof y.change === 'number') { ydaySum += y.change; ydayHas = true; }
    // 消耗/到账可跨账号相加（都是同域的绝对量），但**同为下界估计**
    if (t && typeof t.consumed === 'number') { consSum += t.consumed; consHas = true; }
    if (t && typeof t.gained === 'number') { gainSum += t.gained; gainHas = true; }
    const mo = monthFlowOf(s, footNow);
    if (mo.days) {
      mHas = true;
      mcSum += mo.consumed;
      mgSum += mo.gained;
      if (!mo.covered) mCovered = false;
      if (mo.firstDay && (!mFirstDay || mo.firstDay < mFirstDay)) mFirstDay = mo.firstDay;
    }
  });
  const num = (v, has) => has
    ? '<td class="num" style="color:' + (v > 0 ? 'var(--ok)' : v < 0 ? 'var(--bad)' : 'inherit') + '">' +
      (v > 0 ? '+' : '') + fmtNum(v) + '</td>'
    : '<td class="num na">—</td>';
  // 可用账号数：该域内**当前真正能干活**的账号（未禁用且未暂停）。
  // 为什么放在合计行：「合计余额」是历史水位之和，可能包含已死号的残留 ——
  // 只看余额会高估可用资源，必须并排给出「这些钱有几个号在扛」。
  let usable = 0;
  rows.forEach(s => {
    const a = acctStateOf(s.uid);
    if (!a) { usable += 1; return; } // 无状态记录：按可用计（不无端扣减）
    if (!a.disabled && !a.paused) usable += 1;
  });
  host.innerHTML = '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>合计（' + esc(realmLabel(bhRealm)) + '）' +
      '<span class="hint" style="margin-left:8px;color:var(--ink-3)">可用 ' + usable + '/' + rows.length + '</span>' +
    '</td>' +
    '<td class="num">' + fmtNum(cur) + '</td>' +
    // 最低/最高不求和——见函数头注释
    '<td class="num na" title="各账号低谷不在同一时刻，不可相加">—</td>' +
    '<td class="num na" title="各账号高峰不在同一时刻，不可相加">—</td>' +
    '<td class="num" style="color:' + (chg > 0 ? 'var(--ok)' : chg < 0 ? 'var(--bad)' : 'inherit') + '">' +
      (chg > 0 ? '+' : '') + fmtNum(chg) + '</td>' +
    num(todaySum, todayHas) + num(ydaySum, ydayHas) +
    fmtFlowCell({ consumed: consSum, gained: gainSum, full: true }, { consumed: mcSum, gained: mgSum, days: mHas ? 1 : 0, covered: mCovered, firstDay: mFirstDay }, 'consumed', footDays) +
    fmtFlowCell({ consumed: consSum, gained: gainSum, full: true }, { consumed: mcSum, gained: mgSum, days: mHas ? 1 : 0, covered: mCovered, firstDay: mFirstDay }, 'gained', footDays) +
    '<td class="num">' + fmtNum(pts) + '</td>' +
    '</tr>';
}
async function loadBalance() {
  const host = $('bhChart');
  const days = Number(($('bhWindow') || {}).value || 30);
  try {
    const d = await api('balance_history?days=' + days);
    const all = d.series || [];
    // 首次进入（或记忆的域已无数据）时确定默认域；之后保持用户的显式选择。
    if (bhRealm === null || !all.some(s => realmOf(s) === bhRealm)) {
      bhRealm = pickRealm(all);
    }
    renderRealmSeg(all);
    const rows = filterRealm(all, bhRealm);
    renderBalanceChart(rows);
    renderDailyBars(rows);
    // 分域小计：同域内可加（语义成立）；**不做跨域总计**——cn 与 global 的
    // 积分不同源、不可互换，相加得出的数没有意义（见 BH_REALMS 注释）。
    let total = 0, delta = 0;
    rows.forEach(s => { total += Number(s.current || 0); delta += Number(s.change || 0); });
    // 可用账号数：未禁用且未暂停（口径与合计行一致，两处必须同源）。
    let usableCount = 0;
    rows.forEach(s => {
      const a = acctStateOf(s.uid);
      if (!a || (!a.disabled && !a.paused)) usableCount += 1;
    });
    const st = $('bhStats');
    if (st) st.innerHTML =
      bhCard('账号', String(rows.length)) +
      // 可用数单列：合计余额可能包含已死号的残留，必须并排看「几个号在扛这些分」。
      bhCard('可用', String(usableCount) + '/' + String(rows.length), usableCount < rows.length ? 'bad' : '') +
      bhCard('当前合计', fmtNum(total)) +
      bhCard('窗口净变化', (delta > 0 ? '+' : '') + fmtNum(delta), delta > 0 ? 'ok' : delta < 0 ? 'bad' : '') +
      bhCard('采样点数', fmtNum(rows.reduce((a, s) => a + (s.points || []).length, 0)));
    const tb = $('bhAccBody');
    if (tb) tb.innerHTML = rows.length ? rows.map((s, i) =>
      '<tr><td class="mark"><i class="dot" style="background:' + BH_COLORS[i % BH_COLORS.length] + '"></i></td>' +
      (() => {
        const a = acctStateOf(s.uid);
        // 账号已禁用/暂停：名字后加小标签。曲线仍展示（历史数据有意义），
        // 但要让读者知道「之后不再更新」的原因，否则会把断更误读成没在用。
        const mark = !a ? ''
          : a.disabled ? '<span class="tag bad" style="margin-left:6px">' + esc(disabledKind(a.reason).label) + '</span>'
          : a.paused ? '<span class="tag warn" style="margin-left:6px">暂停选号</span>'
          : '';
        const nm = s.name || s.uid.slice(0, 8);
        const tip = a && a.reason ? nm + ' · ' + a.reason : nm;
        return '<td title="' + esc(tip) + '">' + esc(nm) + mark + '</td>';
      })() +
      '<td class="num">' + fmtNum(s.current) + '</td>' +
      '<td class="num">' + fmtNum(s.min) + '</td>' +
      '<td class="num">' + fmtNum(s.max) + '</td>' +
      '<td class="num" style="color:' + (s.change > 0 ? 'var(--ok)' : s.change < 0 ? 'var(--bad)' : 'inherit') + '">' +
        (s.change > 0 ? '+' : '') + fmtNum(s.change) + '</td>' +
      (function () {
        const t = dayChangeOf(s, 'today'), y = dayChangeOf(s, 'yesterday');
        // 本月口径：与窗口脱钩判定覆盖范围（见 monthFlowOf 注释）
        const mo = monthFlowOf(s, d.now);
        return '<td class="num">' + fmtChange(t && t.change, t && t.full) + '</td>' +
               '<td class="num">' + fmtChange(y && y.change, y && y.full) + '</td>' +
               // 消耗/到账：上行今日、下行本月（方案 A，不增列）
               fmtFlowCell(t, mo, 'consumed', days) +
               fmtFlowCell(t, mo, 'gained', days);
      })() +
      '<td class="num">' + (s.points || []).length + '</td></tr>').join('')
      : '<tr><td colspan="11"><div class="empty"><div class="big">该域暂无采样</div>' +
        '切到其他域，或等待下一次余额刷新</div></td></tr>';
    renderBalanceFoot(rows, d.now, days);
    const note = $('bhNote');
    if (note) note.textContent = '更新于 ' + new Date(d.now).toLocaleTimeString('zh-CN', { hour12: false });
  } catch (e) {
    if (host) host.innerHTML = '<div class="us-empty">读取余额趋势失败：' + esc(e.message) + '</div>';
  }
}

/* ── 图表与域标签工具（迁移遗漏，20261021 补齐）── */
const CHART_FALLBACK_W = 760;

/* measureWidth 量容器内容宽（去掉 padding）。
   为什么用 clientWidth-padding 而不用 getBoundingClientRect：
   后者包含 border、且受 CSS transform 影响，算出来的坐标系
   会与 CSS 布局尺寸不一致（一个被 transform 缩放的容器会让图表
   再次被非等比拉伸）。 */
function measureWidth(el) {
  if (!el) return CHART_FALLBACK_W;
  const cs = getComputedStyle(el);
  const pl = parseFloat(cs.paddingLeft) || 0;
  const pr = parseFloat(cs.paddingRight) || 0;
  const w = el.clientWidth - pl - pr;
  return w > 40 ? Math.round(w) : CHART_FALLBACK_W;
}

/* ── 折线共用的三个渲染规则（两处折线图必须一致，故抽成共享函数） ──────
   为什么抽出来：余额趋势图与账号×模型图是**同类图形**（都是"真实时间 x
   轴 + 值 y 轴的多序列折线"）。同一类图形若各写一套线宽/平滑/点标记，
   两处必然逐渐漂移，用户看到的是"同一个面板里两种线风格" —— 这正是
   本次要修的问题本身。故把三者收敛为共享实现，两处只负责传数据。 */

/* lineWidthFor 按**系列数**给线宽：系列多时略细、少时略粗。
   为什么与系列数挂钩：线宽是"信息密度"的补偿手段。1~3 条线时线细会显得
   单薄、且单条线的视觉权重不足以承载注意力；>6 条时线越细越不糊 ——
   60px 高的绘图区里 8 条 2.0px 线会互相吃掉、趋势读不出来。
   这与既有的"点标记按点数自适应"是同一思路：**图形密度决定笔触粗细**。
   分档取整（2.0/1.8/1.5）而非连续函数，是为了让同一系列数在任何容器宽度
   下都得到同一线宽，避免"窗口一拉线宽就变"的抖动感。 */
function lineWidthFor(n) {
  if (n > 6) return 1.5;
  if (n >= 4) return 1.8;
  return 2.0;
}

/* smoothPath 用 **Monotone 三次插值（Fritsch–Carlson）** 生成平滑 <path> 的
   d 属性。入参 pts = [{x, y}, ...]（已投影到屏幕坐标，x 单调递增）。

   ⚠️ 为什么**必须**是 monotone，不能用 Catmull-Rom / 普通三次样条：
   后两者只保证**经过**控制点与一阶连续，**不保证不过冲**。在"余额/用量"
   这类统计图上，过冲会在两个真实点之间画出**数据里不存在的波峰或负值** ——
   例如余额从 4000→4100 的两点之间被样条拱到 4150，用户据此判断"当时冲到
   过 4150"，而成因只是曲线形状。这是**虚构数据**，比"线条不好看"严重得多。
   Fritsch–Carlson 的做法是：先按三点斜率算切线，再把切线限制在相邻割线
   斜率的 3 倍以内（即保证插值函数在每个子区间内单调），于是平滑段
   **始终夹在两端真实值之间**，不会产生局部极值。

   边界：点数 <2 退回直线（无插值意义）；点数 ==2 直接给一条线段
   （两点之间任何"平滑"都是凭空造的弧度）。 */
function smoothPath(pts) {
  if (!pts || pts.length < 2) return '';
  if (pts.length === 2) {
    return 'M' + pts[0].x.toFixed(1) + ' ' + pts[0].y.toFixed(1) +
      'L' + pts[1].x.toFixed(1) + ' ' + pts[1].y.toFixed(1);
  }
  const n = pts.length;
  // 1) 相邻割线斜率 d[i] = (y[i+1]-y[i]) / (x[i+1]-x[i])
  const dx = [], dy = [], d = [];
  for (let i = 0; i < n - 1; i++) {
    dx[i] = pts[i + 1].x - pts[i].x;
    dy[i] = pts[i + 1].y - pts[i].y;
    d[i] = dx[i] === 0 ? 0 : dy[i] / dx[i];
  }
  // 2) 各点切线 m[i]：端点用单侧割线，内部点用相邻割线的加权平均
  //    （Fritsch–Carlson 的标准初始化；下一步再对 m 作单调性约束）。
  const m = new Array(n);
  m[0] = d[0];
  m[n - 1] = d[n - 2];
  for (let i = 1; i < n - 1; i++) {
    if (d[i - 1] * d[i] <= 0) {
      m[i] = 0;                     // 极值点/拐点：切线必须为 0，否则必然过冲
    } else {
      m[i] = (d[i - 1] + d[i]) / 2;
    }
  }
  // 3) 单调性约束（本算法的关键）：把每点切线夹到相邻割线的 3 倍以内。
  //    这一步才是"单调保持"的来源；只做第 2 步仍会过冲。
  for (let i = 0; i < n - 1; i++) {
    if (d[i] === 0) {               // 水平段：两端切线都归零，段内恒为常数
      m[i] = 0;
      m[i + 1] = 0;
      continue;
    }
    const a = m[i] / d[i], b = m[i + 1] / d[i];
    const s = a * a + b * b;
    if (s > 9) {                    // 限制在半径 3 的圆内（Fritsch–Carlson 判据）
      const t = 3 / Math.sqrt(s);
      m[i] = t * a * d[i];
      m[i + 1] = t * b * d[i];
    }
  }
  // 4) 转成三次贝塞尔：控制点 = 端点沿切线外推 1/3 段长（与 Hermite 等价）
  let out = 'M' + pts[0].x.toFixed(1) + ' ' + pts[0].y.toFixed(1);
  for (let i = 0; i < n - 1; i++) {
    const c1x = pts[i].x + dx[i] / 3, c1y = pts[i].y + m[i] * dx[i] / 3;
    const c2x = pts[i + 1].x - dx[i] / 3, c2y = pts[i + 1].y - m[i + 1] * dx[i] / 3;
    out += 'C' + c1x.toFixed(1) + ' ' + c1y.toFixed(1) + ' ' +
      c2x.toFixed(1) + ' ' + c2y.toFixed(1) + ' ' +
      pts[i + 1].x.toFixed(1) + ' ' + pts[i + 1].y.toFixed(1);
  }
  return out;
}

/* POINT_MARK_MAX 点标记（实心小圆）的**点数上限**：超过即不画圆点。
   为什么：60 个点 × 每点一个 r=2.2 实心圆，在 ~7px 点距下糊成花带——
   圆点是前注意加工最易捕捉的特征，注意力被它抓走反而看不到"线"（趋势）。
   阈值 30 的经验来源：绘图区宽约 900px 时，30 点在 30px 间距下圆点互不
   粘连，仍能起"采样点"的作用；再多就退化为噪音。
   ⚠️ 不画圆点 ≠ 少给信息：hover 仍靠透明大热区（见 POINT_HIT_R）逐点可查，
   且"显著变化"的点在两个图上都仍然保留标记（降噪 ≠ 减信息）。 */
const POINT_MARK_MAX = 30;
/* POINT_HIT_R 无圆点时使用的**透明热区半径**：圆点不画了，但命中判定不能
   跟着消失，否则 tooltip 只能靠线宽命中、悬停极难触发（线宽仅 1.5~2px）。
   取 6px ≈ 手指/光标可稳定停留的范围，且相邻热区不重叠（点距通常 >12px）。 */
const POINT_HIT_R = 6;

/* shouldMarkPoints 是否画实心圆点（点数自适应）。
   为什么做成函数而不是各处内联比较：这条规则是**两个折线图共有的产品决策**，
   分散写会漂移；且集中一处后"阈值"只有一个真源，调整时不会漏改一处。 */
function shouldMarkPoints(n) {
  return n <= POINT_MARK_MAX;
}

/* drawPointMarker 画一个数据点的标记 + hover 文案。
   marked=true → 实心小圆（能被看见）；false → 透明大热区（不可见但可悬停）。
   两个折线图共用，保证"看不见的点同样查得到值"这一行为在两处一致。 */
function drawPointMarker(x, y, marked, color, title) {
  const r = marked ? 2.6 : 0;
  const t = esc(title);
  if (marked) {
    return '<circle cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="' + r +
      '" fill="' + color + '"><title>' + t + '</title></circle>';
  }
  return '<circle cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="' + POINT_HIT_R +
    '" fill="transparent"><title>' + t + '</title></circle>';
}


/* realmOf 归一化账号域：空/未知归入 'other'（不静默丢弃，让它可见）。 */

function realmLabel(key) {
  const r = BH_REALMS.find(x => x.key === key);
  return r ? r.label : '其他';
}

/* ── 数值格式化（迁移遗漏）── */
function fmtNum(v) {
  const n = Number(v) || 0;
  return n.toLocaleString('zh-CN');
}
/* fmtTokens 人类可读 token 量（额度评估的数量级都是"亿"）。 */
function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1e8) return (n / 1e8).toFixed(2) + ' 亿';
  if (n >= 1e4) return (n / 1e4).toFixed(1) + ' 万';
  return String(n);
}
