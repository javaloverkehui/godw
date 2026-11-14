package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 保留策略是「先算账再动手」：计划必须给出截止日与语句，
// 执行必须真的删掉过期分区并留下审计。
func TestRetentionPolicies(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02", "2024-01-03")

	if items := list(t, e.ok("GET", "/api/v1/retention", nil)); len(items) != 0 {
		t.Fatalf("初始保留策略应为空，得到 %d", len(items))
	}
	// modelName 必填；非 admin/editor 不能保存。
	e.fail("POST", "/api/v1/retention", map[string]any{"retainDays": 30}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/retention", map[string]any{"modelName": "dwd_order"}, http.StatusForbidden)

	saved := obj(t, e.ok("POST", "/api/v1/retention", map[string]any{
		"modelName": "dwd_order", "retainDays": 1, "enabled": true,
	}))
	if field(t, saved, "modelName") != "dwd_order" || fxInt(t, saved, "retainDays") != 1 {
		t.Fatalf("保留策略保存结果不对: %v", saved)
	}
	if items := list(t, e.ok("GET", "/api/v1/retention", nil)); len(items) != 1 {
		t.Fatalf("保留策略列表应有 1 条，得到 %d", len(items))
	}

	// 计划：缺 model 400；未知模型 404；正常模型给出 delete 语句。
	e.fail("GET", "/api/v1/retention/plan", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/retention/plan?model=ghost", nil, http.StatusNotFound)
	plan := obj(t, e.ok("GET", "/api/v1/retention/plan?model=dwd_order", nil))
	if field(t, plan, "action") != "delete" || field(t, plan, "cutoff") == "" {
		t.Fatalf("保留计划不对: %v", plan)
	}
	if stmts := list(t, plan["statements"]); len(stmts) == 0 || !strings.Contains(stmts[0].(string), "DELETE") {
		t.Fatalf("保留计划应给出删除语句: %v", plan)
	}

	// 执行：只有 admin；删除过期分区后返回受影响行数。
	e.as("editor").fail("POST", "/api/v1/retention/run", map[string]any{"model": "dwd_order"}, http.StatusForbidden)
	run := obj(t, e.ok("POST", "/api/v1/retention/run", map[string]any{"model": "dwd_order"}))
	if field(t, run, "model") != "dwd_order" || fxInt(t, run, "deleted") != 2 {
		t.Fatalf("保留执行结果不对: %v", run)
	}
	// 执行后数据应真的被删掉。
	prev := obj(t, e.ok("GET", "/api/v1/table/rows?datasource="+ds+"&schema="+schema+"&table=dwd_order", nil))
	if rows := list(t, prev["rows"]); len(rows) != 0 {
		t.Fatalf("过期数据应已删除，剩余 %d 行", len(rows))
	}
}

// 删除权（erasure）是合规硬要求：必须指定主体、必须写理由、必须留任务轨迹。
func TestErasureJobs(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02")

	if items := list(t, e.ok("GET", "/api/v1/erasure-jobs", nil)); len(items) != 0 {
		t.Fatalf("初始删除任务应为空，得到 %d", len(items))
	}
	// 缺列 / 缺理由 / 列不存在，三种拒绝都要有。
	e.fail("POST", "/api/v1/erasure-jobs", map[string]any{"model": "dwd_order"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/erasure-jobs", map[string]any{
		"model": "dwd_order", "column": "order_id", "value": "1000",
	}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/erasure-jobs", map[string]any{
		"model": "dwd_order", "column": "no_such", "value": "1000", "reason": "用户请求",
	}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/erasure-jobs", map[string]any{
		"model": "dwd_order", "column": "order_id", "value": "1000", "reason": "用户请求",
	}, http.StatusForbidden)

	job := obj(t, e.ok("POST", "/api/v1/erasure-jobs", map[string]any{
		"model": "dwd_order", "column": "order_id", "value": "1000", "reason": "用户行使删除权",
	}))
	if field(t, job, "id") == "" || field(t, job, "status") != "pending" {
		t.Fatalf("删除任务创建结果不对: %v", job)
	}
	if items := list(t, e.ok("GET", "/api/v1/erasure-jobs", nil)); len(items) != 1 {
		t.Fatalf("删除任务列表应有 1 条，得到 %d", len(items))
	}
}

