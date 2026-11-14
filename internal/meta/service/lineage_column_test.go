package service

import (
	"testing"

	"github.com/godw/godw/internal/lineage/graph"
	"github.com/godw/godw/internal/meta/model"
)

func links() []graph.ColumnLink {
	r := func(t, c string) graph.ColumnRef { return graph.ColumnRef{Table: t, Column: c} }
	return []graph.ColumnLink{
		{Upstream: r("dwd_order", "dt"), Downstream: r("dws_order_daily", "dt")},
		{Upstream: r("dwd_order", "amount"), Downstream: r("dws_order_daily", "gmv")},
		{Upstream: r("dwd_order", "id"), Downstream: r("dws_order_daily", "order_cnt")},
	}
}

// 默认列不能是 dt：分区列在每张表都有，但它不是用户打开血缘页想先看的字段。
func TestPickFocusColumnSkipsPartition(t *testing.T) {
	specs := map[string]*model.ModelSpec{
		"dws_order_daily": {
			PrimaryKey:  []string{"dt", "status"},
			PartitionBy: []string{"dt"},
		},
	}
	got := pickFocusColumn(links(), "dws_order_daily", specs)
	if got == "dt" {
		t.Fatalf("仍选中分区列 dt")
	}
	if got != "gmv" && got != "order_cnt" {
		t.Fatalf("选了非字段列: %s", got)
	}
}

// 只有作为上游出现过的表，也要能挑出一列，而不是返回空。
func TestPickFocusColumnUpstreamOnly(t *testing.T) {
	if got := pickFocusColumn(links(), "dwd_order", nil); got == "" {
		t.Fatalf("上游表没有挑出列")
	}
}

func TestPickFocusColumnNoLineage(t *testing.T) {
	if got := pickFocusColumn(nil, "unknown", nil); got != "" {
		t.Fatalf("无血缘应返回空，得到 %q", got)
	}
}

func TestEnrichColumns(t *testing.T) {
	specs := map[string]*model.ModelSpec{
		"dws_order_daily": {
			Layer:       "dws",
			PrimaryKey:  []string{"dt"},
			PartitionBy: []string{"dt"},
			Columns:     []model.ColumnSpec{{Name: "gmv", Type: "numeric(18,2)"}},
		},
	}
	g := enrichColumns(graph.ColumnGraph{Nodes: []graph.ColumnNode{
		{ID: "dws_order_daily.gmv", Table: "dws_order_daily", Column: "gmv"},
		{ID: "dws_order_daily.dt", Table: "dws_order_daily", Column: "dt"},
		{ID: "ods.order.amount", Table: "ods.order", Column: "amount"},
	}}, specs)

	byID := map[string]graph.ColumnNode{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if n := byID["dws_order_daily.gmv"]; n.Layer != "dws" || n.Type != "numeric(18,2)" || n.Kind != "field" || n.External {
		t.Fatalf("gmv 元信息不对: %+v", n)
	}
	if n := byID["dws_order_daily.dt"]; n.Kind != "key" {
		t.Fatalf("dt 应为主键（主键优先于分区）: %+v", n)
	}
	if n := byID["ods.order.amount"]; !n.External {
		t.Fatalf("非模型表应标记 External: %+v", n)
	}
}
