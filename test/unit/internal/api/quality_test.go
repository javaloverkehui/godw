package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// 质量结果的读接口与血缘视图是排障时最先打开的两个页面。
// 这里用手工写入的质量结果做夹具：判定逻辑属于 quality 包，
// HTTP 层要验的是「筛选、聚合、空值形状」是否正确。
func TestQualityAndLineageReads(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02", "2024-01-02")
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 缺 instanceId 的 400 必须在查库前拦住。
	e.fail("GET", "/api/v1/quality", nil, http.StatusBadRequest)
	if items := list(t, e.ok("GET", "/api/v1/quality?instanceId="+id, nil)); len(items) != 0 {
		t.Fatalf("未写入前应为空，得到 %d", len(items))
	}

	// 两条规则：一条通过、一条失败，聚合口径才有区分度。
	err := e.svc.Repo.SaveQualityResults(context.Background(), id, "load",
		[]struct {
			RuleName, Observed, Expected, Severity string
			Passed                                 bool
		}{
			{"order_id_not_null", "0", "", "error", true},
			{"amount_non_negative", "3", "0", "error", false},
		})
	if err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}

	results := list(t, e.ok("GET", "/api/v1/quality?instanceId="+id, nil))
	if len(results) != 2 {
		t.Fatalf("应有 2 条质量结果，得到 %d", len(results))
	}
	if r := obj(t, results[1]); field(t, r, "ruleName") != "amount_non_negative" || fxBool(t, r, "passed") {
		t.Fatalf("质量结果内容不对: %v", r)
	}
	// 实例详情要同时带出质量结果。
	detail := obj(t, e.ok("GET", "/api/v1/instances/"+id, nil))
	if q := list(t, detail["quality"]); len(q) != 2 {
		t.Fatalf("实例详情应含 2 条质量结果，得到 %d", len(q))
	}

	// 概览按规则聚合：失败数为 1 的规则排在前面。
	overview := list(t, e.ok("GET", "/api/v1/quality/overview?limit=10", nil))
	if len(overview) != 2 {
		t.Fatalf("概览应有 2 条规则，得到 %d", len(overview))
	}
	first := obj(t, overview[0])
	if field(t, first, "rule") != "amount_non_negative" || fxInt(t, first, "failed") != 1 {
		t.Fatalf("概览排序或聚合不对: %v", first)
	}

	// 趋势：days 越界由仓储兜底成 14，今天应有一条统计。
	trend := list(t, e.ok("GET", "/api/v1/quality/trend?days=0", nil))
	if len(trend) != 1 {
		t.Fatalf("趋势应有 1 天，得到 %d", len(trend))
	}
	if day := obj(t, trend[0]); fxInt(t, day, "total") != 2 || fxInt(t, day, "passed") != 1 {
		t.Fatalf("趋势统计不对: %v", day)
	}

	// 血缘：模型发布时写入 datasource.pgmain → dwd_order。
	lin := obj(t, e.ok("GET", "/api/v1/lineage", nil))
	if nodes := list(t, lin["nodes"]); len(nodes) != 2 {
		t.Fatalf("血缘节点应有 2 个，得到 %d", len(nodes))
	}
	if edges := list(t, lin["edges"]); len(edges) != 1 {
		t.Fatalf("血缘边应有 1 条，得到 %d", len(edges))
	}
	if edge := obj(t, list(t, lin["edges"])[0]); field(t, edge, "source") != "datasource.pgmain" || field(t, edge, "target") != "dwd_order" {
		t.Fatalf("血缘方向不对: %v", edge)
	}

	// 图视图：按管道过滤后仍应保留写该模型的边。
	g := obj(t, e.ok("GET", "/api/v1/lineage/graph?pipeline=dwd_order_daily", nil))
	if len(list(t, g["nodes"])) != 2 {
		t.Fatalf("管道血缘图应有 2 个节点: %v", g)
	}
	e.fail("GET", "/api/v1/lineage/graph?pipeline=ghost", nil, http.StatusNotFound)

	// 聚焦子图：缺 table 是 400；指定后按 depth 展开。
	e.fail("GET", "/api/v1/lineage/focus", nil, http.StatusBadRequest)
	focus := obj(t, e.ok("GET", "/api/v1/lineage/focus?table=dwd_order&direction=up&depth=2", nil))
	if len(list(t, focus["nodes"])) == 0 {
		t.Fatalf("聚焦子图不应为空: %v", focus)
	}

	// 路径：from/to 必填，命中时应给出 1 条路径。
	e.fail("GET", "/api/v1/lineage/paths?from=datasource.pgmain", nil, http.StatusBadRequest)
	paths := obj(t, e.ok("GET", "/api/v1/lineage/paths?from=datasource.pgmain&to=dwd_order", nil))
	if fxInt(t, paths, "count") != 1 {
		t.Fatalf("路径数量不对: %v", paths)
	}
	if p := list(t, paths["paths"]); len(p) != 1 || len(list(t, p[0])) != 2 {
		t.Fatalf("路径内容不对: %v", paths)
	}

	// DOT 导出是 Graphviz 文本，不是 JSON。
	status, _, dot := e.doRaw("GET", "/api/v1/lineage/dot?table=dwd_order", nil)
	if status != http.StatusOK || !strings.Contains(dot, "digraph lineage") {
		t.Fatalf("DOT 输出不对(%d): %s", status, dot)
	}
	// 不带 table 时导出全图。
	if status, _, _ := e.doRaw("GET", "/api/v1/lineage/dot", nil); status != http.StatusOK {
		t.Fatalf("全图 DOT 应为 200，得到 %d", status)
	}

	// 缓存统计：两个子缓存都要在。
	cache := obj(t, e.ok("GET", "/api/v1/lineage/cache", nil))
	if cache["table"] == nil || cache["column"] == nil {
		t.Fatalf("缓存统计缺字段: %v", cache)
	}
	// 命中一次后命中数应大于 0（同一张图重复请求走缓存）。
	e.ok("GET", "/api/v1/lineage/graph", nil)
	e.ok("GET", "/api/v1/lineage/graph", nil)
	again := obj(t, e.ok("GET", "/api/v1/lineage/cache", nil))
	if tbl := obj(t, again["table"]); fxInt(t, tbl, "hits") == 0 {
		t.Fatalf("重复请求应命中血缘缓存: %v", tbl)
	}
}

