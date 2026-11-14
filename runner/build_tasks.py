#!/usr/bin/env python3
"""从 docs/tasks/*.md 解析任务卡，生成 runner/tasks.json。

依赖关系是**手工校准的真实依赖**，不是「同 Epic 顺延」：
  - 只保留「不做 A 就没法做 B」的硬依赖；
  - 其余任务只要文件范围（scope）不冲突即可并行；
  - scope 从任务卡的「产出」字段自动提取。

改完任务卡或依赖后重新运行：
    python3 runner/build_tasks.py
"""

import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TASK_DIR = ROOT / "docs" / "tasks"
OUT = Path(__file__).resolve().parent / "tasks.json"

TASK_RE = re.compile(r"^####\s+(T\d{4})\s+·\s+(.+?)\s+`(P\d)`\s+`([\d.]+)d`")
EPIC_RE = re.compile(r"^##\s+(E\d{2})\s+(.+?)（")
PATH_RE = re.compile(r"`([^`]+)`")

EPIC_NAMES = {
    "E01": "工程基座",
    "E02": "元数据与数据源",
    "E03": "分层建模",
    "E04": "管道与 SQL 引擎",
    "E05": "调度与执行",
    "E06": "质量与血缘",
    "E07": "语义层与 BI",
    "E08": "治理与安全",
    "E09": "前端与可视化",
    "E10": "交付与工程化",
}

# ---------------------------------------------------------------------------
# 硬依赖：只有「B 的实现必须建立在 A 的产物之上」才写进来。
# 同 Epic 内不再默认串行；分支任务只要不共享文件就能同时开工。
# ---------------------------------------------------------------------------
DEPS = {
    # E01 基座
    "T0102": ["T0101"],
    "T0103": ["T0101"],
    "T0104": ["T0101"],
    "T0105": ["T0103", "T0104"],
    "T0106": ["T0101"],
    "T0107": ["T0101"],
    "T0108": ["T0101"],
    "T0109": ["T0104", "T0107"],
    "T0110": ["T0102", "T0105"],
    # E02 元数据与数据源
    "T0201": ["T0104"],
    "T0202": ["T0201"],
    "T0203": ["T0202", "T0105"],
    "T0204": ["T0202", "T0103"],
    "T0205": ["T0202"],
    "T0206": ["T0205", "T0204"],
    "T0207": ["T0206", "T0201"],
    "T0208": ["T0207"],
    "T0209": ["T0207"],
    "T0210": ["T0201", "T0203"],
    # E03 建模
    "T0301": ["T0201"],
    "T0302": ["T0301", "T0206"],
    "T0303": ["T0301"],
    "T0304": ["T0302"],
    "T0305": ["T0301", "T0206"],
    "T0306": ["T0302", "T0305"],
    "T0307": ["T0302", "T0306"],
    "T0308": ["T0302"],
    "T0309": ["T0302", "T0207"],
    "T0310": ["T0307"],
    # E04 管道与 SQL
    "T0401": ["T0301"],
    "T0402": ["T0401"],
    "T0403": ["T0401"],
    "T0404": ["T0403", "T0307"],
    "T0405": ["T0206"],
    "T0406": ["T0405", "T0404"],
    "T0407": ["T0406", "T0304"],
    "T0408": ["T0404", "T0406"],
    "T0409": ["T0402", "T0406"],
    "T0410": ["T0404", "T0406", "T0409"],
    # E05 调度与执行
    "T0501": ["T0409"],
    "T0502": ["T0501", "T0402"],
    "T0503": ["T0501"],
    "T0504": ["T0501", "T0502"],
    "T0505": ["T0504"],
    "T0506": ["T0501", "T0406"],
    "T0507": ["T0505", "T0506"],
    "T0508": ["T0505", "T0502"],
    "T0509": ["T0506"],
    "T0510": ["T0504"],
    "T0511": ["T0506"],
    "T0512": ["T0507", "T0511"],
    # E06 质量与血缘
    "T0601": ["T0406"],
    "T0602": ["T0601", "T0506"],
    "T0603": ["T0602", "T0507"],
    "T0604": ["T0602"],
    "T0605": ["T0404"],
    "T0606": ["T0409"],
    "T0607": ["T0605", "T0606"],
    "T0608": ["T0607"],
    "T0609": ["T0608"],
    "T0610": ["T0608"],
    # E07 语义层与 BI
    "T0701": ["T0406"],
    "T0702": ["T0701"],
    "T0703": ["T0701", "T0405"],
    "T0704": ["T0703", "T0105"],
    "T0705": ["T0704", "T0801", "T0803"],
    "T0706": ["T0704"],
    "T0707": ["T0704"],
    "T0708": ["T0707"],
    "T0709": ["T0708"],
    "T0710": ["T0708", "T0804"],
    # E08 治理
    "T0801": ["T0105"],
    "T0802": ["T0801"],
    "T0803": ["T0801", "T0209"],
    "T0804": ["T0105"],
    "T0805": ["T0801"],
    "T0806": ["T0204"],
    "T0807": ["T0803", "T0804"],
    "T0808": ["T0506"],
    # E09 前端
    "T0901": ["T0106"],
    "T0902": ["T0901"],
    "T0903": ["T0902", "T0207"],
    "T0904": ["T0902", "T0307"],
    "T0905": ["T0902", "T0402"],
    "T0906": ["T0902", "T0508"],
    "T0907": ["T0902", "T0608"],
    "T0908": ["T0902", "T0603"],
    "T0909": ["T0902", "T0708"],
    "T0910": ["T0902", "T0801"],
    # E10 交付
    "T1001": ["T0508"],
    "T1002": ["T0709", "T0512"],
    "T1003": ["T0510", "T0704"],
    "T1004": ["T0508"],
    "T1005": ["T0410"],
    "T1006": ["T0409"],
    "T1007": ["T0205", "T0408"],
    "T1008": ["T0512"],
    "T1009": ["T1001"],
    "T1010": ["T1001"],
}


