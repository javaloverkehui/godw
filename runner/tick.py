#!/usr/bin/env python3
"""GoDW 无人值守任务执行器（状态机 + 互斥锁 + 多 worker 并行）。

单 worker（默认，最稳）：
  python3 runner/tick.py next --budget 45
  python3 runner/tick.py done --note "..."

多 worker（各自独立 worktree，按 scope 防冲突）：
  python3 runner/tick.py next --worker w1 --budget 45
  # 在打印出的 worktree 里写代码、commit
  python3 runner/tick.py done --worker w1 --note "..."

查看：
  python3 runner/tick.py status
  python3 runner/tick.py plan          # 依赖分层与可并行情况
  python3 runner/tick.py report        # 刷新 PROGRESS.md
"""

import argparse
import fcntl
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from pathlib import Path

RUNNER = Path(__file__).resolve().parent
ROOT = RUNNER.parent
STATE_FILE = RUNNER / "state.json"
TASKS_FILE = RUNNER / "tasks.json"
CURRENT_FILE = RUNNER / "CURRENT.md"
PROGRESS_FILE = RUNNER / "PROGRESS.md"
STATE_LOCK = RUNNER / ".state.lock"
MERGE_LOCK = RUNNER / ".merge.lock"
LOCK_DIR = RUNNER / ".locks"
WT_ROOT = ROOT.parent / ".godw-worktrees"

MAX_ATTEMPTS = 3
MAX_HANDOFFS = 3
TZ = timezone(timedelta(hours=8))

READING = {
    "E01": ["docs/03-核心抽象与接口.md 第 1–3 节", "docs/04-AI协作与验收规范.md 第 2 节"],
    "E02": ["docs/02-元数据模型.md 第 2 节", "docs/03-核心抽象与接口.md 第 2 节"],
    "E03": ["docs/03-核心抽象与接口.md 第 4 节", "docs/02-元数据模型.md 第 2.4 节"],
    "E04": ["docs/03-核心抽象与接口.md 第 5–6 节", "docs/04-AI协作与验收规范.md 第 6 节"],
    "E05": ["docs/02-元数据模型.md 第 3–4 节", "docs/03-核心抽象与接口.md 第 7 节"],
    "E06": ["docs/03-核心抽象与接口.md 第 4/6 节", "docs/02-元数据模型.md 第 2.7 节"],
    "E07": ["docs/03-核心抽象与接口.md 第 8 节", "docs/02-元数据模型.md 第 2.7 节"],
    "E08": ["docs/02-元数据模型.md 第 2 节", "docs/03-核心抽象与接口.md 第 8 节"],
    "E09": ["api/openapi.yaml（若已存在）", "docs/03-核心抽象与接口.md 第 8 节"],
    "E10": ["README.md 第 4 节", "docs/04-AI协作与验收规范.md 第 5 节"],
}


# ---------------- 基础 ----------------

def now():
    return datetime.now(TZ)


def iso(dt=None):
    return (dt or now()).isoformat(timespec="seconds")


def load_json(path, default):
    if not path.exists():
        return default
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def save_json(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=".tmp-", suffix=".json")
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


@contextmanager
def state_tx():
    """对 state.json 做读-改-写，进程间用 flock 串行化。"""
    STATE_LOCK.parent.mkdir(parents=True, exist_ok=True)
    with STATE_LOCK.open("w") as lk:
        fcntl.flock(lk, fcntl.LOCK_EX)
        st = load_json(STATE_FILE, None)
        tasks_meta = load_json(TASKS_FILE, None)
        if tasks_meta is None:
            raise SystemExit("缺少 runner/tasks.json，请先运行 python3 runner/build_tasks.py")
        if st is None:
            st = init_state(tasks_meta["tasks"])
        for t in tasks_meta["tasks"]:
            st["tasks"].setdefault(
                t["id"],
                {"status": "pending", "attempts": 0, "handoffs": 0, "notes": [],
                 "started_at": None, "finished_at": None, "commit": None, "worker": None},
            )
            st["tasks"][t["id"]].setdefault("worker", None)
        st.setdefault("workers", {})
        try:
            yield st
        finally:
            msg = st.pop("_commit_msg", None)
            st["updated_at"] = iso()
            save_json(STATE_FILE, st)
            if msg:
                commit_bookkeeping(msg)


