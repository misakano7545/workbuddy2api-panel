<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  Web 面板 · OAuth 浏览器登录 · 账号池轮转 · 熔断与冷却 · 会话粘性 · **联网检索** · 定时签到 / 活跃 / 旅行 / 保活 · <b>成长任务一键完成（17/18）</b> · 流式 / 非流式
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26.10-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

> **本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强分支**（fork）。
> 在上游基础上重构了可视化运维层；对上游的同步止于其停更前的最后一次更新（`ea8b1e5`），此后由本分支独立演进。
> 差异概览见 [与上游的差异](#-与上游的差异)；上游设计的精巧之处（账号池调度、错误分类、提示词体系）原样保留，详见下文与上游 README。
>
> 感谢 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel/)，以及原仓库删库后的延续 [HanawaBanana/workbuddy2api](https://github.com/HanawaBanana/workbuddy2api)。

> [!IMPORTANT]
> **本仓的源头 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 早已删库停更**：原仓库连同代码、issue 与提交历史整体消失，不再可访问。本仓属于那次删库之后延续下来的分支，链路为 `Sliverkiss/workbuddy2api` → `linguo2625469/workbuddy2api-panel` → 本仓，**中间每一环都没有官方支持与后续保障**。
>
> **本仓的直接上游 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 已于 2026-10-21 宣布停止维护**：不再更新代码、不跟进上游变化、不再处理新的 issue / PR。原话给出的原因是上游开始禁用反代、作者精力有限，以及批量小号 / 售卖额度的滥用。
>
> 本仓不是它的官方延续，是自用增强分支，**仍在按自己的节奏维护**——已合入的修复不回退，问题与 PR 在本仓处理。
>
> **维护方式披露**：本仓的 issue / PR 处置、代码审查、测试与修复实现由 **Ekko Studio + Hermes Agent**（AI 编码 agent）辅助完成，合并与发布决策由维护者本人作出。这与上游停更前一贯的披露方式相同，请知悉后再使用或参与贡献。
>
> **关于作者与维护节奏**：本仓作者现实身份是一名在读大学生、摄影爱好者，也是玉林市公共汽车公司的公交迷。因家庭经济困难，周末需兼职于玉林市某公司从事相关工作，业余还有自己的兴趣与生活。**这不代表作者有全部精力投入维护**——本项目是兴趣驱动，更新随缘、响应不定，可能出现较长时间没有提交；也请不要把它当作有 SLA 的商业项目或有求必应的服务。作者没有跑路，只是精力有限，看到的问题会在有余力时处理。
>
> 尽管时间有限，作者仍保有一部分精力用于维护本项目——在自己的电脑上对相关请求做抓包分析（自有设备、本人账号），据此定位并修复接口变化。**感谢上游仓库及相关维护者，在作者最艰难的时期仍给予支持、陪着这个项目继续开发；也感谢腾讯 WorkBuddy 提供的相关服务。**
>
> **AutoBot CI（开发中）**：本仓正在开发 `.github/workflows/autobot.yml` + 根目录 `AutoBot.yml`——每天定时 clone 一批同类实现（含本仓上游与其他参考仓库），由 agent 读代码写差异报告，再按报告写 Go 实现并开 PR。**该流水线目前处于开发阶段**：分片策略、报告质量与自动 PR 的筛选规则都还在调整，可能误报、漏报、开出需要人工打回的 PR，请勿把它的产出当作既成能力依赖；稳定性与信任度以后续说明为准。
>
> 风险自评：**上游随时可能彻底收紧导致通道不可用**；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

> [!WARNING]
> **郑重提醒**——这段话针对两类人：**现实中与作者同校的使用者**，以及**拿本项目搭建对外中转站、做批量小号 / 售卖额度等恶意滥用的人**。
>
> 作者无法追踪、也不会去追踪任何人的行为，不对任何人行使监控。但必须把话说清楚：上述用法**严重违背上游项目作者与维护者的初衷**，也是消耗本仓作者维护动力的最大来源——本项目免费、开源、由一名学生用课余时间维护，不是给任何人拿去转卖的资源。
>
> 如果哪一天这个项目**确确实实让作者心力憔悴、不再有动力维护，本仓会像上游那样停止维护**，且不另行通知。上游的今天，可能就是本仓的明天，请自重。
>
> 同样的底线也适用于**同校使用者**：若有人出于一己私利，或以作者无法理解、也不知情的行为，把本项目的情况反映到**学校信息中心**、作者所在的**二级学院**，乃至**学院院长与校方其他上级领导**，藉此换取奖学金等个人好处，**本仓也会同时停止维护**。这不是赌气——接受核查、说明与后续处理所占用的时间与心力，已经超出本仓能承受的维护成本。

## 项目简介

WorkBuddy2API 是一个自托管的 **OpenAI 兼容反向代理网关**，将腾讯 CodeBuddy（`copilot.tencent.com`）账号包装为统一服务。入站支持 `chat_completions`（`POST /v1/chat/completions`）、`codex_responses`（`POST /v1/responses`）与 `anthropic_messages`（`POST /v1/messages`，同样接受 `POST /messages`）。

- 官方不提供 OpenAI 形态的开放 API，本项目通过 **OAuth 设备授权**（面板「添加账号」或 `login.sh`）获取账号凭证，在网关侧做 token 自动刷新、账号池调度与流量治理；
- 面向 **个人多账号** 场景：多账号共享、单号故障自动换号、冷却 / 熔断防止雪崩、会话粘性保证多轮上下文不跳号；
- 对客户端暴露 `chat_completions`、`codex_responses` 与 `anthropic_messages`。模型名原样交给上游，不把 `claude-*` 改写成别的模型。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 CodeBuddy 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[安全与合规](#安全与合规)。

## 核心能力

| 能力 | 说明 |
|---|---|
| 🔑 **OAuth 一键登录** | `login.sh` / 面板设备授权流程，三站点可选（CN / 官方国际站 / 旧桌面端），自动落盘凭证并加载新账号 |
| 🔄 **多账号池** | 快过期积分加权 + 成本分层 + 加权随机选号，Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避（封顶 `soft_rate_max`）、404 固定 60s 短冷却、402 硬冷却至次日 04:00、连续失败熔断、在途租约限流 |
| 🧲 **会话粘性** | 同一会话（`conversation_id`）尽量绑定同一账号，TTL 滚动续期，失败自动解绑，可镜像 Redis 防重启丢失 |
| ⏰ **定时任务** | 签到（09/21 点，末尾自动跑**连登管家**：兑换已解锁档位 + 抽完抽奖次数）+ 活跃上报（10 点，点亮连登 / 解锁领养 + streak 自检；国际号另跑**网页会话腿**，见下）+ 猫猫旅行（09/21 点，独立排程）+ token 保活（22 点）+ **成长任务队列**（11 点，自动跑一遍任务中心待办），各类独立开关 |
| ⚡ **流式 + 非流式** | 出站强制 `stream:true`；SSE 帧按规范白名单重建；非流式由本地聚合为单响应 |
| 🧠 **推理模型兼容** | DeepSeek 思维链注入（`thinking.type=enabled` + 默认档）、`reasoning_content` 多轮回填、effort 档位自动对齐到模型支持的**最近档**（同距优先模型默认档，行里显示生效档位）；网关侧 `thinking.*` 档位策略（客户端优先 / 网关优先 + 作用域「只档位 / 档位与开关」，含按模型覆盖、`off` 尊重与保存后热生效） |
| 💬 **系统提示词体系** | 网关自有提示词替换客户端 system（默认 `custom`），从源头消灭 system 来源的内容误报；`passthrough` 遇拦截自动降级重试 |
| 🗑️ **指纹脱敏** | 出站请求体黑名单指纹字段清洗（可关闭），与提示词体系两层叠加 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / token 速率 / uid）；`/healthz` 带 `service` 身份标识可接负载均衡 / 宿主探活 |
| 💾 **状态持久化** | 池状态本地原子落盘 + Upstash Redis 异步镜像（可选），重启择新恢复 |
| 🖥️ **Web 管理面板** | 内嵌单页面板（明暗主题），账号运维 / 模型档位查询 / 在线改配置（热生效）/ 运行日志 / 积分任务，见 [Web 管理面板](#-web-管理面板) |

## 代理池与账号绑定

面板「代理池」支持一行一个批量添加，接受 `http`、`https`、`socks5`、`socks5h` URL，包括用户名密码。空行忽略，重复项跳过，无效行单独报告，其他有效行仍保存。例如：

```text
http://user:password@127.0.0.1:8080
https://127.0.0.1:8443
socks5://user:password@127.0.0.1:1080
socks5h://127.0.0.1:1081
```

账号池每行的代理菜单保存后立即生效：

| 模式 | 出站行为 |
|---|---|
| 默认随机 | 每个 HTTP 请求独立从可用节点中随机选择；该请求的重定向和响应流保持同一节点 |
| 直连 | 使用原有直连传输 |
| 固定代理 | 只用绑定节点；节点故障时保留绑定并返回代理错误 |

分区默认出口（面板「代理池 → 分区默认出口」）按 realm 指定：`cn` 与 `global` 各一条，账号行没单独指定代理时取它。查表顺序是**账号绑定 > realm 默认 > 随机池 > 直连**——账号行里点过「直连」的号不会被 realm 默认覆盖。

池为空时默认随机使用直连。池中已有节点但没有可用节点时返回错误。代理故障不处罚账号，不触发额外换号重试。聊天、图片、搜索、嵌入、token 刷新、余额和账号后台任务使用同一账号路由入口。未关联账号的 OAuth 授权与更新下载沿用原出站行为。

HTTP/HTTPS 使用标准代理传输；`socks5` 在本机解析目标域名，`socks5h` 由代理解析。各节点复用独立连接池，沿用现有 DNS、TLS、响应头超时和流式空闲控制。禁用或删除只关闭空闲连接，在途请求继续完成。

自动测活默认每 60 秒运行，单次超时 10 秒，最多 8 个节点并发。默认检测 `https://www.workbuddy.cn/`，最终响应为 2xx/3xx 即成功。新增节点与重启后的节点先检测，首次成功后进入候选；连续失败 3 次自动禁用，之后连续成功 2 次恢复。面板可修改 URL、间隔、超时与阈值，也可单独或全部检测。手动禁用优先，检测成功仍保持禁用。业务限流、余额不足等响应不影响测活状态。

节点、手动启停、账号绑定与测活设置原子保存到 `state_file` 同目录的 `proxy-pool.json`，健康状态仅保存在内存。代理列表、接口响应和错误日志不返回密码；编辑时密码留空保持原值，从 URL 删除用户名可移除认证。被账号或 realm 默认引用的节点禁止删除（HTTP 409），需先解除引用；从面板删除账号时同步清理绑定。

管理接口沿用面板鉴权：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET / POST | `/panel/api/proxies` | 列表与批量添加（`{"lines":"..."}`） |
| PATCH / DELETE | `/panel/api/proxies/{id}` | 编辑（`name`、`url`、`password`、`enabled`）与删除 |
| POST | `/panel/api/proxies/{id}/check` | 排队检测单个节点 |
| POST | `/panel/api/proxies/check` | 排队检测所有节点 |
| PUT | `/panel/api/proxies/health` | 保存 `url`、`interval_seconds`、`timeout_seconds`、`failure_threshold`、`success_threshold` |
| PUT | `/panel/api/accounts/{uid}/proxy` | 保存 `{"mode":"random"}`、`{"mode":"direct"}` 或 `{"mode":"fixed","proxy_id":"..."}` |
| PUT | `/panel/api/proxies/realm/{realm}` | 同上格式，保存 `cn` / `global` 的分区默认出口 |

## 🎯 成长任务一键完成（17/18）

官方「成长计划」的 18 个成长任务中，**17 个可在面板上一键纯 API 完成**——无需安装官方客户端、无需人工交互，点一下「一键完成」即自动推进进度、等待异步计分落定并**自动领奖**。剩余任务展示操作指引。

### 任务覆盖与奖励

| 任务 | 奖励 | 一键完成方式 |
|---|---|---|
| `first_buddy` | +300c +8e | 解锁上报 → 同意协议 → 领养第一只 Buddy |
| `create_canvas` | +300c +5e | 设计画布创建事件组（Ardot 遥测） |
| `chat_5` | +100c | 对话活跃上报 ×5（自动补足差额） |
| `Model_chat_GLM5.2` | +100c +5e | glm-5.2 真实对话一次（发一条短消息） |
| `RichMeow_Chat` | +100c +5e +UR Buddy | 桌面端对话事件链（6 事件，含成功回执） |
| `Buddy_App` | +100c +5e | Buddy 应用「发现→进入→授权」事件链 |
| `Buddy_App_QQ` | +50c +5e | 企鹅教师助手进入事件链（与上一条共用） |
| `automation_1` | +100c +5e | 定时任务创建成功事件 |
| `Library_read` | +100c +5e | 资料库阅读点击（web 域上报） |
| `template_5` | +100c +5e | 模板使用事件组 ×5 |
| `playbook_prompt` | +100c +5e | 灵感案例「做同款」发送事件 |
| `expert_5` | +100c +5e | 真实专家召唤+使用链 ×5（专家市场拉真实专家 → 真实对话 → 使用事件） |
| `Expert_team_use_3` | +100c +5e | 专家团召唤+使用链 ×3 |
| `Hp_Appearance` | +100c +5e | 主题设置 + 皮肤生效事件 |
| `Expert_lighthouse` | +100c +5e | 轻量云专家召唤+使用链（真实对话 requestId，**可免费领一个月轻量服务器**） |
| `skill_1` | +100c +5e | 真实对话 + 技能加载事件（skill_info） |

**全新账号一键全做完 ≈ +1950 credits +78 能量**，其中仅数个任务涉及真实对话（`Model_chat_GLM5.2` 一条、`expert_5`/`Expert_team_use_3`/`skill_1` 各数条 fast-model 短对话），其余全部为行为事件上报，零对话消耗。

### 不可自动的 1 个

| 任务 | 原因 |
|---|---|
| `Expert_Philanthropy` | 需真实捐款（服务端领奖时校验捐赠回执，已实测无法绕过） |

### 实现原理（简述）

任务计分走 `/v2/report` 行为上报，但**不同任务认不同客户端指纹**：CLI 指纹（`www.codebuddy.cn`）、桌面指纹（`copilot.tencent.com` + `desktopUAFor` 给出的桌面 UA + `workbuddy-desktop` 事件族）、web 指纹（`www.workbuddy.cn` + Node/axios 形态的 `/v2/report`）。网关为每类任务构造对应指纹的判据事件链（`internal/upstream/desktop.go`）；专家类任务额外要求真实专家 id 与真实对话回执（`internal/upstream/streak.go` 之外的 expert 序列）。上报 200 ≠ 计分——面板在执行后轮询任务进度，达标即自动调用 Web 域领奖接口。

> 两个曾经写错、已按真机抓包纠正的点：桌面 UA 不是硬编码版本号，而是与业务链路同源的 `desktopUAFor`（版本由配置驱动，CN 5.9.3 / GL 5.9.4）；**`/v2/report` 路径**不带 `x-client-platform`、`Origin`、`Referer` 或任何 `sec-ch-*`——官方 report 是纯 Node 形态的 11 个头，往这里补 Client Hints 反而是破功。别把两处混为一谈：`/console/account`（`profile.go`）与任务领奖端点（`tasks.go`）**确实**带 `x-client-platform: web`，那是另一条链，删了必坏。

> ⚠️ 行为事件按天幂等：重复点「一键完成」不会重复扣资源，已达标的任务自动跳过。
> 补充（issue #174）：**白名单外**的任务只要进度已达标也会入队，以「只领奖」形态执行（不跑任何动作、不消耗对话配额）——避免任务达标但没人 claim、积分卡在 2000/2100/2300 上不去。

### 🧭 任务中心（面板新视图）

「任务中心」视图把散落的任务能力收拢成一处：

- **全账号任务扫描**：一键拉取每个账号未完成且可自动化的成长任务（含小程序口径），列表一目了然
- **执行队列**：把待办按账号排队执行——账号内串行（与单任务/一键完成共用互斥锁），账号间可选并发（1-3）；执行进度实时更新到每个条目。**同一队列也可按排程每日自动跑**（`schedule.growth_hours`，默认 11 点；排程轮次账号间固定串行）
- **日志分频道**：运行日志按「任务 / 对话 / 系统」三个频道筛选——对话流量再大，任务结果也不会被冲掉；日志条目带频道徽标与时间

任务中心扫描会合并小程序口径待办（`Sequential_Tasks_*`，默认列表不可见）。accept 带登记回读：上游有过 200 但没落账的情况，未生效会再试一次。

### 连登兑换与抽奖（自动）

成长中心连登档位（连续登录 7/14/28 天）兑换后发放积分 / 能量 / 补签卡 / **抽奖次数**，抽奖次数只能从兑换获得。网关把它挂在每日签到排程末尾自动跑闭环（见[定时任务](#定时任务)）：档位解锁当天自动兑换、有抽奖次数自动抽完，全程无需人工盯。

## 🆚 与上游的差异

本分支相对 [上游 master](https://github.com/Sliverkiss/workbuddy2api) 的增量（均已在真实多账号环境验证）：

### 新增

| 能力 | 说明 |
|---|---|
| **Web 管理面板** | `internal/panel`，前端 go:embed 单文件进二进制，零外部依赖。账号池可视化（健康色条 / 积分量条 / 冷却倒计时）、积分到期分布、单号运维、批量任务、日志查看、明暗主题 |
| **请求指标与脱敏日志** | 面板展示完成成功率 / HTTP 成功率 / 平均耗时 / 最近请求，响应带 `X-Request-Id`；JSONL 只归档请求元数据，不写提示词、响应正文或凭证。请求记录表带**调用来源**（客户端 IP / User-Agent，按 `logging.request_client_info` 可关），支持按 IP / UA / 模型 / 账号 / 请求 ID 与结果筛选 |
| **浏览器内 OAuth 添加账号** | 面板「添加账号」按钮完成设备授权 → 凭证落盘 → **热加载进池（免重启）**，替代命令行 `login.sh` 流程 |
| **在线配置编辑（热生效）** | 面板直接改 `config.json`：API 密钥 / `soft_rate` / 脱敏开关 / 池参数 / 任务排程**立即生效**；装配期字段（listen 等）保存后提示需重启。写入采用深合并 + 原子替换，保留未知键 |
| **积分任务体系** | 任务列表 / 接受 / 领取接口 + 面板弹窗；「一键完成」覆盖 **25 个任务**（对话 / 领养 / 桌面行为链 / 模板 / 灵感案例 / 专家召唤 / 技能尝鲜 / 主题 / 资料库等），推进进度、等待异步计分落定后**自动领奖**，纯 API 零客户端依赖 |
| **模型繁忙度（面板）** | 「模型与档位」列出官方 `/v2/chat/busy` 的实时繁忙度（idle/normal/busy/congested/unavailable），只在 `繁忙 / 拥堵 / 不可用` 时挂色标——它解释了 chat 侧偶发的 429 / 6021 queue.full。**只读观测**：不参与选号；服务端偶发对整批回 `unknown`（实测同批重查可在 idle/unknown 间跳），故取不到就不标；同页每行另有**网关档位**下拉（首项＝模型支持的最高档，其余为具体档与 `off`；写入 `thinking.model_defaults` 精确键、保存即热生效，策略关闭时整体禁用并显示生效档）与**下游可见**开关（与 `models.hidden` 同一份黑名单：关掉的模型从 `/v1/models` 隐藏、点名调用被拒，行尾置灰并排到列表末尾） |
| **联网检索（官方 agenttool）** | `POST /v1/search` 透传官方 `{base}/agenttool/v1/search`（`{query,max_results}` → `{results[].title/url/snippet/date,total_results}`，查询免费；对 DSH（DeepSeek Harness，按 UA 含 `deepseek-harness` 识别）额外启用 DeepSeek Search API 兼容：接受 `count` 作为条数，`date` 缺失时补 `null`、`title`/`snippet` 缺失时补空串；其余客户端保持上游原文逐字节透传）；同一 base 还提供 `POST /v1/fetch`（`{"url":"https://…"}` → `{"content":"<markdown>"}`，透传官方 `agenttool/v1/webfetch`）；加 `"deep":true` 则改走官方**综述式**检索 `agentic_search`（服务端多轮检索 + 抓页 + 带引用综述，回 `done` 帧原文：`synthesis.content` + `results` 整页正文 + `statuses` + `partial` 降级标记）——实测单次 18~66s、prompt ~48K tokens，故默认走 snippet 快路径（~1s）。另在 `/v1/messages` 与 `/v1/responses` 上落地服务端联网工具：客户端（Claude Code / Codex）只声明 `{"type":"web_search_20250305","name":"web_search"}`（或 Responses 的托管 `{"type":"web_search"}`），网关侧先让模型自己写检索词（探测）、执行检索、把结果注入上下文，再走既有流式管道出答案。Anthropic 侧按协议回报 `server_tool_use` + `web_search_tool_result` 块（含来源 URL，且**不**降级成客户端可执行的 `tool_use`）；没声明该工具的请求零额外开销；检索失败如实告知模型（不静默当成「网上没有」），联网不可用则降级为普通对话 |
| **成长任务队列每日排程** | `schedule.growth_hours`（默认 `[11]`）+ `schedule.growth_enabled`：面板任务中心的执行队列按排程每日自动跑一遍（扫描待办 → accept → 动作 → 达标领奖），与手动入口共用同一队列与账号锁；`POST /admin/tasks/growth/run` 可手动补跑 |
| **国际版每日活跃（网页通道）** | 国际版每日 30/50 积分只认「**跑完的网页 agent 会话**」——桌面身分 chat/completions 与 `/v2/report` 只点亮连登，积分不动（issue #111 实测）。排程的国际版活跃腿因此完整复刻网页端链路：`POST /console/as/conversations/` 建会话 → `GET .../{id}/session` 取沙箱 `link`/`token` → ACP over SSE（`initialize` → `session/load` → `session/set_model` → `session/prompt`）→ 轮询 `data.status` 到 `completed` → `stop`（失败再 `delete`）。三个必须照抄客户端的细节：ACP 鉴权用**沙箱 token**（账号 token 一律 401 `12801`）、走浏览器身份 UA/Origin、跑完必须收尾（单号活跃会话上限 10 条，漏收尾攒满就再也建不出会话）。模型钉 `deepseek-v4.1-flash`（国际版 x0.00，打卡不扣积分）；当日成功一次后不再重复建会话。国内版零调用 |
| **首启自动生成配置** | 目录下无 `config.json` 时自动生成推荐配置（含 `crypto/rand` 随机 `api_key`），双击即开 |
| **粘性会话内容回退** | 客户端不发 `conversation_id` 时，用 `system + 首条 user` 哈希派生会话键（`d-` 前缀），通用 OpenAI 客户端也能享受粘性 |
| **余额后台刷新** | `schedule.balance_refresh_minutes`（默认 5）周期查余额并更新池，积分冷却（余额耗尽）账号余额恢复自动解冻 |
| **模型降级路由（6004）** | `pool.model_fallback`：`{"deepseek-v4.1-flash":"deepseek-v4-flash"}`。上游 `6004`「该模型使用量超限」是**模型级**限流（实测见过 8 小时窗口），此时账号健康、等价模型可用——配了表就**同账号改道等价模型**继续服务。仅 `6004` 触发（`11102`/`11101`/普通 429/5xx 不降级）；冷却仍记在原模型名下（记成等价模型会把好的也冷却，降级白做）；不写持久状态，到期自动回原模型；全池该模型冷却（选号返回空）时也先试降级再报 503。缺省空 = 不降级。面板在限流提示里标注「⤳ 已路由至 <等价模型>」，否则用户看到「v4.1 限流」会以为服务断了。另附 `POST /panel/api/accounts/{uid}/model_toggle`：**手动**关闭某 (账号, 模型) 组合（永久语义、仅手动解，且优先于一切自动冷却——否则「我关了它，一次自动恢复又开了」） |
| **余额趋势视图** | `internal/balance` 每 30 分钟在余额刷新之后顺手记一个水位（不额外打上游，按账号节流），落盘 `data/balance_history.json`（近 30 天原始点 + 365 天日粒度，点数超 20 万自动折叠）。面板「余额趋势」：多账号水位折线（cn/global 分域，两域积分不同源、不做跨域总计）、每日变化柱状图（不完整日淡化）、按账号明细（当前 / 最低 / 最高 / 净变化 / 今日消耗 / 今日到账 / 本月消耗 / 本月到账 / 采样点数）。**消耗与到账是下界估计**：按相邻采样差值分正负累加，30 分钟间隔远粗于真实事件密度（实测 6 小时 2127 次请求 vs 14 个采样点），同一窗口内的消耗与到账会被合并成净差，UI 显式标注、不做成精确值的样子。折叠点随点保存当日首值 / 消耗 / 到账，否则历史日这三项会静默归零（不报错、无空值，只是给出错的 0） |
| **补领清扫** | `schedule.task_sweep_enabled`（默认开）/ `task_sweep_minutes`（默认 15）：周期 accept 待接受的、claim 已达标未领的（不跑行为链路），兜住新号自动跑当时未达标与 `autoActions` 白名单外的任务 |
| **保留积分** | `pool.reserve_credits`（缺省 10）设最低余额：余额**已知**且低于该值的账号停止接单，面板账号行显示「保留积分」标记，避免余额被用到 0 触发上游提醒短信；仍留在池中照常签到/猫猫旅行，充值后自动恢复。`0` = 关闭 |
| **用量页积分历史** | 用量视图新增「积分与 token 按小时」表：每小时列出请求数 / prompt / completion / 合计 token / **扣费积分** / 兑换率（1 积分换多少 token，倒序，最近一小时在最上面），卡片另给窗口内积分合计与兑换率。积分取上游末帧 `usage.credit`，**未观测到的请求不计**（「没测到」≠「没花钱」）；随小时桶一起折叠进日桶，保留 90 天；另给窗口内**总积分**卡片（`usage.credit` 求和，无样本时显示 —） |
| **模型能力透出** | `/v1/models` 附带 `context_length` / `max_output_tokens` / 工具·视觉·思考能力标记 / 积分倍率等上游真实字段，并声明 `reasoning_supported_efforts` / `reasoning_default_effort`：默认档按 `thinking.*` 策略写成网关**实际生效档**（策略关闭时忠于上游），`can_disable_thinking` 的模型在档位表里追加 `off`，目录只给固定档的模型声明为单档 |
| **models.dev 形状能力声明** | 上一条之外，`/v1/models` 每条再**追加**一组 models.dev 词表字段，给只认这套词的客户端（magpie 等）读：`modalities`（`input` 按 `supports_images` 给 `["text"]` 或 `["text","image"]`；`output` 对话模型恒 `["text"]`、出图模型 `["image"]`）、`limit`（`context`/`output` 直接搬同一轮的 `context_length`/`max_output_tokens`，缺哪个省哪个，**不拿 1M/131072 兜底**）、`tool_call`、`reasoning`、`reasoning_options`（档位取 `reasoning_supported_efforts` 去掉 `off`；有 `off` 或 `can_disable_thinking` 时补 `{"type":"toggle"}`）。**纯追加**：既有键一个字节都不改（`cn:` 前缀、`object`、`created`、`owned_by` 照旧），无数据源的字段（`cost`/`attachment`/`structured_output`/`knowledge`/`release_date`）一律不写——客户端按「字段在不在」判能力，编造比缺失更糟。另加 `supported_reasoning_levels`（`[{"effort":"low"},…]`）：**这是本组里唯一不属于 models.dev 词表的键**，因为 magpie 解析厂商列表时只认它（`internal/catalog/live.go:704` 的 `liveModel.Levels`），「关闭思考」按其 `EffortRank` 写成 `none` 而不是 `off`，出图与无档位模型整键不写。不认这些键的客户端按 JSON 惯例忽略。面板自己的 `/panel/api/models` 不受影响，仍是网关形状 |
| **安全加固** | 常量时间密钥比较（`internal/httpauth`）、CSP 与安全响应头、UID 白名单防路径穿越、前端属性转义修复 |
| **领养前置修复** | 上游 `travelAdopt` 缺 report 前置导致领养恒失败于 `first_buddy task not completed yet`；本分支修正后实测 +300 到账（3/3 账号） |
| **原生工具调用标记还原** | 上游（DeepSeek 系）的工具调用原生语法是带全角竖线的标记文本，**仅在请求声明了 `tools` 时**才被上游解析成结构化 `tool_calls`。实测 `tools` 声明在中转环节（Responses → chat/completions 等）丢失是常态，此时模型仍想调工具，标记就以纯文本落进 `content`——Codex 这类只执行结构化 `function_call` 的客户端会把它当普通正文写进会话历史，回合直接结束、工具一次都没跑。网关在出站响应里识别并还原成 `tool_calls`（流式与非流式都覆盖，见下文） |
| **零输出守卫** | 上游「收下请求却零输出」（HTTP 200 + `finish_reason=length` + 正文/思考/工具全空，典型成因：输入越过模型**真实**窗口，声明的 `context_length` 被虚报）此前被当成一次成功：客户端拿到空正文，网关零日志零计数，下一次照旧挑同一个号撞同一面墙。现在流式在**写出第一个字节之前**判出来（首帧否决窗口延长到首个**可见输出**帧，role 帧不提交响应头）→ 换号重发一次，客户端完全无痕；非流式在聚合体上判定，同样重发一次。全池都撞则如实回 `502 zero_output_length` + 可行动 hint（缩短输入 / 调大 `max_tokens`），**不罚账号、不冷却**（这是请求形态问题，不是账号健康问题） |
| **整体不可用模型提升** | 11102「该后端无此模型」原是逐账号负缓存：模型整体下架时每个号都要白撞一次，期间 `/v1/models` 照旧列出它。现在同一 realm 有 **≥2 个不同账号**都被上游明确回 11102 → 记为整体不可用（**只凭上游阳性证据，绝不从「远端目录里没有」反推**；11158「号型不允许」是账号权限差异，不参与提升）→ `/v1/models` 不再列出、点名调用在打上游之前 `400 model_unavailable` 指去换模型；面板「整体不可用模型」单列一张表（与「模型锁池」分清：那是限流等就有，这是等不到），给命中账号数 / 复检时刻 / 上游原话 + 逐条与一键恢复（恢复同时清掉各号的 11102 负缓存，否则「恢复了但请求永远挑不到号」）。24h 自动复检，随 `state.json` 持久化 |
| **账号备份 / 迁移（未实现，待做设计）** | 面板「添加账号 → 备份 / 迁移」：导出为自包含快照（凭据原文 + 备注 + realm + 企业 id + device token），支持**逐个勾选**子集；可选口令加密，**加密在浏览器侧完成**（PBKDF2-SHA256 200k + AES-GCM），口令与明文都不经过网关落盘与日志。导入两种模式取**白名单**（未知值一律 400，不静默回落）：`merge` **只增加**——池中已有的 uid 一个字节都不写（从别人的老备份导入不会用陈旧 token 打断本机活跃号）；`replace` 覆盖/新增，但**快照外的账号一律保留**（刻意不提供「整体替换」，删凭证不可逆）。判重三层：池内 uid → 文件内重复 uid → 规范化（trim + 小写）后重复。备份**不含**本机配置、模型黑名单、保留积分阈值、排程与告警设置，导出文件里用 `note` 写清这条。**当前 main 上没有这个实现**（全树无 PBKDF2 / AES-GCM / `crypto.subtle`，账号导入只有 cockpit 格式一条路），别按这行去面板里找按钮；眼下迁移账号用 `auths/` + `data/` 目录搬迁（见「账号管理」与「凭据管理（auths）」两节） |
| **嵌入端点（实验）** | `POST /v1/embeddings`：OpenAI 兼容嵌入，默认关闭（`features.embeddings_enabled`）。上游 `POST /v2/embeddings` 与对话是两套注册表，实测只有 `bge-m3`（1024 维）；`11351` 映射成 404 `model_not_found`，回程补 `object`/`index`，条数不符 502。关闭时回 403 `embeddings_disabled` |
| **补签卡每轮上限** | `schedule.makeup_cards_per_round`（默认 `0` = 不限，保持既有行为）。补签会改账号历史，保守部署设 `1` 即每轮只补一张，剩余漏签下一轮继续；面板保存后热生效 |

