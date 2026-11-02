#!/usr/bin/env python3
"""AutoBot：把参考仓库的实现读成「本仓可以借鉴什么」，写成 PR 供人审。

两个阶段，可分开跑也可一次跑完：
  analyze    每个参考仓库 clone 到 /tmp/up/<slug>，agent 读代码写差异报告
  implement  按报告自评的值得度排序，取前 N 条让 agent 写 Go 实现并开 PR

硬约束：只推 autobot/* 分支，绝不 push main；提交身份是 GitHub Actions bot。
"""
import argparse
import hashlib
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPORT = os.path.join(ROOT, "autobot-report.md")
REPORT_DIR = "/tmp/autobot-reports"
UP_DIR = "/tmp/up"
# 只在自产提交上用 -c 传身份；写 git config 会污染本机手写提交（真出过事故）
BOT = ("-c", "user.name=github-actions[bot]",
       "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com")


def run(*args, check=True, timeout=None):
    p = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, timeout=timeout)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} 失败: {p.stderr.strip()[:400]}")
    return p.stdout.strip() if check else (p.returncode, (p.stdout + p.stderr).strip())


def say(lines, text=""):
    lines.append(text)
    print(text, flush=True)


def agent(task, timeout, expect):
    """跑一次 Hermes（ACP 模式），思考/回复/工具调用全部实时回显。

    shortcut: 不用 `hermes -z`——它设计上只打印最终答案（No tool previews），
    看不到逐轮推理。ACP 提供 agent_thought_chunk 等事件流，日志即完整思考链。

    429 是限流的常态，退避重试三次再认输。用 Popen.wait 而不是
    subprocess.run(capture_output)：后者要读完管道才算完，工具子进程继承管道后
    kill 主进程也不返回（实测挂到 job 超时 48 分钟）。

    expect 是这一轮必须写出的文件：**有产物就是成功**，不看日志内容。日志里出现
    「429」完全可能是 agent 在读或写的文本（本仓自己就在处理 429），拿子串判定会
    把已经跑完的整轮丢掉重放，重试次数一路涨到 5 却每轮都有产出（实测）。
    """
    # CI 里没人点审批弹窗：config 的 approvals.mode=off + HERMES_YOLO_MODE，
    # ACP 客户端的 session/request_permission 也一律允许
    cmd = [sys.executable, os.path.join(ROOT, "scripts", "acp_run.py"), task]
    tail = ""
    for attempt in range(5):
        if os.path.isfile(expect):
            os.remove(expect)   # 上一轮残留不算本轮产出
        log_f = "/tmp/autobot-agent.log"
        pipe = " ".join(shlex.quote(c) for c in cmd) + " 2>&1 | tee " + shlex.quote(log_f)
        p = subprocess.Popen(
            ["bash", "-c", pipe], cwd=ROOT, stdin=subprocess.DEVNULL,
            start_new_session=True)
        try:
            p.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(p.pid, signal.SIGKILL)   # 连它拉起的工具子进程一起杀
            except ProcessLookupError:
                pass
            p.wait(30)
        out = open(log_f, encoding="utf-8", errors="replace").read()
        if not out.strip():
            print("--- agent 无任何输出 ---", flush=True)
        code = 124 if (p.returncode is None or p.returncode < 0) else p.returncode
        keep = (out.strip().splitlines() or [""])[-3:]
        tail = " / ".join(keep)[:300]
        # 只看日志不看退出码：agent 吞掉 429 之后仍以 0 退出，按 code==0 放行
        # 会把这轮当成「跑完了但没产出」（真踩过，17 片里 1 片这么挂的）。
        if os.path.isfile(expect) or "429" not in out:
            return code, tail
        # 免费档是「允许突发 30、持续 10 RPM」：agent 一阵工具调用就会打满，
        # 退避要够长才能等到窗口滑过去，30→300s 封顶。
        wait = min(30 * 2 ** attempt, 300)
        print(f"  429 限流，{wait}s 后重试（{attempt + 1}/5）", flush=True)
        time.sleep(wait)
    return code, tail