// 画像依赖已采集的元数据：先采集再画像，未采集时必须明确提示而不是拼一条坏 SQL。
func TestQualityProfile(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02", "2024-01-02")

	// 未采集：404 并提示先采集。
	msg := e.fail("POST", "/api/v1/quality/profile", map[string]any{
		"datasource": ds, "schema": schema, "table": "dwd_order",
	}, http.StatusNotFound)
	if !strings.Contains(msg, "采集") {
		t.Fatalf("应提示先采集元数据，得到 %q", msg)
	}

	e.ok("POST", "/api/v1/datasources/"+ds+"/collect", nil)

	prof := obj(t, e.ok("POST", "/api/v1/quality/profile", map[string]any{
		"datasource": ds, "schema": schema, "table": "dwd_order", "sampleRows": 10,
	}))
	if fxInt(t, prof, "rows") != 2 {
		t.Fatalf("画像行数不对: %v", prof)
	}
	if cols := list(t, prof["columns"]); len(cols) != 4 {
		t.Fatalf("画像列数应为 4，得到 %d", len(cols))
	}
	if rec := list(t, prof["recommended"]); len(rec) == 0 {
		t.Fatalf("画像应推荐质量规则: %v", prof)
	}
	if field(t, prof, "sql") == "" {
		t.Fatalf("画像应回显实际执行的 SQL: %v", prof)
	}
	// 非法 JSON 在解码阶段就 400。
	e.fail("POST", "/api/v1/quality/profile", "{", http.StatusBadRequest)
}

// 强制放行是「带责任地继续」：必须有理由、必须留审计、只对失败实例生效。
func TestQualityWaive(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 排队中的实例不能放行：没有失败就谈不上「放行」。
	e.fail("POST", "/api/v1/instances/"+id+"/waive", map[string]any{"reason": "手工跳过"}, http.StatusConflict)

	// 造一次失败：把状态改成 failed（真实失败由执行器写入，这里只关心门禁语义）。
	if err := e.svc.Repo.SetInstanceStatus(context.Background(), id, "failed"); err != nil {
		t.Fatalf("置失败状态失败: %v", err)
	}

	// viewer 无权放行；空理由被拒。
	e.as("viewer").fail("POST", "/api/v1/instances/"+id+"/waive", map[string]any{"reason": "x"}, http.StatusForbidden)
	e.fail("POST", "/api/v1/instances/"+id+"/waive", map[string]any{"reason": "   "}, http.StatusBadRequest)

	res := obj(t, e.ok("POST", "/api/v1/instances/"+id+"/waive", map[string]any{
		"reason": "上游数据源延迟，确认数据无误", "nodeId": "load",
	}))
	if !fxBool(t, res, "waived") || field(t, res, "instance") != id {
		t.Fatalf("放行响应不对: %v", res)
	}

	// 放行后重新排队，下游可以继续；记录里能查到理由与操作人。
	inst := obj(t, obj(t, e.ok("GET", "/api/v1/instances/"+id, nil))["instance"])
	if field(t, inst, "status") != "queued" {
		t.Fatalf("放行后应重新排队: %v", inst)
	}
	waivers := list(t, e.ok("GET", "/api/v1/instances/"+id+"/waivers", nil))
	if len(waivers) != 1 {
		t.Fatalf("应有 1 条放行记录，得到 %d", len(waivers))
	}
	w := obj(t, waivers[0])
	if field(t, w, "reason") == "" || field(t, w, "actor") != e.admin || field(t, w, "nodeId") != "load" {
		t.Fatalf("放行记录内容不对: %v", w)
	}

	// 放行必须留审计：谁在什么时候因为什么跳过了门禁。
	audits := list(t, e.ok("GET", "/api/v1/audit?limit=20", nil))
	found := false
	for _, a := range audits {
		if field(t, obj(t, a), "action") == "quality.waive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应写入 quality.waive 审计: %v", audits)
	}
}
