package graph

import "testing"

func colLinks() []ColumnLink {
	r := func(t, c string) ColumnRef { return ColumnRef{Table: t, Column: c} }
	return []ColumnLink{
		{Upstream: r("ods.order", "amount"), Downstream: r("dwd_order", "paid_amount")},
		{Upstream: r("ods.refund", "amount"), Downstream: r("dwd_order", "paid_amount")},
		{Upstream: r("dwd_order", "paid_amount"), Downstream: r("dws_order_daily", "gmv")},
		{Upstream: r("dwd_order", "status"), Downstream: r("dws_order_daily", "paid_orders")},
		{Upstream: r("dws_order_daily", "gmv"), Downstream: r("ads_gmv", "gmv")},
	}
}

func TestColumnFocusBoth(t *testing.T) {
	g := ColumnFocus(colLinks(), ColumnRef{Table: "dwd_order", Column: "paid_amount"}, Both, 6)
	if g.UpstreamCount != 2 || g.DownstreamCount != 2 {
		t.Fatalf("counts = up %d down %d, want 2/2", g.UpstreamCount, g.DownstreamCount)
	}
	if len(g.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5: %+v", len(g.Nodes), g.Nodes)
	}
	if len(g.Edges) != 4 {
		t.Fatalf("edges = %d, want 4: %+v", len(g.Edges), g.Edges)
	}
	// status 这条边不在焦点路径上，必须被裁掉。
	for _, e := range g.Edges {
		if e.Source == "dwd_order.status" {
			t.Fatalf("unrelated edge kept: %+v", e)
		}
	}
}

func TestColumnFocusUpOnly(t *testing.T) {
	g := ColumnFocus(colLinks(), ColumnRef{Table: "dws_order_daily", Column: "gmv"}, Up, 1)
	if g.DownstreamCount != 0 {
		t.Fatalf("downstream should be empty in up mode, got %d", g.DownstreamCount)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("depth 1 should only reach direct parent, got %d nodes", len(g.Nodes))
	}
}

func TestColumnFocusDepth(t *testing.T) {
	g := ColumnFocus(colLinks(), ColumnRef{Table: "ads_gmv", Column: "gmv"}, Up, 1)
	if len(g.Nodes) != 2 {
		t.Fatalf("depth 1 = %d nodes, want 2", len(g.Nodes))
	}
	g = ColumnFocus(colLinks(), ColumnRef{Table: "ads_gmv", Column: "gmv"}, Up, 3)
	if len(g.Nodes) != 5 {
		t.Fatalf("depth 3 = %d nodes, want 5", len(g.Nodes))
	}
}

// 没有血缘的列不应该凭空造出边，也不应该报错。
func TestColumnFocusIsolated(t *testing.T) {
	g := ColumnFocus(colLinks(), ColumnRef{Table: "dwd_order", Column: "remark"}, Both, 6)
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Fatalf("isolated column produced graph: %+v", g)
	}
	if g.Focus.ID() != "dwd_order.remark" {
		t.Fatalf("focus = %q", g.Focus.ID())
	}
}

// 环状血缘（自引用或互引）不能死循环。
func TestColumnFocusCycle(t *testing.T) {
	r := func(t, c string) ColumnRef { return ColumnRef{Table: t, Column: c} }
	links := []ColumnLink{
		{Upstream: r("a", "x"), Downstream: r("b", "x")},
		{Upstream: r("b", "x"), Downstream: r("a", "x")},
	}
	g := ColumnFocus(links, r("a", "x"), Both, 12)
	if len(g.Nodes) != 2 || len(g.Edges) != 2 {
		t.Fatalf("cycle graph = %d nodes %d edges", len(g.Nodes), len(g.Edges))
	}
}

func TestColumnCache(t *testing.T) {
	c := NewColumnCache(0, 2)
	c.Put("a", ColumnGraph{Focus: ColumnRef{Table: "t", Column: "a"}})
	if g, ok := c.Get("a"); !ok || g.Focus.Column != "a" {
		t.Fatalf("column cache miss")
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatalf("unexpected hit")
	}
	if st := c.Stats(); st["hits"].(int64) != 1 {
		t.Fatalf("stats = %+v", st)
	}
	c.Invalidate()
	if _, ok := c.Get("a"); ok {
		t.Fatalf("invalidate did not clear")
	}
}