def up_clone(src):
    """上游仓完整 clone 到 /tmp/up/<slug>；已存在就浅拉一次更新。"""
    d = os.path.join(UP_DIR, src.replace("/", "__"))
    if os.path.isdir(os.path.join(d, ".git")):
        run("git", "-C", d, "fetch", "-q", "--depth", "200", "origin", check=False, timeout=600)
        return d
    os.makedirs(UP_DIR, exist_ok=True)
    code, _ = run("git", "clone", "-q", "--depth", "200",
                  f"https://github.com/{src}.git", d, check=False, timeout=600)
    return d if code == 0 else ""


def upstream_brief(up):
    """上游清单：文件表 + 最近提交 + README 摘要。

    shortcut: 不让 agent 自己在仓里翻。deepseek 走网关每轮往返几十秒，
    让它探索 100+ 文件的仓 25 分钟也跑不完（实测三次全超时）。
    """
    files = run("git", "-C", up, "ls-files", check=False)[1].splitlines()[:200]
    logs = run("git", "-C", up, "log", "--oneline", "-40", "--no-merges", check=False)[1]
    readme = ""
    for n in ("README.md", "readme.md", "README.zh-CN.md", "README_EN.md"):
        f = os.path.join(up, n)
        if os.path.isfile(f):
            readme = open(f, encoding="utf-8", errors="replace").read()[:2500]
            break
    return files, logs, readme


def report_path(src):
    return os.path.join(REPORT_DIR, src.replace("/", "__") + ".md")


def report_score(text):
    """报告自评的值得度；没写就按 0（宁可不落地）。"""
    m = re.search(r"值得度\s*[:：]\s*([0-3])", text)
    return int(m.group(1)) if m else 0


def policy_hit(files, text, pol):
    """policy.never 按路径、policy.off_limits 按内容，命中返回一句说明，否则 ""。

    这两张表写在 AutoBot.yml 里，但在 PR #141 之前没有任何代码读它——「硬禁项」没人拦
    就等于没有（当时脚本里还留着一个没人用的 FORBIDDEN 常量）。所以闸门必须落在代码里。
    """
    for n in (pol.get("never") or []):
        if "/" not in n and "." not in n:
            continue            # push_main / merge 这类是动作，不是路径
        base = n.rstrip("*").rstrip("/")
        for f in files:
            if f == base or f.startswith(base + "/"):
                return f"policy.never 命中 {n}（{f}）"
    low = (text or "").lower()
    for r in (pol.get("off_limits") or []):
        for k in (r.get("match") or []):
            if str(k).lower() in low:
                return f"不做清单「{r.get('name')}」命中 {k}"
    return ""


def ban_brief(pol):
    """把 policy 的两张表变成提示词里的几句话。

    policy.never 原来只在提交前扫一次：agent 花 20 分钟写完改动、最后一步被驳回，
    上一轮两个候选就是这么白干的（都改了 config.example.json）。写之前先说清楚。
    """
    paths = [n for n in (pol.get("never") or []) if "/" in n or "." in n]
    rules = pol.get("off_limits") or []
    out = []
    if paths:
        out.append("这些路径一个字节都不要改，提交前会被自动驳回：" + "、".join(paths) + "。")
    if rules:
        out.append("本仓明确不做的能力（值得度一律 0，也不要写进「适配」）：\n"
                   + "\n".join(f"- {r.get('name')}：{r.get('why', '')}" for r in rules))
    return "\n".join(out) + "\n" if out else ""


def own_brief():
    """本仓 Go 文件清单：让 agent 判「本仓现状」时有据可依，而不是靠猜。

    shortcut: 只给路径不给内容，读不读由 agent 自己定（预算内最多 2 个）。
    """
    return run("git", "ls-files", "*.go", check=False)[1].splitlines()[:250]


