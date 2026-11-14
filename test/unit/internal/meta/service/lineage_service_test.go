package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// modelAt 按分层生成一个合法模型（各层的命名/主键规则不同，测试里必须真的满足，
// 否则测的是校验器而不是血缘）。
func modelAt(datasource, name, layer string) model.ModelSpec {
	spec := modelSpec(datasource, name, "dw", name)
	spec.Layer = layer
	if layer == "ods" {
		spec.PrimaryKey = nil
	}
	return spec
}

// seedLineage 建 ods → dwd → dws 三节点链路，并写入列级映射。
func seedLineage(t *testing.T, svc *service.Service, wsID string) string {
	t.Helper()
	ds := newPgDatasource(t, svc, wsID)
	for _, name := range []string{"ods_order_line", "dwd_order_line", "dws_order_line"} {
		layer := strings.SplitN(name, "_", 2)[0]
		publishModel(t, svc, wsID, modelAt(ds.Name, name, layer))
	}
	if err := svc.Repo.ReplaceLineage(ctxBG(), wsID, "dwd_order_line", []string{"ods_order_line"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	if err := svc.Repo.ReplaceLineage(ctxBG(), wsID, "dws_order_line", []string{"dwd_order_line"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	if err := svc.Repo.ReplaceColumnLineage(ctxBG(), wsID, "dws_order_line", []model.ColumnEdge{
		{DownstreamTable: "dws_order_line", DownstreamColumn: "id", UpstreamTable: "dwd_order_line", UpstreamColumn: "id"},
		{DownstreamTable: "dws_order_line", DownstreamColumn: "amount", UpstreamTable: "dwd_order_line", UpstreamColumn: "amount"},
	}); err != nil {
		t.Fatalf("写列级血缘失败: %v", err)
	}
	svc.InvalidateLineage()
	return ds.Name
}

// 整图 / 管道视图 / 聚焦子图 / 路径 / DOT 是同一份数据的五种切法，必须一致。
func TestLineageGraphViews(t *testing.T) {
	svc, ws := newSvc(t)
	seedLineage(t, svc, ws.ID)

	g, err := svc.LineageGraph(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("LineageGraph 失败: %v", err)
	}
	if len(g.Nodes) < 3 || len(g.Edges) < 2 {
		t.Fatalf("整图应含 3 节点 2 边: %+v", g)
	}
	for _, n := range g.Nodes {
		if n.ID == "dwd_order_line" && n.Layer != "dwd" {
			t.Fatalf("节点应带分层信息: %+v", n)
		}
	}
	// 边上应补上列级配对，否则前端只能显示一条光秃秃的线。
	for _, e := range g.Edges {
		if e.Target == "dws_order_line" && len(e.Columns) != 2 {
			t.Fatalf("边应带列级映射: %+v", e)
		}
	}

	// 按管道过滤：只保留该管道写出的模型作为下游的边。
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "dws_pipe", Nodes: []model.NodeSpec{
		{ID: "w", Type: model.NodeSQL, Model: "dws_order_line", SQL: "SELECT 1"},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	filtered, err := svc.LineageGraphFor(ctxBG(), ws.ID, "dws_pipe")
	if err != nil {
		t.Fatalf("LineageGraphFor 失败: %v", err)
	}
	if len(filtered.Edges) != 1 || filtered.Edges[0].Target != "dws_order_line" {
		t.Fatalf("管道视图应只留一条边: %+v", filtered.Edges)
	}
	if _, err := svc.LineageGraphFor(ctxBG(), ws.ID, "ghost_pipe"); err == nil {
		t.Fatal("不存在的管道应报错")
	}

	// 聚焦：up 只看上游，down 只看下游，双向都要包含。
	up, err := svc.LineageFocus(ctxBG(), ws.ID, "dwd_order_line", "up", 1)
	if err != nil {
		t.Fatalf("LineageFocus 失败: %v", err)
	}
	ids := make([]string, 0, len(up.Nodes))
	for _, n := range up.Nodes {
		ids = append(ids, n.ID)
	}
	if !hasStr(ids, "ods_order_line") || hasStr(ids, "dws_order_line") {
		t.Fatalf("up 视角只应含上游: %v", ids)
	}
	up, err = svc.LineageFocus(ctxBG(), ws.ID, "dwd_order_line", "downstream", 1)
	if err != nil {
		t.Fatalf("LineageFocus 失败: %v", err)
	}
	ids = ids[:0]
	for _, n := range up.Nodes {
		ids = append(ids, n.ID)
	}
	if !hasStr(ids, "dws_order_line") || hasStr(ids, "ods_order_line") {
		t.Fatalf("down 视角只应含下游: %v", ids)
	}

	paths, err := svc.LineagePaths(ctxBG(), ws.ID, "ods_order_line", "dws_order_line", 5)
	if err != nil {
		t.Fatalf("LineagePaths 失败: %v", err)
	}
	if len(paths) != 1 || len(paths[0]) != 3 {
		t.Fatalf("应有一条三跳路径: %+v", paths)
	}

	dot, err := svc.LineageDOT(ctxBG(), ws.ID, "")
	if err != nil {
		t.Fatalf("LineageDOT 失败: %v", err)
	}
	if !strings.HasPrefix(dot, "digraph lineage {") || !strings.Contains(dot, "dwd_order_line") {
		t.Fatalf("DOT 输出不对: %s", dot)
	}
	dot, err = svc.LineageDOT(ctxBG(), ws.ID, "dwd_order_line")
	if err != nil || !strings.Contains(dot, "dws_order_line") {
		t.Fatalf("指定表时应输出其下游: %v %s", err, dot)
	}

	// 缓存：第二次取应命中，失效后重新构建。
	stats := svc.LineageCacheStats()
	if stats["table"] == nil || stats["column"] == nil {
		t.Fatalf("应返回表级/列级两份缓存统计: %+v", stats)
	}
	svc.InvalidateLineage()
	if _, err := svc.LineageGraph(ctxBG(), ws.ID); err != nil {
		t.Fatalf("失效后重建失败: %v", err)
	}
}

// 列级图：不指定列时自动挑一列，且补全分层/主键/类型等展示信息。
func TestColumnGraph(t *testing.T) {
	svc, ws := newSvc(t)
	seedLineage(t, svc, ws.ID)
	svc.InvalidateLineage()

	g, err := svc.ColumnGraph(ctxBG(), ws.ID, "dws_order_line", "", "up", 3)
	if err != nil {
		t.Fatalf("ColumnGraph 失败: %v", err)
	}
	if g.Focus.Table != "dws_order_line" || g.Focus.Column == "" {
		t.Fatalf("应自动选出一个聚焦列: %+v", g.Focus)
	}
	var key *struct{ kind, typ, layer string }
	for _, n := range g.Nodes {
		if n.Table == "dws_order_line" && n.Column == g.Focus.Column {
			key = &struct{ kind, typ, layer string }{n.Kind, n.Type, n.Layer}
		}
	}
	if key == nil {
		t.Fatalf("聚焦节点应出现在图里: %+v", g.Nodes)
	}
	if key.layer != "dws" {
		t.Fatalf("节点应补充分层: %+v", key)
	}
	// 聚焦列若是主键，Kind 应为 key；否则至少是 field。
	if key.kind != "key" && key.kind != "field" {
		t.Fatalf("节点应标注列类型: %+v", key)
	}

	// 没有列级血缘的表：返回空图，不造假节点。
	empty, err := svc.ColumnGraph(ctxBG(), ws.ID, "ods_order_line", "", "", 3)
	if err != nil {
		t.Fatalf("ColumnGraph 失败: %v", err)
	}
	if len(empty.Nodes) != 0 || empty.Focus.Table != "ods_order_line" {
		t.Fatalf("无血缘时应返回空图: %+v", empty)
	}
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, " ", "", "", 3); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("空表名应被拒绝，实际 %v", err)
	}
}
