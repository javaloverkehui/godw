#!/usr/bin/env python3
"""生成固定规模的管道 DAG，用于编排画布的性能与正确性验收。

为什么需要它：500 节点「卡不卡」这件事，靠人工拖半天也说不清；先有一份
节点名、依赖、规模都可复现的 DAG，才有办法量化「首屏多久」「拖动多少帧」。

用法：
    python scripts/gen_dag.py 500 > /tmp/dag500.yaml
    python scripts/gen_dag.py 500 --width 40 --out examples/dag_500.yaml

生成规则（完全确定性，不用随机数）：
  - 节点按层分布，层名 ods/dim/dwd/dws/ads 循环，节点 id 带层前缀与序号；
  - 每个节点依赖上一层 1~2 个节点（下标由节点序号推导），因此不存在环；
  - 第一层没有上游，作为 source 起点；
  - 节点不带 model：这是一份「形状」夹具，用于渲染验收，不参与真实调度。

依赖关系写成 1~2 条而不是随机 3 条，是因为画布的连线密度直接影响边的 DOM 数量，
而本次要验证的正是「边合并 + 视口裁剪」在密集图上是否真的省下来。
"""

import argparse
import sys
from pathlib import Path

LAYERS = ["ods", "dwd", "dws", "ads"]


def build(nodes: int, width: int):
    """返回 (node dicts, dependency map)。节点数会向上取整到整层——
    半层节点会让「每层宽度」这种指标失去意义。"""
    layers = max(1, (nodes + width - 1) // width)
    specs = []
    deps = {}
    idx = 0
    for layer in range(layers):
        layer_name = LAYERS[layer % len(LAYERS)]
        prev_start = (layer - 1) * width
        prev_name = LAYERS[(layer - 1) % len(LAYERS)]
        prev_count = min(width, nodes - prev_start) if layer > 0 else 0
        for col in range(width):
            if idx >= nodes:
                break
            node_id = f"{layer_name}_{idx:04d}"
            if layer == 0:
                deps[node_id] = []
            else:
                # 上一层同列优先，再补一个相邻列：既保证连通，又让同层节点
                # 不会全部挂到同一个上游（那种星形图看不出布局问题）。
                first = f"{prev_name}_{prev_start + (col % prev_count):04d}"
                picked = [first]
                if col % 3 == 1 and prev_count > 2:
                    second = f"{prev_name}_{prev_start + ((col + 1) % prev_count):04d}"
                    if second != first:
                        picked.append(second)
                deps[node_id] = picked
            specs.append({
                "id": node_id,
                "layer": layer_name,
                "type": "sql" if layer else "source",
            })
            idx += 1
    return specs, deps


def render(name: str, specs, deps) -> str:
    lines = [
        "# 由 scripts/gen_dag.py 生成，请勿手工编辑。",
        "# 用途：编排画布的规模与性能验收（首屏 / 拖动 / 缩放）。",
        "kind: Pipeline",
        f"name: {name}",
        f"description: 规模夹具，{len(specs)} 个节点，用于画布渲染与交互回归",
        'schedule: ""',
        "nodes:",
    ]
    for s in specs:
        lines.append(f"  - id: {s['id']}")
        lines.append(f"    type: {s['type']}")
        lines.append("    datasource: pg_dw")
        # 不写 model：这是渲染夹具，不参与真实调度；带 model 会要求模型已发布。
        lines.append("    sql: SELECT 1 AS probe")
        dep = deps[s["id"]]
        if dep:
            lines.append("    depends_on: [" + ", ".join(dep) + "]")
    return "\n".join(lines) + "\n"


def check(specs, deps) -> None:
    ids = [s["id"] for s in specs]
    if len(set(ids)) != len(ids):
        raise SystemExit("节点 ID 重复")
    known = set(ids)
    for node, ups in deps.items():
        for u in ups:
            if u not in known:
                raise SystemExit(f"{node} 依赖不存在的节点 {u}")
    # 拓扑检查：只需按生成顺序验证依赖都指向更早的节点，就天然无环。
    order = {n: i for i, n in enumerate(ids)}
    for node, ups in deps.items():
        for u in ups:
            if order[u] >= order[node]:
                raise SystemExit(f"依赖方向错误：{u} → {node}")


def main() -> int:
    ap = argparse.ArgumentParser(description="生成固定规模的管道 DAG")
    ap.add_argument("nodes", nargs="?", type=int, default=500, help="节点数（默认 500）")
    ap.add_argument("--width", type=int, default=25, help="每层节点数（默认 25）")
    ap.add_argument("--name", default="dag_scale_fixture", help="管道名")
    ap.add_argument("--out", default="", help="输出文件；留空写 stdout")
    args = ap.parse_args()

    if args.nodes < 1:
        raise SystemExit("节点数必须大于 0")
    if args.width < 1:
        raise SystemExit("每层节点数必须大于 0")

    specs, deps = build(args.nodes, args.width)
    check(specs, deps)
    body = render(args.name, specs, deps)

    if args.out:
        out = Path(args.out)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(body, encoding="utf-8")
        print(f"已写入 {out}（{len(specs)} 个节点、{sum(len(v) for v in deps.values())} 条边）",
              file=sys.stderr)
    else:
        sys.stdout.write(body)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