def analyze(src, lines, pol, hist):
    """clone → agent 读代码 → 差异报告。返回报告路径（失败为 ""）。"""
    up = up_clone(src)
    if not up:
        say(lines, f"{src}: clone 失败")
        return ""
    os.makedirs(REPORT_DIR, exist_ok=True)
    rp = report_path(src)
    files, logs, readme = upstream_brief(up)
    # 材料写文件、prompt 只留指令：十几 KB 的 prompt 会让 agent 静默超时
    up_f = "/tmp/autobot-upstream.md"
    with open(up_f, "w", encoding="utf-8") as f:
        f.write(f"# 上游仓库 {src}（clone 在 {up}，只读参考）\n\n"
                "## 文件清单（最多 6 个，挑最能说明差异的）\n" + "\n".join(files) + "\n\n"
                "## 最近提交\n" + logs + "\n")
        if readme:
            f.write("\n## README 摘要\n" + readme + "\n")
        f.write("\n## 本仓 Go 文件清单（写「差异」时对照）\n" + "\n".join(own_brief()) + "\n")
        f.write(hist_brief(hist))
    ban = ban_brief(pol)
    task = (
        f"先读 {up_f}。本仓是 WorkBuddy 的 Go 网关（当前目录）。\n"
        "任务：把这个上游仓库与本仓**逐功能一一对照**——逐个功能点判断本仓有没有、"
        "差在哪、要不要借鉴。\n"
        "20 分钟内写出报告，只读，不要修改本仓任何文件。\n"
        f"报告写进 {rp}，结构固定，不要写散文：\n"
        "第 1 行：值得度: 0-3（本轮最高分；0=无可借鉴，3=本仓明确缺这个能力）\n"
        "然后一张对照表，一个功能一行：\n"
        "| 功能 | 上游（文件:函数） | 本仓（文件:函数 或 缺失） | 差距 | 值得度 |\n"
        "|---|---|---|---|---|\n"
        "表格之后两小节，各不超过 5 行：\n"
        "适配: 要落地的那几项，改本仓哪个 文件:函数、怎么改\n"
        "影响: 行为/配置/性能风险\n"
        "「本仓」列必须点名真实 文件:函数；没有就写「缺失」，没核实就写「未核实」。"
        "本仓已有等价实现的项，值得度给 0-1，不要报 3。\n"
        "材料里「已经提过、被人关掉的提案」是本仓历史：同样的方向别再提，"
        "重复得先说清和哪一号 PR 不同，说不清就给 0。\n"
        + ban
    )
    code, out = agent(task, int(os.environ.get("AGENT_ANALYZE_TIMEOUT") or 1800), rp)
    if not os.path.isfile(rp):
        say(lines, f"{src}: agent 未产出报告（退出码 {code}）｜{out}")
        return ""
    say(lines, f"{src}: 分析完成 值得度={report_score(open(rp, encoding='utf-8').read())}")
    return rp


def report_head(text, n=60):
    """报告一句话摘要：优先取对照表第一条真有差距的行，其次「功能:」行。"""
    for line in text.splitlines():
        t = line.strip()
        if t.startswith("|") and "---" not in t:
            cells = [c.strip() for c in t.strip("|").split("|")]
            if len(cells) >= 4 and cells[0] not in ("功能", "") and cells[3] not in ("", "无"):
                return " / ".join(cells[:4])[:n]
        if t.startswith(("功能:", "功能：")):
            return t[3:].strip()[:n]
    lines = [x for x in text.strip().splitlines() if x.strip()]
    return lines[0][:n] if lines else ""


def adapt_brief(idea):
    """从报告里只取「功能」和「适配」两段。

    shortcut: 整篇报告（含 200 行文件表）喂进去后模型会重新理解全仓，
    60 分钟改不完一处（实测超时两次）。
    """
    want = []
    for m in re.finditer(r"^#{2,4}\s*(.+)$", idea, re.M):
        title = m.group(1)
        start = m.end()
        nxt = idea.find("\n#", start)
        body = idea[start:nxt if nxt > 0 else len(idea)]
        if "适配" in title or "功能" in title or "改动" in title:
            want.append(f"{title}\n{body.strip()[:1800]}")
    return "\n\n".join(want)[:4000] or idea[:2500]


def humanize_hint():
    """PR 正文的「说人话」技能路径。

    技能本身 36 KB，塞进 prompt 会让 agent 静默超时（实测十几 KB 的 prompt 30 分钟零输出），
    所以只给路径，让它自己读。CI 由 workflow 把 .github/autobot-skills 拷进 $HERMES_HOME/skills。
    """
    home = os.environ.get("HERMES_HOME") or os.path.expanduser("~/.hermes")
    return (f"写正文前先读 {os.path.join(home, 'skills', 'humanizer', 'SKILL.md')}"
            "（怎么把话说成人话、哪些 AI 味要删）；文件不在就忽略这句。\n")