def init_state(tasks):
    return {
        "version": 2,
        "created_at": iso(),
        "updated_at": iso(),
        "current": None,
        "workers": {},
        "tasks": {
            t["id"]: {"status": "pending", "attempts": 0, "handoffs": 0, "notes": [],
                      "started_at": None, "finished_at": None, "commit": None, "worker": None}
            for t in tasks
        },
    }


def run(cmd, cwd=None, timeout=600, check=False):
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout, check=check)


def git(*args, cwd=None, timeout=120):
    return run(["git", *args], cwd=cwd or ROOT, timeout=timeout)


def is_git_repo():
    return git("rev-parse", "--git-dir").returncode == 0


def is_main_worktree():
    if not is_git_repo():
        return True
    common = git("rev-parse", "--git-common-dir").stdout.strip()
    if not common:
        return True
    return Path(common).resolve() == (ROOT / ".git").resolve()


def git_head(cwd=None):
    r = git("rev-parse", "--short", "HEAD", cwd=cwd)
    return r.stdout.strip() or None if r.returncode == 0 else None


# ---------------- 锁 ----------------

def worker_lock(worker):
    LOCK_DIR.mkdir(parents=True, exist_ok=True)
    return LOCK_DIR / f"{worker}.lock"


def lock_age(path):
    if not path.exists():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return (now() - datetime.fromisoformat(data["started_at"])).total_seconds() / 60
    except Exception:
        return 10**6