// 元数据搜索与标签是「找得到、标得上、能追回」的三件事。
func TestSearchAndTags(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	// 搜索：模型已发布，按名字能命中；类型过滤也应生效。
	res := obj(t, e.ok("GET", "/api/v1/search?q=dwd_order", nil))
	if fxInt(t, res, "total") < 1 || len(list(t, res["items"])) < 1 {
		t.Fatalf("搜索应命中模型: %v", res)
	}
	if filtered := obj(t, e.ok("GET", "/api/v1/search?q=dwd_order&types=dataset&layer=dwd&tags=a,b", nil)); filtered == nil {
		t.Fatalf("带过滤的搜索应返回结构")
	}
	// 空查询是合法请求，返回结构而不是报错。
	e.ok("GET", "/api/v1/search", nil)

	// 标签：必填校验 + 增删查。
	if tags := list(t, e.ok("GET", "/api/v1/tags", nil)); len(tags) != 0 {
		t.Fatalf("初始标签应为空，得到 %d", len(tags))
	}
	e.fail("POST", "/api/v1/tags", map[string]any{"name": " "}, http.StatusBadRequest)
	tag := obj(t, e.ok("POST", "/api/v1/tags", map[string]any{
		"name": "core", "color": "#1890ff", "description": "核心资产",
	}))
	if field(t, tag, "name") != "core" {
		t.Fatalf("标签保存结果不对: %v", tag)
	}
	if tags := list(t, e.ok("GET", "/api/v1/tags", nil)); len(tags) != 1 {
		t.Fatalf("标签列表应有 1 条，得到 %d", len(tags))
	}

	// 打标：owner 会被翻译成 owner:xxx 标签，随实体标签一起返回。
	attached := obj(t, e.ok("POST", "/api/v1/tags/attach", map[string]any{
		"type": "model", "name": "dwd_order", "tags": []string{"core"}, "owner": "alice",
	}))
	tags := list(t, attached["tags"])
	if len(tags) != 2 {
		t.Fatalf("实体应有 2 个标签，得到 %v", tags)
	}
	if got := obj(t, e.ok("GET", "/api/v1/entity/tags?type=model&name=dwd_order", nil)); len(list(t, got["tags"])) != 2 {
		t.Fatalf("实体标签查询不对: %v", got)
	}
	if hits := obj(t, e.ok("GET", "/api/v1/tags/core/entities", nil)); fxInt(t, hits, "total") < 1 {
		t.Fatalf("按标签应能反查实体: %v", hits)
	}

	// 解除单个标签后只剩 owner。
	e.ok("DELETE", "/api/v1/tags/core/entities/model/dwd_order", nil)
	if got := obj(t, e.ok("GET", "/api/v1/entity/tags?type=model&name=dwd_order", nil)); len(list(t, got["tags"])) != 1 {
		t.Fatalf("解绑后应只剩 owner: %v", got)
	}
	// 删除标签定义。
	e.ok("DELETE", "/api/v1/tags/core", nil)
	if tags := list(t, e.ok("GET", "/api/v1/tags", nil)); len(tags) != 0 {
		t.Fatalf("删除后标签应为空，得到 %d", len(tags))
	}
}

// 数据文档站是「实时拼装」的只读视图，导出只允许 html/md 两种格式。
func TestCatalogDocSite(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	page := obj(t, e.ok("GET", "/api/v1/catalog", nil))
	if field(t, page, "generatedAt") == "" {
		t.Fatalf("文档站应带生成时间: %v", page)
	}
	entries := list(t, page["entries"])
	if len(entries) != 1 || field(t, obj(t, entries[0]), "name") != "dwd_order" {
		t.Fatalf("文档站条目不对: %v", entries)
	}

	// 默认 inline HTML，download=1 时改为附件。
	status, _, html := e.doRaw("GET", "/api/v1/catalog/export", nil)
	if status != http.StatusOK || !strings.Contains(strings.ToLower(html), "<html") {
		t.Fatalf("HTML 导出内容不对(%d): %.80s", status, html)
	}
	status, _, md := e.doRaw("GET", "/api/v1/catalog/export?format=md&download=1", nil)
	if status != http.StatusOK || !strings.Contains(md, "dwd_order") {
		t.Fatalf("Markdown 导出内容不对(%d): %.80s", status, md)
	}
	// 不支持的格式明确 400。
	e.fail("GET", "/api/v1/catalog/export?format=xml", nil, http.StatusBadRequest)
}