def implement(src, idea, lines, pol):
    """两次短调用：先出改动方案，再出 unified diff。

    shortcut: 一次调用干完「读代码→判断→写实现→写测试→接线」时 7 轮全败
    （只写计划/超时/不接线/抄垃圾/判定无需改动），单次失败要 90 分钟；
    拆成 10 分钟 + 20 分钟两段，失败便宜，方案段还能给 diff 段当输入。

    材料一律写文件、prompt 只留几句：十几 KB 的 prompt 让 agent 30 分钟零输出，
    而同样环境下的短 prompt 4 分钟就能跑完（自检）。
    """
    up = up_clone(src)
    ban = ban_brief(pol)
    brief_f = "/tmp/autobot-brief.md"
    with open(brief_f, "w", encoding="utf-8") as f:
        f.write(f"# 上游仓库\n{src} clone 在 {up}（只读参考）\n\n"
                f"# 分析结论\n{adapt_brief(idea)}\n\n"
                f"# 硬线\n{ban}\n"
                "# 本仓 Go 文件清单（不要猜路径，就照这个读）\n"
                + "\n".join(own_brief()) + "\n")
    plan_f = os.path.join(ROOT, "autobot-plan.md")
    patch = os.path.join(ROOT, "autobot.patch")
    for f in (plan_f, patch):
        if os.path.isfile(f):
            os.remove(f)

    # 第一段：只出方案，不许写其它文件
    code, out = agent(
        f"先读 {brief_f}。本仓是 Go 项目（当前目录）。\n"
        f"把改动方案写进 {plan_f}：要改哪个 文件:函数、改成什么、测试加在哪，"
        "每处给出准确位置与代码。\n"
        "不要写实现文件、不要写 diff、不要跑 go build。直接写文件，不要回说明。\n"
        + ban,
        int(os.environ.get("AGENT_PLAN_TIMEOUT") or 1800), plan_f)
    if not os.path.isfile(plan_f):
        say(lines, f"{src}: 改动方案未产出（退出码 {code}，prompt 材料已落文件）｜{out}")
        return False, ""
    plan_in = "/tmp/autobot-plan-input.md"
    with open(plan_in, "w", encoding="utf-8") as f:
        f.write(open(plan_f, encoding="utf-8").read()[:12000])
    os.remove(plan_f)

    # 第二段：按方案出 diff
    code, out = agent(
        f"读 {plan_in}（改动方案）和 {brief_f}（本仓文件清单）。本仓是 Go 项目（当前目录）。\n"
        f"把它写成标准 unified diff 存进 {patch}，不要动工作区任何文件：\n"
        "--- a/internal/foo.go\n+++ b/internal/foo.go\n@@ -10,6 +10,8 @@\n 上下文行\n-旧行\n+新行\n"
        "只能修改已存在的文件；测试加进已存在的 _test.go。直接写 patch，不要回说明。\n"
        f"写完自己跑 `git apply --check {patch}` 确认能打上（--check 不改工作区），"
        "打不上就照文件真实内容重写，别交一个打不上的 patch。\n"
        f"写完再写 autobot-pr.md，第一段用人话，四行，然后空一行，再接五行细节：\n"
        "人话四行——一句话说清这 PR 干了什么 / 合了之后日常用法有什么变化（没变化就直说）/ 会不会弄坏现在能用的东西（会就说是哪一处） / 要不要我拍板（比如映射是猜的就得人定）\n"
        "细节五行：问题 / 改动 / 位置 / 优缺点 / 影响。\n"
        + humanize_hint() + ban,
        int(os.environ.get("AGENT_IMPLEMENT_TIMEOUT") or 1800), patch)
    if not os.path.isfile(patch):
        say(lines, f"{src}: 实现未产出（退出码 {code}）｜{out}")
        return False, ""
    ok, why = apply_patch(patch)
    if ok:
        os.remove(patch)
    else:
        # 留证据：patch 是模型手写的，删掉就查不出是行号漂了、还是内容被剥掉了
        # （上一轮 Buddy2api 那条只留了一句 payload.go:221，无从下手）。
        raw = open(patch, encoding="utf-8", errors="replace").read()
        print("--- 应用失败的 patch 原文 ---\n" + raw[:12000], flush=True)
        os.remove(patch)
        say(lines, f"{src}: patch 应用失败｜{why}")
        return False, ""
    if not run("git", "status", "--porcelain", "-uall", check=False)[1].strip():
        say(lines, f"{src}: patch 应用后没有改动")
        return False, ""
    return True, read_summary()


