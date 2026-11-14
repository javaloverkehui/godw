package graph_test

import (
	"fmt"
	"testing"

	"github.com/godw/godw/internal/lineage/graph"
)

// 列级图节点天然比表级多一个数量级，节点上限必须真的生效并标记 Truncated，
// 否则一次「看某个公共维表的影响面」就能把浏览器拖死。
func TestColumnFocusTruncatesAtLimit(t *testing.T) {
	// 一张宽表被 400 个下游列引用：深度只有 1，但节点数会冲破上限。
	root := graph.ColumnRef{Table: "wide", Column: "id"}
	links := make([]graph.ColumnLink, 0, 400)
	for i := 0; i < 400; i++ {
		links = append(links, graph.ColumnLink{
			Upstream:   root,
			Downstream: graph.ColumnRef{Table: fmt.Sprintf("t%d", i), Column: "fk"},
		})
	}

	g := graph.ColumnFocus(links, root, graph.Down, 3)
	if !g.Truncated {
		t.Fatalf("超过节点上限必须标记截断，实际节点 %d", len(g.Nodes))
	}
	// 上限是 300 个节点（含 focus），允许遍历循环多收一个。
	if len(g.Nodes) < 295 || len(g.Nodes) > 305 {
		t.Fatalf("截断后节点数应接近上限，得到 %d", len(g.Nodes))
	}
	// 计数与节点数保持同一口径，前端用计数做提示。
	if g.DownstreamCount != len(g.Nodes)-1 {
		t.Fatalf("下游计数应与节点数一致: %d vs %d", g.DownstreamCount, len(g.Nodes))
	}
	// 截断后仍要保证边只连已保留的节点。
	kept := map[string]bool{}
	for _, n := range g.Nodes {
		kept[n.ID] = true
	}
	for _, e := range g.Edges {
		if !kept[e.Source] || !kept[e.Target] {
			t.Fatalf("边引用了被截断的节点: %+v", e)
		}
	}
}
