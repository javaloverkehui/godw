package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 管道是「定义 → 校验 → 发布 → 触发」的完整回路。
// 单独覆盖 apply 只能证明 YAML 能存，必须一路走到实例生成才算闭环。
func TestPipelineLifecycle(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()

	if items := list(t, e.ok("GET", "/api/v1/pipelines", nil)); len(items) != 0 {
		t.Fatalf("初始管道应为空，得到 %d", len(items))
	}

	// 非法 YAML：编译失败直接 400。
	e.fail("POST", "/api/v1/pipelines", map[string]any{"yaml": "kind: Pipeline\nnodes: ["}, http.StatusBadRequest)

	// viewer 不能发布管道。
	pipelineYAML := fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema)
	e.as("viewer").fail("POST", "/api/v1/pipelines", map[string]any{"yaml": pipelineYAML}, http.StatusForbidden)

	// 引用未发布的模型：提交即拒，避免上线后才发现缺表。
	msg := e.fail("POST", "/api/v1/pipelines", map[string]any{"yaml": pipelineYAML}, http.StatusBadRequest)
	if !strings.Contains(msg, "未发布") {
		t.Fatalf("应提示模型未发布，得到 %q", msg)
	}

	// 补上模型后发布管道。
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	pipe := fxApplyPipeline(t, e, pipelineYAML, true)
	if field(t, pipe, "name") != "dwd_order_daily" || fxInt(t, pipe, "activeVersion") != 1 {
		t.Fatalf("管道发布结果不对: %v", pipe)
	}

	// 再来一次只存草稿，但列表仍然按名字去重只显示一条。
	fxApplyPipeline(t, e, pipelineYAML, false)
	items := list(t, e.ok("GET", "/api/v1/pipelines", nil))
	if len(items) != 1 {
		t.Fatalf("列表应有 1 条管道，得到 %d", len(items))
	}

	// 触发：manual 允许同业务日期重复触发（每次都应新建实例）。
	first := obj(t, e.ok("POST", "/api/v1/pipelines/dwd_order_daily/trigger", map[string]any{"bizDate": "2024-01-02"}))
	if !fxBool(t, first, "created") {
		t.Fatalf("首次触发应新建实例: %v", first)
	}
	inst := obj(t, first["instance"])
	if field(t, inst, "bizDate") != "2024-01-02" || field(t, inst, "triggerType") != "manual" {
		t.Fatalf("实例字段不对: %v", inst)
	}
	second := obj(t, e.ok("POST", "/api/v1/pipelines/dwd_order_daily/trigger", map[string]any{"bizDate": "2024-01-02"}))
	if !fxBool(t, second, "created") {
		t.Fatalf("manual 重复触发应允许新建: %v", second)
	}

	// 触发不存在的管道：404。
	e.fail("POST", "/api/v1/pipelines/ghost/trigger", map[string]any{"bizDate": "2024-01-02"}, http.StatusNotFound)
	// 空 body 也要能触发：handler 对 decode 错误是刻意忽略的。
	e.ok("PUT", "/api/v1/pipelines/dwd_order_daily/enabled", map[string]any{"enabled": true})

	// 停用：响应回显目标状态，列表里也要同步。
	if off := obj(t, e.ok("PUT", "/api/v1/pipelines/dwd_order_daily/enabled", map[string]any{"enabled": false})); fxBool(t, off, "enabled") {
		t.Fatalf("停用响应不对: %v", off)
	}
	p := obj(t, list(t, e.ok("GET", "/api/v1/pipelines", nil))[0])
	if fxBool(t, p, "enabled") {
		t.Fatalf("列表应显示已停用: %v", p)
	}
	e.fail("PUT", "/api/v1/pipelines/ghost/enabled", map[string]any{"enabled": false}, http.StatusNotFound)
	e.fail("PUT", "/api/v1/pipelines/dwd_order_daily/enabled", `{"enabled":`, http.StatusBadRequest)

	// 选择器解析：layer:dwd 应命中模型与写它的管道。
	res := obj(t, e.ok("GET", "/api/v1/select/preview?select=layer:dwd", nil))
	if models := list(t, res["models"]); len(models) != 1 || models[0] != "dwd_order" {
		t.Fatalf("选择器模型集合不对: %v", res)
	}
	if pipes := list(t, res["pipelines"]); len(pipes) != 1 || pipes[0] != "dwd_order_daily" {
		t.Fatalf("选择器管道集合不对: %v", res)
	}
	// 缺少 select 与未知前缀都是 400。
	e.fail("GET", "/api/v1/select/preview", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/select/preview?select=foo:bar", nil, http.StatusBadRequest)
	// 选中的模型没有管道覆盖时，uncovered 要如实给出。
	unc := obj(t, e.ok("GET", "/api/v1/select/preview?select=+dwd_order", nil))
	if _, ok := unc["uncovered"]; !ok {
		t.Fatalf("应返回 uncovered 字段: %v", unc)
	}

	// 按选择器触发：dryRun 只解析不排队。
	dry := obj(t, e.ok("POST", "/api/v1/runs/select", map[string]any{"select": "layer:dwd", "dryRun": true}))
	if runs := list(t, dry["runs"]); len(runs) != 0 {
		t.Fatalf("dryRun 不应产生运行记录: %v", dry)
	}
	run := obj(t, e.ok("POST", "/api/v1/runs/select", map[string]any{"select": "layer:dwd", "bizDate": "2024-01-03"}))
	runs := list(t, run["runs"])
	if len(runs) != 1 || field(t, obj(t, runs[0]), "pipeline") != "dwd_order_daily" {
		t.Fatalf("运行明细不对: %v", run)
	}
	// trigger_type=select 已在 migrations/0007 进白名单，命中管道必须真的建出实例。
	if item := obj(t, runs[0]); !fxBool(t, item, "created") || item["error"] != nil {
		t.Fatalf("命中管道应创建实例且无错误: %v", item)
	}
	e.fail("POST", "/api/v1/runs/select", map[string]any{"select": "  "}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/runs/select", map[string]any{"select": "layer:dwd"}, http.StatusForbidden)
}

// 试跑（dry-run）与采样（sample-run）是上线前的最后一道闸：
// 前者绝不连库，后者只跑 SELECT。两者的差异必须被这两条用例守住。
func TestPipelineDryRunAndSample(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02", "2024-01-02")
	pipelineYAML := fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema)

	// dry-run：不传 bizDate 时默认 T-1；节点、阶段、写入目标都要给出。
	dry := obj(t, e.ok("POST", "/api/v1/pipelines/dry-run", map[string]any{"yaml": pipelineYAML}))
	if field(t, dry, "bizDate") == "" {
		t.Fatalf("dry-run 应填充默认业务日期: %v", dry)
	}
	nodes := list(t, dry["nodes"])
	if len(nodes) != 2 {
		t.Fatalf("dry-run 应渲染 2 个节点，得到 %d", len(nodes))
	}
	if stages := list(t, dry["stages"]); len(stages) != 2 {
		t.Fatalf("两节点串行应产生 2 个阶段，得到 %d", len(stages))
	}
	written := list(t, dry["wouldWrite"])
	if len(written) != 1 || written[0] != "dwd_order" {
		t.Fatalf("dry-run 写入目标不对: %v", written)
	}
	// 指定参数后 SQL 里应出现真实日期，证明渲染生效而不是原样回显。
	withDate := obj(t, e.ok("POST", "/api/v1/pipelines/dry-run", map[string]any{
		"yaml": pipelineYAML, "bizDate": "2024-01-02",
	}))
	if sqlText := field(t, obj(t, list(t, withDate["nodes"])[0]), "sql"); !strings.Contains(sqlText, "2024-01-02") {
		t.Fatalf("dry-run SQL 未渲染业务日期: %q", sqlText)
	}
	// 解析失败必须在 400，而不是 500。
	e.fail("POST", "/api/v1/pipelines/dry-run", map[string]any{"yaml": "kind: Pipeline"}, http.StatusBadRequest)

	// sample-run：真跑但只取 SELECT，结果带列与行。
	sample := obj(t, e.ok("POST", "/api/v1/pipelines/sample-run", map[string]any{
		"yaml": pipelineYAML, "nodeId": "", "bizDate": "2024-01-02", "limit": 5,
	}))
	if fxInt(t, sample, "count") != 2 {
		t.Fatalf("采样应返回 2 行: %v", sample)
	}
	if cols := list(t, sample["columns"]); len(cols) != 4 {
		t.Fatalf("采样列数不对: %v", cols)
	}
	if !strings.Contains(field(t, sample, "sql"), "godw_sample") {
		t.Fatalf("采样 SQL 应包一层子查询: %q", field(t, sample, "sql"))
	}
	// limit 小于数据量时标记截断。
	trunc := obj(t, e.ok("POST", "/api/v1/pipelines/sample-run", map[string]any{
		"yaml": pipelineYAML, "bizDate": "2024-01-02", "limit": 1,
	}))
	if !fxBool(t, trunc, "truncated") || fxInt(t, trunc, "count") != 1 {
		t.Fatalf("limit=1 应截断: %v", trunc)
	}
	// 指定不存在的节点：没有可采样目标，404。
	e.fail("POST", "/api/v1/pipelines/sample-run", map[string]any{
		"yaml": pipelineYAML, "nodeId": "nope", "bizDate": "2024-01-02",
	}, http.StatusNotFound)
}