def apply_patch(patch):
    """模型手写的 unified diff 行号常常不准：--recount 重算 hunk 头，-C1 放宽上下文。

    shortcut: 不做 fuzzy 合并，三次都失败就算这条候选没成（人看的报告还在）。
    """
    # 模型常把 diff 包在 ```diff 里或前面带说明，剥掉再喂 git apply
    raw = open(patch, encoding="utf-8").read()
    lines = [l for l in raw.splitlines() if not l.startswith("```")]
    start = next((i for i, l in enumerate(lines) if l.startswith(("--- ", "diff --git"))), 0)
    open(patch, "w", encoding="utf-8").write("\n".join(lines[start:]) + "\n")
    err = ""
    for extra in (["--recount", "-C1"], ["--recount", "-C1", "--3way"], ["--recount", "-C0"]):
        r = subprocess.run(["git", "apply", *extra, patch], cwd=ROOT,
                           capture_output=True, text=True)
        if r.returncode == 0:
            return True, ""
        err = (r.stderr or r.stdout).strip()[:300]
    return False, err


def read_summary():
    """agent 写的 PR 正文材料；读完即删，别让它混进提交。"""
    f = os.path.join(ROOT, "autobot-pr.md")
    if not os.path.isfile(f):
        return ""
    text = open(f, encoding="utf-8").read().strip()
    os.remove(f)
    return text


def finish(src, pol):
    """落地闸：外星文件、孤儿实现、build/vet/test 全绿才提交。返回 (ok, 说明)。"""
    status = run("git", "status", "--porcelain", "-uall", check=False)[1].strip()
    if not status:
        return False, "没有改动"
    tracked = set(run("git", "ls-tree", "-r", "--name-only", "HEAD").splitlines())
    changed = [l[3:] for l in status.splitlines()]
    # agent 常把上游的 .bat/.sh 一起抄进来。这些本来就不该进本仓，删掉继续，
    # 不要因为一条垃圾文件把整条实现丢掉（真丢过一整轮）。
    alien = [f for f in changed if f not in tracked and not f.endswith((".go", ".md"))]
    if alien:
        run("git", "reset", "-q", "HEAD", "--", *alien, check=False)
        for f in alien:
            if os.path.isfile(f):
                os.remove(f)
        changed = [f for f in changed if f not in alien]
        if not changed:
            return False, "清掉外星文件后没有改动"
    status = run("git", "status", "--porcelain", "-uall", check=False)[1].strip()
    if not status:
        return False, "清掉外星文件后没有改动"
    # 只新增文件、没接线到既有代码 = 孤儿实现：能编译过测试，但行为没变
    if all(f not in tracked for f in changed):
        return False, f"只新增 {changed[:5]}、未接线到既有文件"
    # 先 add 再按 --cached 取 diff：新增文件不在 git diff HEAD 里，而 PR #141 那种
    # 「整包加一张密钥表」正是新增文件，只看工作区 diff 会漏掉一半改动。
    run("git", "add", "-A", check=False)
    hit = policy_hit([l[3:] for l in status.splitlines()],
                     run("git", "diff", "--cached", check=False)[1], pol)
    if hit:
        return False, f"闸门拦下 —— {hit}"
    for cmd in (["go", "build", "./..."], ["go", "vet", "./..."], ["go", "test", "./..."]):
        code, out = run(*cmd, check=False)
        if code != 0:
            return False, f"{' '.join(cmd[1:])} 未通过：{out[-600:]}"
    run("git", "add", "-A")
    run("git", *BOT, "commit", "-q", "-m", f"autobot: 借鉴 {src} 的实现（agent 改写，非原样移植）")
    return True, ""


