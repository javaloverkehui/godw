package graph

import (
	"sort"
)

// T0606 列级血缘展开。
//
// 表级图回答「这张表从哪来」，列级图回答「这个指标字段从哪来」。
// 排障时真正要回答的是后者：上游某张表的 status 改了枚举值，
// 到底会影响看板里哪几个数字。表级图到这里就不够用了。
//
// 与表级 Focus 的区别：
//   - 节点是「表.列」，同一张表的列在图上是一个分组（前端按表画泳道）；
//   - 方向默认双向，因为看一列通常既要往上追来源，也要往下看用途；
//   - 节点上限更小（300），列级图节点天然比表级多一个数量级。

// ColumnRef 是「表.列」引用。表名用模型名（与表级图节点 ID 一致）。
type ColumnRef struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

// ID 返回图上的唯一标识。
func (c ColumnRef) ID() string {
	if c.Table == "" {
		return c.Column
	}
	return c.Table + "." + c.Column
}

// ColumnLink 是一条列级上游→下游关系。
type ColumnLink struct {
	Upstream   ColumnRef
	Downstream ColumnRef
}

// ColumnNode 是列级图上的一个节点。
type ColumnNode struct {
	ID     string `json:"id"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Layer  string `json:"layer,omitempty"`
	// Kind 取 key|partition|field，前端用来区分主键与普通字段。
	Kind string `json:"kind,omitempty"`
	// Type 是列的数据类型，悬停时显示。
	Type string `json:"type,omitempty"`
	// External 表示这张表不在本工作区模型里（例如外部接入的 ODS 表）。
	External bool `json:"external,omitempty"`
}

// ColumnGraph 是列级子图。
type ColumnGraph struct {
	Focus ColumnRef    `json:"focus"`
	Nodes []ColumnNode `json:"nodes"`
	Edges []ColumnEdge `json:"edges"`
	// UpstreamCount / DownstreamCount 是聚焦列可追溯的列数，用于提示影响面。
	UpstreamCount   int  `json:"upstreamCount"`
	DownstreamCount int  `json:"downstreamCount"`
	Truncated       bool `json:"truncated"`
}

// ColumnEdge 是列级图上的一条边。
type ColumnEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// ColumnFocus 从 focus 出发按方向与深度展开，返回只含相关列的子图。
//
// 与表级不同，这里不做「先 up 再 down 的混合穿透」：一旦允许交叉，
// 一张公共维表会把半个数仓拉进来。需要看另一侧时前端再点一次。
func ColumnFocus(links []ColumnLink, focus ColumnRef, dir Direction, depth int) ColumnGraph {
	if depth <= 0 || depth > 12 {
		depth = 6
	}
	const maxNodes = 300

	down := map[string][]string{}
	up := map[string][]string{}
	for _, l := range links {
		// 同表同列的自引用（SELECT dt FROM t）不构成血缘，留着只会在图上画出自环。
		if l.Upstream.ID() == l.Downstream.ID() {
			continue
		}
		down[l.Upstream.ID()] = append(down[l.Upstream.ID()], l.Downstream.ID())
		up[l.Downstream.ID()] = append(up[l.Downstream.ID()], l.Upstream.ID())
	}

	keep := map[string]bool{focus.ID(): true}
	walk := func(adj map[string][]string) int {
		frontier := []string{focus.ID()}
		count := 0
		for d := 0; d < depth && len(frontier) > 0; d++ {
			next := []string{}
			for _, cur := range frontier {
				for _, nxt := range adj[cur] {
					if keep[nxt] {
						continue
					}
					if len(keep) >= maxNodes {
						return count
					}
					keep[nxt] = true
					count++
					next = append(next, nxt)
				}
			}
			frontier = next
		}
		return count
	}

	out := ColumnGraph{Focus: focus, Nodes: []ColumnNode{}, Edges: []ColumnEdge{}}
	switch dir {
	case Up:
		out.UpstreamCount = walk(up)
	case Down:
		out.DownstreamCount = walk(down)
	default:
		out.UpstreamCount = walk(up)
		out.DownstreamCount = walk(down)
	}

	nodeSet := map[string]ColumnNode{}
	for _, l := range links {
		if l.Upstream.ID() == l.Downstream.ID() {
			continue // 与遍历时同一口径，否则自环仍会画在图上
		}
		if !keep[l.Upstream.ID()] || !keep[l.Downstream.ID()] {
			continue
		}
		from, to := l.Upstream.ID(), l.Downstream.ID()
		out.Edges = append(out.Edges, ColumnEdge{Source: from, Target: to})
		for _, r := range []ColumnRef{l.Upstream, l.Downstream} {
			if _, ok := nodeSet[r.ID()]; ok {
				continue
			}
			nodeSet[r.ID()] = ColumnNode{ID: r.ID(), Table: r.Table, Column: r.Column}
		}
	}

	out.Nodes = make([]ColumnNode, 0, len(nodeSet))
	for _, n := range nodeSet {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Table != out.Nodes[j].Table {
			return out.Nodes[i].Table < out.Nodes[j].Table
		}
		return out.Nodes[i].Column < out.Nodes[j].Column
	})
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].Source != out.Edges[j].Source {
			return out.Edges[i].Source < out.Edges[j].Source
		}
		return out.Edges[i].Target < out.Edges[j].Target
	})
	out.Truncated = len(keep) >= maxNodes
	return out
}