def parse_file(path: Path):
    lines = path.read_text(encoding="utf-8").splitlines()
    epic = None
    tasks = []
    i = 0
    while i < len(lines):
        line = lines[i]
        m = EPIC_RE.match(line)
        if m:
            epic = m.group(1)
        m = TASK_RE.match(line)
        if m:
            tid, title, prio, est = m.groups()
            body = []
            i += 1
            while i < len(lines) and not lines[i].startswith("#### ") and not lines[i].startswith("## "):
                if lines[i].strip() == "---":
                    break
                body.append(lines[i])
                i += 1
            tasks.append(
                {
                    "id": tid,
                    "title": title.strip(),
                    "priority": prio,
                    "estimate_days": float(est),
                    "epic": epic or "E??",
                    "epic_name": EPIC_NAMES.get(epic, ""),
                    "source": str(path.relative_to(ROOT)),
                    "card": "\n".join(body).strip(),
                }
            )
            continue
        i += 1
    return tasks


def extract_scope(card: str):
    """只从「产出」字段提取路径范围，用于判断两个任务能否并行。

    产出里没有路径时返回空列表，调度器会把空 scope 视为「与所有任务冲突」，
    宁可串行也不冒险并发改同一片代码。
    """
    m = re.search(r"\*\*产出\*\*[：:](.+?)(?:\n\*\*|\Z)", card, re.S)
    text = m.group(1) if m else ""
    scopes = set()
    for tok in PATH_RE.findall(text):
        tok = tok.strip()
        if not tok or " " in tok or tok.startswith(("/", "http", "git@")):
            continue
        if tok.endswith(".md"):
            continue
        if "{" in tok:                    # components/{A,B,C} -> components
            tok = tok.split("{", 1)[0].rstrip("/")
        tok = tok.rstrip("/*")
        if not tok:
            continue
        if "/" not in tok and tok not in {"components", "pages", "features", "stores", "hooks", "api", "web", "schema", "migrations", "deploy", "test"}:
            continue
        last = tok.rsplit("/", 1)[-1]
        if "." in last:                   # 是文件，取目录
            tok = tok.rsplit("/", 1)[0]
        if tok.startswith(("docs", "docs-site")):
            continue
        scopes.add(tok)
    return sorted(scopes)


def topo_levels(tasks):
    by_id = {t["id"]: t for t in tasks}
    level = {}

    def calc(tid, stack=()):
        if tid in level:
            return level[tid]
        if tid in stack:
            raise SystemExit(f"依赖成环：{stack + (tid,)}")
        deps = by_id[tid]["deps"]
        level[tid] = 0 if not deps else max(calc(d) for d in deps) + 1
        return level[tid]

    for t in tasks:
        calc(t["id"])
    return level


def critical_path(tasks):
    by_id = {t["id"]: t for t in tasks}
    memo = {}

    def longest(tid):
        if tid in memo:
            return memo[tid]
        t = by_id[tid]
        if not t["deps"]:
            memo[tid] = (t["estimate_days"], [tid])
        else:
            best = max((longest(d) for d in t["deps"]), key=lambda x: x[0])
            memo[tid] = (best[0] + t["estimate_days"], best[1] + [tid])
        return memo[tid]

    return max((longest(t["id"]) for t in tasks), key=lambda x: x[0])


def main():
    all_tasks = []
    for path in sorted(TASK_DIR.glob("*.md")):
        all_tasks.extend(parse_file(path))
    all_tasks.sort(key=lambda t: (t["epic"], t["id"]))

    ids = {t["id"] for t in all_tasks}
    for tid, deps in DEPS.items():
        if tid not in ids:
            raise SystemExit(f"依赖表里的 {tid} 不存在")
        for d in deps:
            if d not in ids:
                raise SystemExit(f"{tid} 依赖了不存在的 {d}")
            if d == tid:
                raise SystemExit(f"{tid} 依赖了自己")

    for t in all_tasks:
        t["deps"] = sorted(set(DEPS.get(t["id"], [])))
        t["scope"] = extract_scope(t["card"]) or [f"epic:{t['epic']}"]

    level = topo_levels(all_tasks)
    for t in all_tasks:
        t["level"] = level[t["id"]]

    total = sum(t["estimate_days"] for t in all_tasks)
    cp_days, cp = critical_path(all_tasks)
    width = {}
    for t in all_tasks:
        width[t["level"]] = width.get(t["level"], 0) + 1

    payload = {
        "generated_from": "docs/tasks/*.md",
        "count": len(all_tasks),
        "total_days": round(total, 1),
        "critical_path_days": round(cp_days, 1),
        "critical_path": cp,
        "max_level_width": max(width.values()),
        "epics": EPIC_NAMES,
        "tasks": all_tasks,
    }
    OUT.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")

    print(f"生成 {OUT}")
    print(f"任务 {len(all_tasks)} 个，合计 {total:g} 人日")
    print(f"关键路径 {cp_days:g} 人日：{' → '.join(cp)}")
    print(f"最大并行宽度 {max(width.values())}（第 {max(width, key=width.get)} 层）")
    print("层级分布：" + "  ".join(f"L{k}={v}" for k, v in sorted(width.items())))
    nodep = [t["id"] for t in all_tasks if not t["deps"]]
    print(f"可立即开工的任务 {len(nodep)} 个：{', '.join(nodep)}")


if __name__ == "__main__":
    main()