def past_attempts():
    """本仓已关掉（未被合并）的 AutoBot PR：改过哪些文件 + 人给的理由。

    AutoBot 原先没有任何记忆：yuechuya 那个 tool_call_sequence_broken 提了三次
    （#29/#31 被人关掉，#140 换个 sha 又来一遍 —— 分支名是报告内容的 sha1，
    报告重写一次就绕过去了）。这里一次 graphql 把历史取回来，分析时喂给 agent、
    落地时当冷却闸。

    shortcut: 只取近 100 条已关 PR（一次调用），不翻完整历史；真要更早的再说。
    """
    repo = os.environ.get("GITHUB_REPOSITORY") or ""
    if "/" not in repo:
        return []
    owner, name = repo.split("/", 1)
    q = ('query{repository(owner:%s,name:%s){pullRequests(states:CLOSED,first:100,'
         'orderBy:{field:CREATED_AT,direction:DESC})'
         '{nodes{number title url mergedAt closedAt headRefName '
         'files(first:80){nodes{path}} comments(last:3){nodes{body}}}}}}')
    code, out = run("gh", "api", "graphql", "-f",
                    "query=" + (q % (json.dumps(owner), json.dumps(name))),
                    check=False, timeout=180)
    if code != 0:
        print(f"历史读取失败（不当致命）：{out[:200]}", flush=True)
        return []
    try:
        nodes = json.loads(out)["data"]["repository"]["pullRequests"]["nodes"] or []
    except (ValueError, KeyError, TypeError):
        return []
    hist = []
    for n in nodes:
        title = n.get("title") or ""
        if n.get("mergedAt") or not title.startswith("autobot:"):
            continue
        # 关掉的理由取最后一条非空评论（人通常补在末尾）；自动关闭的那种也留着
        why = ""
        for c in (n.get("comments") or {}).get("nodes") or []:
            if (c.get("body") or "").strip():
                why = " ".join(c["body"].split())
        hist.append({
            "number": n["number"],
            "title": title,
            "closed": (n.get("closedAt") or "")[:10],
            "branch": n.get("headRefName") or "",
            "files": [f["path"] for f in (n.get("files") or {}).get("nodes") or []],
            "why": why[:400],
        })
    return hist


def hist_brief(hist):
    """历史提案写进 agent 的材料文件：跨仓库的重复方向只能靠它拦（同仓库有冷却闸）。"""
    if not hist:
        return ""
    rows = []
    for h in hist[:20]:
        rows.append(f"- #{h['number']}（{h['closed']} 关）{h['title']}\n"
                    f"  改过: {', '.join(h['files'][:12]) or '（未取到）'}\n"
                    f"  关掉的理由: {h['why'] or '（没写理由）'}")
    return ("\n## 已经提过、被人关掉的提案（同样的方向别再提）\n"
            "这些是本仓自己的历史。重复提之前必须在报告里写清楚「这次和 #N 的不同在哪」，"
            "写不出来就值得度 0。\n" + "\n".join(rows) + "\n")


COOLDOWN_DAYS = 14


def cooling_down(hist, repos, days=COOLDOWN_DAYS):
    """仓库 → 上次被关的日期（冷却期内）。人关一次 PR 就是一次明确否决。

    按仓库而不是按文件冷却：panel.go 这种主文件几乎所有改动都会碰，按文件拦会把
    正常的新功能一起拦掉；「这个仓库刚被否过」才是稳定的信号。

    仓库取自 PR 的分支名而不是标题：标题 38e4f3970 起改成人话第一行，不再含仓库名，
    按标题正则提取的 gate 会让之后每个 PR 都提取不到、冷却永不生效（#145 实测）。
    分支名是 branch_for 生成的，不随标题口径变。
    """
    cutoff = time.time() - days * 86400
    slugs = {slug_of(r): r for r in repos}
    cool = {}
    for h in hist:
        br = (h.get("branch") or "").rsplit("/", 1)[-1]  # 去掉 autobot/ 前缀
        src = next((r for s, r in slugs.items() if br.startswith(s + "-")), "")
        if not src:
            continue
        try:
            t = time.mktime(time.strptime(h["closed"], "%Y-%m-%d"))
        except ValueError:
            continue
        if t > cutoff and t > cool.get(src, (0, ""))[0]:
            cool[src] = (t, h["number"], h["closed"])
    return {k: (v[1], v[2]) for k, v in cool.items()}