def read_lock(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return {}


def acquire_worker_lock(worker, task_id, stale_minutes):
    path = worker_lock(worker)
    age = lock_age(path)
    if age is not None and age < stale_minutes:
        lk = read_lock(path)
        raise SystemExit(
            f"worker {worker} 忙：{lk.get('task_id')} 已运行 {age:.1f} 分钟。"
            f"确认进程已死可删除 {path}"
        )
    if age is not None:
        print(f"[warn] 接管 {worker} 的陈旧锁（{age:.0f} 分钟）")
        path.unlink(missing_ok=True)
    path.write_text(json.dumps(
        {"task_id": task_id, "pid": os.getpid(), "host": socket.gethostname(), "started_at": iso()},
        ensure_ascii=False), encoding="utf-8")


def release_worker_lock(worker):
    worker_lock(worker).unlink(missing_ok=True)


# ---------------- 任务选择 ----------------

def task_map():
    return {t["id"]: t for t in load_json(TASKS_FILE, {"tasks": []})["tasks"]}


def eligible(st, tasks, worker=None):
    done = {tid for tid, v in st["tasks"].items() if v["status"] in ("done", "skipped")}
    busy = {v["task_id"] for w, v in st["workers"].items() if w != worker and v.get("task_id")}
    busy_scopes = []
    for tid in busy:
        t = next((x for x in tasks if x["id"] == tid), None)
        if t:
            busy_scopes.append((tid, t["scope"]))

    out = []
    for t in tasks:
        item = st["tasks"][t["id"]]
        if item["status"] != "pending":
            continue
        if not all(d in done for d in t["deps"]):
            continue
        if t["id"] in busy:
            continue
        if any(scopes_conflict(t["scope"], s) for _, s in busy_scopes):
            continue
        out.append(t)

    rank = {"P0": 0, "P1": 1, "P2": 2}
    out.sort(key=lambda t: (rank.get(t["priority"], 9), t["level"], t["id"]))
    return out


def scopes_conflict(a, b):
    for x in a:
        for y in b:
            if x == y or x.startswith(y + "/") or y.startswith(x + "/"):
                return True
    return False


def counts(st):
    c = {}
    for v in st["tasks"].values():
        c[v["status"]] = c.get(v["status"], 0) + 1
    return c


# ---------------- worktree ----------------

def worktree_path(worker):
    return WT_ROOT / worker


def branch_name(task_id, worker):
    return f"task/{task_id}-{worker}"


BOOKKEEPING = ("runner/state.json", "runner/PROGRESS.md", "runner/CURRENT.md")


def ensure_clean_main():
    """允许 runner 自己的账本文件处于未提交状态，其余必须干净。"""
    r = git("status", "--porcelain")
    dirty = []
    for line in r.stdout.splitlines():
        path = line[3:].strip()
        if any(path == b or path.startswith(b) for b in BOOKKEEPING):
            continue
        dirty.append(line)
    if dirty:
        raise SystemExit("主仓库有未提交改动，无法创建 worktree。先提交或清理：\n" + "\n".join(dirty[:30]))


def commit_bookkeeping(msg):
    """把 runner 的账本单独提交，避免污染任务提交，也让进度可随仓库恢复。"""
    if not is_git_repo():
        return
    paths = [p for p in BOOKKEEPING if (ROOT / p).exists()]
    if not paths:
        return
    git("add", "--", *paths)
    staged = git("diff", "--cached", "--name-only")
    if not staged.stdout.strip():
        return
    git("commit", "-m", f"chore(runner): {msg}", "--", *paths)


def create_worktree(worker, task_id):
    if not is_git_repo():
        raise SystemExit("未初始化 git，无法使用多 worker。先 git init && git add -A && git commit -m init")
    if git_head() is None:
        raise SystemExit("git 还没有任何提交，无法创建 worktree")
    ensure_clean_main()
    path = worktree_path(worker)
    if path.exists():
        git("worktree", "remove", "--force", str(path))
        git("worktree", "prune")
        shutil.rmtree(path, ignore_errors=True)
    branch = branch_name(task_id, worker)
    r = git("worktree", "add", "-B", branch, str(path), "HEAD")
    if r.returncode != 0:
        raise SystemExit(f"创建 worktree 失败：{r.stderr.strip()}")
    return path, branch


def remove_worktree(worker, delete_branch=None):
    path = worktree_path(worker)
    if path.exists():
        git("worktree", "remove", "--force", str(path))
        git("worktree", "prune")
        shutil.rmtree(path, ignore_errors=True)
    if delete_branch:
        git("branch", "-D", delete_branch)


def merge_branch(branch, tid):
    """把任务分支合回主分支；失败则 abort 并返回错误。"""
    MERGE_LOCK.parent.mkdir(parents=True, exist_ok=True)
    with MERGE_LOCK.open("w") as lk:
        fcntl.flock(lk, fcntl.LOCK_EX)
        r = git("merge", "--no-ff", branch, "-m", f"merge {tid} ({branch})")
        if r.returncode != 0:
            git("merge", "--abort")
            return False, (r.stderr or r.stdout).strip()[:800]
        return True, ""


# ---------------- 渲染 ----------------

def render_current(task, item, budget, workdir, worker):
    reading = "\n".join(f"- {x}" for x in READING.get(task["epic"], []))
    retry = ""
    if item["attempts"] > 1 or item["notes"]:
        notes = "\n".join(f"- {n}" for n in item["notes"][-5:])
        retry = f"\n## 上一次的遗留信息（务必先看）\n{notes}\n"

    if worker == "main":
        where = f"工作目录：`{ROOT}`"
        finish = "python3 runner/tick.py"
    else:
        where = (f"工作目录（独立 worktree）：`{workdir}`\n"
                 f"分支：`{branch_name(task['id'], worker)}`　worker：`{worker}`\n\n"
                 f"> 先 `cd {workdir}`。不要在主仓库改代码。")
        finish = f"python3 {ROOT}/runner/tick.py"

    content = f"""# 当前任务 · {task['id']} {task['title']}

| 项 | 值 |
|---|---|
| 优先级 | {task['priority']} |
| 预估 | {task['estimate_days']} 人日 |
| 所属 | {task['epic']} {task['epic_name']} |
| 尝试 | 第 {item['attempts']} 次 |
| 时间预算 | {budget} 分钟 |
| 任务卡来源 | {task['source']} |

{where}

## 必读（先读再动手）
{reading}
- 任务卡原文见下方；如与文档冲突，以任务卡为准

## 任务卡原文

{task['card']}
{retry}
## 执行约束

1. **只做本任务**：不顺手重构、不提前实现后续任务、不改无关模块。
2. **先读后写**：动手前先看已有代码与接口，遵循 `docs/03` 的包结构与签名。
3. **测试是交付的一部分**：新增逻辑必须有测试；改完跑 `go build ./...` 与相关 `go test`。
4. **不静默降级**：不支持的能力返回明确错误，不假装成功。
5. **落盘优先**：每完成一个可工作的阶段就保存文件；不要把所有改动留到最后。
6. **时间不够就交接**：剩余时间不足 10 分钟时，停止开新坑，跑一次测试，然后 handoff。

## 完成动作（三选一，必须执行）

```bash
# 成功：测试通过后先 commit
git add -A && git commit -m "feat({task['id']}): {task['title']}"
{finish} done --worker {worker} --note "一句话说明做了什么、测试结果"

# 失败
{finish} fail --worker {worker} --note "失败现象 + 已尝试的方案 + 疑似原因"

# 时间到
{finish} handoff --worker {worker} --note "已完成 X；未完成 Y；下一步建议 Z"
```

> 不要修改 `runner/state.json` 与 `runner/tasks.json`；
> 多 worker 模式下必须带 `--worker {worker}`，且命令用上面的绝对路径执行。
"""
    path = (workdir / "runner" / "CURRENT.md") if worker != "main" else CURRENT_FILE
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def render_progress(st, tasks):
    c = counts(st)
    total = len(tasks)
    finished = c.get("done", 0) + c.get("skipped", 0)
    lines = [
        "# GoDW 执行进度",
        "",
        f"更新时间：{iso()}　完成 {finished}/{total}　"
        f"待执行 {c.get('pending', 0)}　运行中 {c.get('running', 0)}　"
        f"阻塞 {c.get('blocked', 0)}",
        "",
    ]
    active = [(w, v) for w, v in st.get("workers", {}).items() if v.get("task_id")]
    if active:
        lines.append("进行中：" + "　".join(f"`{w}` → {v['task_id']}" for w, v in active))
        lines.append("")

    lines += ["| Epic | 任务 | 完成 | 阻塞 | 状态 |", "|---|---|---|---|---|"]
    for epic, name in load_json(TASKS_FILE, {"epics": {}})["epics"].items():
        ids = [t["id"] for t in tasks if t["epic"] == epic]
        if not ids:
            continue
        d = sum(1 for i in ids if st["tasks"][i]["status"] in ("done", "skipped"))
        b = sum(1 for i in ids if st["tasks"][i]["status"] == "blocked")
        filled = int(10 * d / len(ids))
        lines.append(f"| {epic} {name} | {len(ids)} | {d} | {b} | `{'█' * filled}{'░' * (10 - filled)}` |")

    blocked = [tid for tid, v in st["tasks"].items() if v["status"] == "blocked"]
    if blocked:
        lines += ["", "## 阻塞任务", ""]
        for tid in blocked:
            t = next((x for x in tasks if x["id"] == tid), None)
            note = (st["tasks"][tid]["notes"] or ["无备注"])[-1]
            lines.append(f"- **{tid} {t['title'] if t else ''}**：{note}")

    PROGRESS_FILE.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return "\n".join(lines)


# ---------------- 命令 ----------------

def ensure_state():
    if not STATE_FILE.exists():
        with state_tx():
            pass


def cmd_status(args):
    ensure_state()
    st = load_json(STATE_FILE, {"tasks": {}, "workers": {}})
    tasks = load_json(TASKS_FILE, {"tasks": []})["tasks"]
    c = counts(st) if st.get("tasks") else {}
    total = len(tasks)
    print(f"总任务 {total}　完成 {c.get('done', 0)}　运行中 {c.get('running', 0)}　"
          f"待执行 {c.get('pending', 0)}　阻塞 {c.get('blocked', 0)}　跳过 {c.get('skipped', 0)}")
    for w, v in st.get("workers", {}).items():
        if v.get("task_id"):
            print(f"worker {w}: {v['task_id']}（{lock_age(worker_lock(w)) or 0:.1f} 分钟）")
    if c.get("done") == total:
        print("全部任务已完成 🎉")


def cmd_plan(args):
    meta = load_json(TASKS_FILE, None)
    if meta is None:
        raise SystemExit("缺少 tasks.json")
    tasks = meta["tasks"]
    by_level = {}
    for t in tasks:
        by_level.setdefault(t["level"], []).append(t)

    print(f"任务 {meta['count']} 个，合计 {meta['total_days']} 人日")
    print(f"关键路径 {meta['critical_path_days']} 人日：{' → '.join(meta['critical_path'])}")
    print()
    print("| 层 | 任务数 | 建议并行组（scope 不冲突） |")
    print("|---|---|---|")
    for lv in sorted(by_level):
        ts = sorted(by_level[lv], key=lambda x: x["id"])
        groups = []
        for t in ts:
            placed = False
            for g in groups:
                if not any(scopes_conflict(t["scope"], other["scope"]) for other in g):
                    g.append(t)
                    placed = True
                    break
            if not placed:
                groups.append([t])
        desc = " ｜ ".join(" + ".join(x["id"] for x in g) for g in groups)
        print(f"| L{lv} | {len(ts)} | {desc} |")

    print()
    print("可立即开工：" + ", ".join(t["id"] for t in tasks if not t["deps"]))
    print("建议同时开 2–3 个 worker；同一条 scope 上的任务会被自动错开。")


def cmd_next(args):
    worker = args.worker
    tasks = load_json(TASKS_FILE, {"tasks": []})["tasks"]

    if not is_main_worktree():
        raise SystemExit("请在主仓库执行 tick.py（不要在 worktree 里执行）")

    with state_tx() as st:
        active = st["workers"].get(worker, {}).get("task_id")
        if active:
            age = lock_age(worker_lock(worker)) or 0
            if age < args.stale:
                raise SystemExit(f"worker {worker} 正在执行 {active}（{age:.1f} 分钟）")
            print(f"[warn] {worker} 的锁已过期，接管")
            old = st["tasks"].get(active)
            if old and old["status"] == "running":
                old["status"] = "pending"
                old["worker"] = None
                old["notes"].append(f"{iso()} 锁过期自动退回")
            st["workers"].pop(worker, None)

        pool = eligible(st, tasks, worker=worker)
        if not pool:
            c = counts(st)
            if c.get("done", 0) + c.get("skipped", 0) == len(tasks):
                print("全部任务已完成。")
            else:
                print("当前没有可领取的任务：依赖未满足，或与其它 worker 的文件范围冲突。")
                other = [f"{w}→{v['task_id']}" for w, v in st["workers"].items() if v.get("task_id")]
                if other:
                    print("进行中：" + ", ".join(other))
            return

        task = pool[0]
        acquire_worker_lock(worker, task["id"], args.stale)

        branch = None
        if worker == "main":
            workdir = ROOT
        else:
            try:
                workdir, branch = create_worktree(worker, task["id"])
            except BaseException:
                release_worker_lock(worker)
                raise

        item = st["tasks"][task["id"]]
        item["status"] = "running"
        item["attempts"] += 1
        item["started_at"] = iso()
        item["worker"] = worker
        st["workers"][worker] = {
            "task_id": task["id"], "branch": branch,
            "worktree": str(workdir) if worker != "main" else str(ROOT),
            "started_at": iso(),
            "deadline": iso(now() + timedelta(minutes=args.budget)),
        }
        st["current"] = {"task_id": task["id"], "worker": worker, "status": "running",
                         "started_at": iso(), "budget_minutes": args.budget}
        render_current(task, item, args.budget, workdir, worker)

    print(f"【{task['id']}】{task['title']}")
    print(f"worker {worker}　预算 {args.budget} 分钟")
    if worker != "main":
        print(f"工作目录：{workdir}（分支 {branch}）")
    cur = (workdir / "runner" / "CURRENT.md") if worker != "main" else CURRENT_FILE
    print(f"1. 读取并严格执行 {cur}")
    print("2. 结束后必须执行 done / fail / handoff 之一")


def _current_for(st, worker):
    w = st["workers"].get(worker)
    if not w or not w.get("task_id"):
        raise SystemExit(f"worker {worker} 当前没有任务")
    return w["task_id"], w


def _verify(workdir):
    if not (workdir / "go.mod").exists():
        return True, ""
    r = run(["go", "build", "./..."], cwd=workdir, timeout=600)
    if r.returncode != 0:
        return False, (r.stderr or r.stdout).strip()[:1000]
    return True, ""


def cmd_done(args):
    worker = args.worker
    if not is_main_worktree():
        raise SystemExit("请在主仓库执行 tick.py")

    workdir = ROOT if worker == "main" else worktree_path(worker)
    with state_tx() as st:
        tid, w = _current_for(st, worker)
        item = st["tasks"][tid]

        if worker != "main":
            dirty = git("status", "--porcelain", cwd=workdir)
            if dirty.returncode != 0:
                raise SystemExit(f"worktree 不存在：{workdir}")
            if dirty.stdout.strip():
                raise SystemExit("worktree 有未提交改动，先 commit：\n" + dirty.stdout[:1000])
            ok, err = _verify(workdir)
            if not ok:
                item["notes"].append(f"{iso()} 合并前校验失败：{err}")
                print("合并前 go build 失败，未合入主分支。修好后重试 done，或 fail 交接。")
                print(err)
                return
            ok, err = merge_branch(w["branch"], tid)
            if not ok:
                item["status"] = "blocked"
                item["notes"].append(f"{iso()} merge 冲突：{err}")
                item["finished_at"] = iso()
                st["workers"].pop(worker, None)
                release_worker_lock(worker)
                remove_worktree(worker)
                print(f"{tid} 合并冲突，已标记 blocked。分支保留：{w['branch']}")
                return
            remove_worktree(worker)

        item["status"] = "done"
        item["finished_at"] = iso()
        item["commit"] = git_head()
        item["worker"] = None
        if args.note:
            item["notes"].append(f"{iso()} done: {args.note}")
        st["workers"].pop(worker, None)
        st["current"] = {"task_id": tid, "worker": worker, "status": "done"}
        release_worker_lock(worker)
        all_tasks = load_json(TASKS_FILE, {"tasks": []})["tasks"]
        render_progress(st, all_tasks)
        c = counts(st)
        print(f"{tid} 完成。进度 {c.get('done', 0)}/{len(all_tasks)}，"
              f"阻塞 {c.get('blocked', 0)}，待执行 {c.get('pending', 0)}")
        st["_commit_msg"] = f"{tid} done"


def _finish_retry(args, kind):
    worker = args.worker
    if not is_main_worktree():
        raise SystemExit("请在主仓库执行 tick.py")
    with state_tx() as st:
        tid, w = _current_for(st, worker)
        item = st["tasks"][tid]
        note = args.note or ("未说明" if kind == "fail" else "时间到，未说明进度")
        item["notes"].append(f"{iso()} {kind}: {note}")
        if kind == "handoff":
            item["handoffs"] += 1

        if item["attempts"] >= MAX_ATTEMPTS or item["handoffs"] >= MAX_HANDOFFS:
            item["status"] = "blocked"
            item["finished_at"] = iso()
            msg = f"{tid} 连续失败/交接达上限，标记 blocked"
        else:
            item["status"] = "pending"
            msg = f"{tid} 已退回待执行（第 {item['attempts']}/{MAX_ATTEMPTS} 次）"

        item["worker"] = None
        st["workers"].pop(worker, None)
        st["current"] = {"task_id": tid, "worker": worker, "status": kind}
        release_worker_lock(worker)
        if worker != "main":
            remove_worktree(worker, delete_branch=w.get("branch"))
        print(msg)
        st["_commit_msg"] = f"{tid} {kind}"


def cmd_fail(args):
    _finish_retry(args, "fail")


def cmd_handoff(args):
    _finish_retry(args, "handoff")


def cmd_skip(args):
    with state_tx() as st:
        tid = args.task
        if not tid:
            cur = st.get("current") or {}
            tid = cur.get("task_id")
        if not tid:
            raise SystemExit("用法：tick.py skip T0406 --note '原因'")
        st["tasks"][tid]["status"] = "skipped"
        st["tasks"][tid]["notes"].append(f"{iso()} skip: {args.note or '人工跳过'}")
        for w, v in list(st["workers"].items()):
            if v.get("task_id") == tid:
                st["workers"].pop(w)
                release_worker_lock(w)
                if w != "main":
                    remove_worktree(w, delete_branch=v.get("branch"))
        print(f"{tid} 已跳过")
        st["_commit_msg"] = f"{tid} skipped"


def cmd_reset(args):
    with state_tx() as st:
        if args.task not in st["tasks"]:
            raise SystemExit(f"未知任务 {args.task}")
        st["tasks"][args.task].update(
            {"status": "pending", "attempts": 0, "handoffs": 0, "finished_at": None, "worker": None}
        )
        st["tasks"][args.task]["notes"].append(f"{iso()} reset by user")
        st["_commit_msg"] = f"{args.task} reset"
        print(f"{args.task} 已重置为 pending")


def cmd_report(args):
    ensure_state()
    st = load_json(STATE_FILE, {"tasks": {}, "workers": {}})
    tasks = load_json(TASKS_FILE, {"tasks": []})["tasks"]
    print(render_progress(st, tasks))
    print(f"\n已写入 {PROGRESS_FILE}")


def main():
    p = argparse.ArgumentParser(description="GoDW 无人值守任务执行器")
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("status")
    s.set_defaults(func=cmd_status)

    pl = sub.add_parser("plan", help="查看依赖分层与并行建议")
    pl.set_defaults(func=cmd_plan)

    n = sub.add_parser("next")
    n.add_argument("--worker", default="main")
    n.add_argument("--budget", type=int, default=45)
    n.add_argument("--stale", type=int, default=90)
    n.set_defaults(func=cmd_next)

    for name, fn in (("done", cmd_done), ("fail", cmd_fail), ("handoff", cmd_handoff)):
        sp = sub.add_parser(name)
        sp.add_argument("--worker", default="main")
        sp.add_argument("--note", default="")
        sp.set_defaults(func=fn)

    k = sub.add_parser("skip")
    k.add_argument("task", nargs="?", default=None)
    k.add_argument("--note", default="")
    k.set_defaults(func=cmd_skip)

    r = sub.add_parser("reset")
    r.add_argument("task")
    r.set_defaults(func=cmd_reset)

    rp = sub.add_parser("report")
    rp.set_defaults(func=cmd_report)

    args = p.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
