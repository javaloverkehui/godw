#!/usr/bin/env python3
"""把仓库内的 Markdown 文档编译成静态站点。

设计取舍：不引入 mkdocs/docusaurus——文档量不大，标准库 + markdown 足够，
产物是纯静态文件，可直接放到任何对象存储或 nginx 后面。

用法：
    python3 scripts/build_docs.py              # 输出到 docs-site/
    python3 scripts/build_docs.py --out /tmp/site
"""
import argparse
import html
import pathlib
import re
import shutil

import markdown

ROOT = pathlib.Path(__file__).resolve().parent.parent

# 顺序即侧边栏顺序：先概念，再任务，再运维
DOCS = [
    ("index", "概览", "README.md"),
    ("architecture", "总体架构", "docs/01-总体架构.md"),
    ("metadata", "元数据模型", "docs/02-元数据模型.md"),
    ("interfaces", "核心抽象与接口", "docs/03-核心抽象与接口.md"),
    ("ai", "AI 协作与验收", "docs/04-AI协作与验收规范.md"),
    ("unattended", "无人值守执行", "docs/05-无人值守执行方案.md"),
    ("tasks-e01", "任务 E01-E02", "docs/tasks/E01-E02-基座与元数据.md"),
    ("tasks-e03", "任务 E03-E04", "docs/tasks/E03-E04-模型与管道.md"),
    ("tasks-e05", "任务 E05-E06", "docs/tasks/E05-E06-调度与质量血缘.md"),
    ("tasks-e07", "任务 E07-E08", "docs/tasks/E07-E08-BI与治理.md"),
    ("tasks-e09", "任务 E09-E10", "docs/tasks/E09-E10-前端与交付.md"),
    ("perf", "性能与容量", "docs/06-性能与容量.md"),
    ("ops", "运维手册", "docs/07-运维手册.md"),
    ("plugin", "插件开发指南", "docs/08-插件开发指南.md"),
    ("upgrade", "升级与兼容", "docs/09-升级与兼容.md"),
    ("contrib", "贡献指南", "CONTRIBUTING.md"),
    ("changelog", "变更日志", "CHANGELOG.md"),
]

CSS = """
:root{--bg:#0e1116;--bg-elev:#151a21;--line:#232b36;--text:#e6edf3;--dim:#9aa7b4;--mute:#6b7887;--accent:#4f8cff}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
.layout{display:grid;grid-template-columns:250px 1fr;min-height:100vh}
aside{border-right:1px solid var(--line);padding:22px 0;position:sticky;top:0;height:100vh;overflow:auto}
.brand{padding:0 20px 18px;font-weight:600;font-size:15px}
.brand small{display:block;color:var(--mute);font-weight:400;font-size:10.5px;letter-spacing:.8px;text-transform:uppercase;margin-top:3px}
nav a{display:block;padding:7px 20px;color:var(--dim);text-decoration:none;font-size:13px;border-left:2px solid transparent}
nav a:hover{background:var(--bg-elev);color:var(--text)}
nav a.active{color:#cfe0ff;border-left-color:var(--accent);background:#1d2836}
main{padding:34px 44px 80px;max-width:900px}
h1{font-size:26px;margin:0 0 18px}
h2{font-size:19px;margin:32px 0 12px;padding-bottom:7px;border-bottom:1px solid var(--line)}
h3{font-size:15.5px;margin:24px 0 8px}
p,li{color:#cdd7e1}
a{color:var(--accent)}
code{background:#1b222b;padding:2px 5px;border-radius:4px;font-size:12.5px;font-family:ui-monospace,Menlo,Consolas,monospace}
pre{background:#0b0e13;border:1px solid var(--line);border-radius:8px;padding:14px 16px;overflow:auto}
pre code{background:none;padding:0}
table{border-collapse:collapse;width:100%;margin:14px 0;font-size:13px}
th{text-align:left;background:#1b222b;padding:8px 11px;border:1px solid var(--line);font-weight:600}
td{padding:8px 11px;border:1px solid var(--line)}
blockquote{margin:14px 0;padding:10px 16px;border-left:3px solid var(--accent);background:#141b24;color:var(--dim)}
img{max-width:100%;border-radius:8px;border:1px solid var(--line)}
"""


def render_md(path: pathlib.Path) -> str:
    text = path.read_text(encoding="utf-8")
    # 站内 .md 链接改写成对应 html
    text = re.sub(r"\]\((docs/)?([^)\s]+?)\.md(#[^)]*)?\)", rewrite_link, text)
    return markdown.markdown(text, extensions=["tables", "fenced_code", "toc", "sane_lists"])


def rewrite_link(m: re.Match) -> str:
    target = m.group(2)
    anchor = m.group(3) or ""
    slug = target.split("/")[-1]
    mapping = {
        "README": "index.html",
        "01-总体架构": "architecture.html",
        "02-元数据模型": "metadata.html",
        "03-核心抽象与接口": "interfaces.html",
        "04-AI协作与验收规范": "ai.html",
        "05-无人值守执行方案": "unattended.html",
        "E01-E02-基座与元数据": "tasks-e01.html",
        "E03-E04-模型与管道": "tasks-e03.html",
        "E05-E06-调度与质量血缘": "tasks-e05.html",
        "E07-E08-BI与治理": "tasks-e07.html",
        "E09-E10-前端与交付": "tasks-e09.html",
        "06-性能与容量": "perf.html",
        "07-运维手册": "ops.html",
        "08-插件开发指南": "plugin.html",
        "09-升级与兼容": "upgrade.html",
        "CONTRIBUTING": "contrib.html",
        "CHANGELOG": "changelog.html",
    }
    return "](" + mapping.get(slug, "#") + anchor + ")"


def page(title: str, body: str, nav: str) -> str:
    return f"""<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>{html.escape(title)} · GoDW 文档</title><style>{CSS}</style></head>
<body><div class="layout">
<aside><div class="brand">GoDW<small>Documentation</small></div><nav>{nav}</nav></aside>
<main>{body}</main>
</div></body></html>
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(ROOT / "docs-site"))
    args = ap.parse_args()
    out = pathlib.Path(args.out)
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    pages = []
    for slug, title, rel in DOCS:
        p = ROOT / rel
        if not p.exists():
            continue
        pages.append((slug, title, render_md(p)))

    for slug, title, body in pages:
        navHtml = "".join(
            f'<a href="{s}.html" class="{"active" if s == slug else ""}">{html.escape(tt)}</a>'
            for s, tt, _ in pages
        )
        (out / f"{slug}.html").write_text(page(title, body, navHtml), encoding="utf-8")

    # 首页重定向到 index.html，方便直接开目录
    if pages:
        first = pages[0]
        navHtml = "".join(
            f'<a href="{s}.html" class="{"active" if s == first[0] else ""}">{html.escape(tt)}</a>'
            for s, tt, _ in pages
        )
        (out / "index.html").write_text(page(first[1], first[2], navHtml), encoding="utf-8")

    # 复制截图，保持 md 里的相对路径可用
    shots = ROOT / "docs" / "screenshots"
    if shots.exists():
        shutil.copytree(shots, out / "docs" / "screenshots", dirs_exist_ok=True)

    print(f"生成 {len(pages)} 页 -> {out}")


if __name__ == "__main__":
    main()