def slug_of(src):
    """owner/repo → 分支名里的那段 slug（branch_for 与冷却判定共用）。"""
    return re.sub(r"[^a-z0-9-]+", "-", src.lower().replace("/", "-")).strip("-")


def branch_for(src, idea, pol):
    """报告内容相同 → 分支名相同 → 同一件事只推一次（land_max 才能落到新工作上）。

    shortcut: 去重靠内容 sha1，不比对语义；报告改一个字就是新分支，重复提案只能靠
    人关 PR 兜住，升级点是给报告加稳定的 feature key。
    """
    return f"{pol['branch_prefix']}{slug_of(src)}-{hashlib.sha1(idea.encode()).hexdigest()[:7]}"


def proposal_part(idea):
    """只扫报告里「适配:」之后的部分。

    对比表必然把上游的多密钥/配额那一行列出来（还常写着「明确不做」），扫全表等于让
    几乎每份报告都被禁区命中——实测 smart-open/TraeWorkAssistant 就是这么被误拦的。
    """
    i = idea.rfind("\n适配")
    return idea[i:] if i >= 0 else idea


def pick_candidates(reports, land_max, pol, exists, cool=None):
    """按值得度降序取前 land_max 条还没落地过的报告，返回 (picked, dups)。

    分支名是报告内容的 sha1：同名分支已存在 = 这份报告上一轮已经推过，必须剔掉再数
    名额，否则重复报告白占名额且 push 会因远端已存在而失败（实测连跑两次同一批报告，
    第一条就 RuntimeError 退出、后两条根本没跑）。
    """
    fresh, dups, banned = [], set(), {}
    for score, path in reports:
        if score < 2:
            continue
        src = os.path.basename(path)[:-3].replace("__", "/")
        idea = open(path, encoding="utf-8").read()
        hit = policy_hit([], proposal_part(idea), pol)
        if hit:
            banned[path] = hit
        elif (cool or {}).get(src):
            n, d = cool[src]
            banned[path] = f"同一仓库 #{n} 刚被关（{d}），{COOLDOWN_DAYS} 天冷却期内"
        elif exists(branch_for(src, idea, pol)):
            dups.add(path)
        else:
            fresh.append((score, path))
    return fresh[:land_max], dups, banned


def branch_exists(branch):
    return run("git", "ls-remote", "--exit-code", "--heads", "origin", branch,
               check=False)[0] == 0


def pr_title(summary, src):
    """标题用人话第一行。摘要为空才回落仓库名（旧标题等于没说干什么）。"""
    for line in (summary or "").splitlines():
        line = re.sub(r"[*#`]", "", re.sub(r"^[>\-\s]+", "", line)).strip()
        if line and not line.startswith(("问题", "改动", "位置", "优缺点", "影响")):
            return "autobot: " + line
    return f"autobot: 借鉴 {src} 的实现"


def land(src, rp, pol, args, lines):
    idea = open(rp, encoding="utf-8").read()
    branch = branch_for(src, idea, pol)
    # 上一条失败可能留下冲突 index，checkout -B 会报「resolve your index first」
    run("git", "reset", "-q", "--hard", "HEAD", check=False)
    run("git", "clean", "-qfd", check=False)
    run("git", "checkout", "-q", "-B", branch, pol["base"])

    ok, summary = implement(src, idea, lines, pol)
    if not ok:
        run("git", "checkout", "-q", pol["base"])
        return 0
    ok, why = finish(src, pol)
    if not ok:
        say(lines, f"{src}: 闸门拦下 —— {why}")
        run("git", "reset", "-q", "--hard", "HEAD")
        run("git", "clean", "-qfd", check=False)
        run("git", "checkout", "-q", pol["base"])
        return 0
    if args.dry_run:
        say(lines, f"{src}: dry-run，不推送（本地分支 {branch}）")
        run("git", "checkout", "-q", pol["base"])
        return 0

    # main 是生产环境：自动化只许推 autobot/* 分支
    assert branch.startswith("autobot/"), branch
    run("git", "push", "-q", "-u", "origin", branch)
    run("gh", "label", "create", pol["label"], "--repo", os.environ["GITHUB_REPOSITORY"],
        "--color", "ededed", "--description", "AutoBot 分析后提出的借鉴改动", check=False)
    body = (summary or f"分析报告：`{os.path.basename(rp)}`\n") + \
        "\n- `go build` / `go vet` / `go test ./...` 已通过；合并前请人工确认行为等价。\n"
    run("gh", "pr", "create", "--repo", os.environ["GITHUB_REPOSITORY"], "--base", pol["base"],
        "--head", branch, "--title", pr_title(summary, src)[:120],
        "--body", body, "--label", pol["label"])
    say(lines, f"已开 PR：{branch}")
    run("git", "checkout", "-q", pol["base"])
    return 1


def shard(seq, spec):
    """--shard i/n：按直线切片（matrix 并行用）。"""
    if not spec:
        return seq
    i, n = (int(x) for x in spec.split("/"))
    return [x for k, x in enumerate(seq) if k % n == i]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--phase", choices=["analyze", "implement", "all"], default="all")
    ap.add_argument("--shard", default="", help="i/n，只处理该片（matrix 并行用）")
    ap.add_argument("--repo", help="只处理这一个仓库")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    cfg = yaml.safe_load(open(os.path.join(ROOT, "AutoBot.yml"), encoding="utf-8"))
    up, pol = cfg["upstream"], cfg["policy"]
    repos = [args.repo] if args.repo else [r["repo"] if isinstance(r, dict) else r
                                          for r in up["repos"]]
    lines = ["# AutoBot 报告", ""]
    hist = past_attempts()
    if hist:
        say(lines, f"历史：{len(hist)} 个已关掉的 AutoBot PR（冷却中的仓库 {len(cooling_down(hist, repos))} 个）")

    if args.phase in ("analyze", "all"):
        todo = shard(repos, args.shard)
        say(lines, f"分析 {len(todo)} 个仓库（共 {len(repos)}，分片 {args.shard or '全部'}）")
        made = 0
        for src in todo:
            made += 1 if analyze(src, lines, pol, hist) else 0
        if todo and not made:
            # 一个报告都没产出（典型原因：模型额度 429）必须让 job 红，别假装成功
            say(lines, "本片没有产出任何报告")
            write_report(lines)
            return 1

    if args.phase in ("implement", "all"):
        reports = []
        if os.path.isdir(REPORT_DIR):
            for f in os.listdir(REPORT_DIR):
                p = os.path.join(REPORT_DIR, f)
                reports.append((report_score(open(p, encoding="utf-8").read()), p))
        reports.sort(reverse=True)
        if not reports and not args.dry_run:
            # 分析阶段全挂 / 报告没传过来时这里一份输入都没有，别把「什么都没干」报成绿
            say(lines, "没有任何输入报告，落地步骤无事可做")
            write_report(lines)
            return 1
        land_max = int(up.get("land_max") or 2)
        picked, dups, banned = pick_candidates(reports, land_max, pol, branch_exists,
                                              cooling_down(hist, repos))
        # 总决策表：每份报告的去向都留痕，人审时不用回头翻 68 个 job
        say(lines, f"## 决策表（{len(reports)} 份报告，land_max={land_max}）")
        say(lines, "| 仓库 | 值得度 | 摘要 | 决定 |")
        say(lines, "|---|---|---|---|")
        kept = {p for _s, p in picked}
        for sc, rp in reports:
            name = os.path.basename(rp)[:-3].replace("__", "/")
            if rp in kept:
                why = "落地"
            elif sc < 2:
                why = "值得度<2"
            elif rp in banned:
                why = f"禁区：{banned[rp]}"
            elif rp in dups:
                why = "同报告已落地（分支已存在）"
            else:
                why = "超出 land_max"
            say(lines, f"| {name} | {sc} | {report_head(open(rp, encoding='utf-8').read())} | {why} |")
        say(lines, "")
        say(lines, "落地候选：" + (", ".join(f"{os.path.basename(p)}({s})" for s, p in picked) or "无"))
        landed = sum(1 for _s, p in picked
                     if land(os.path.basename(p)[:-3].replace("__", "/"), p, pol, args, lines))
        if picked and not landed and not args.dry_run:
            # 有候选却一条都没落成（超时/闸门）必须红，别报成功
            say(lines, "有候选但没有任何一条落地")
            write_report(lines)
            return 1

    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())