package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 「删除」在控制台里是一个按钮，在数据上是一条不可逆的语句。
// 这组用例测的不是 DELETE 跑没跑，而是删之前的三道闸门有没有拦住人：
// 还有运行中的实例 / 还有别处引用它 / 有历史但没确认。

// TestPipelineDefinitionRoundTrip 守住「编辑页读到的定义 = 库里那份」。
// 前端不再手写 YAML 序列化（那会丢掉 params/defaults/node.config），
// 前提是 GET /pipelines/{name}/yaml 返回的 spec 与 yaml 都完整；
// 一旦有人换成「只挑几个字段」的返回结构，这个用例会先红。
func TestPipelineDefinitionRoundTrip(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	def := obj(t, e.ok("GET", "/api/v1/pipelines/dwd_order_daily/yaml", nil))
	if fxInt(t, def, "version") != 1 {
		t.Fatalf("应返回当前生效版本 1: %v", def["version"])
	}
	yamlText := field(t, def, "yaml")
	for _, want := range []string{"name: dwd_order_daily", "params:", "biz_date", "nodes:"} {
		if !strings.Contains(yamlText, want) {
			t.Fatalf("YAML 缺少 %q:\n%s", want, yamlText)
		}
	}
	spec := obj(t, def["spec"])
	if nodes := list(t, spec["nodes"]); len(nodes) != 2 {
		t.Fatalf("spec 应带 2 个节点，得到 %d", len(nodes))
	}

	// 原样提交回来：版本 +1，但定义必须一字不差。
	before, _ := json.Marshal(spec)
	e.ok("POST", "/api/v1/pipelines", map[string]any{"spec": spec, "publish": true})
	after, _ := json.Marshal(obj(t, e.ok("GET", "/api/v1/pipelines/dwd_order_daily/yaml", nil))["spec"])
	if string(before) != string(after) {
		t.Fatalf("定义往返后被改动:\n前 %s\n后 %s", before, after)
	}

	// 草稿管道（未发布）也要能拿到编辑起点：列表接口里它的 spec 是空的。
	fxApplyPipeline(t, e, fxPipelineYAML("draft_only", ds, "dwd_order", schema), false)
	if d := obj(t, e.ok("GET", "/api/v1/pipelines/draft_only/yaml", nil)); fxInt(t, d, "version") != 1 {
		t.Fatalf("草稿管道应回落到最新版本 1: %v", d["version"])
	}

	// 参数缺失要明确拒绝，而不是存一条空管道。
	e.fail("POST", "/api/v1/pipelines", map[string]any{}, http.StatusBadRequest)
}

func TestPipelineDelete(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	// 没跑过的管道：refs 全 0，直接删。
	refs := obj(t, e.ok("GET", "/api/v1/pipelines/dwd_order_daily/refs", nil))
	if fxInt(t, refs, "instances") != 0 || fxInt(t, refs, "running") != 0 {
		t.Fatalf("未运行的管道 refs 应为 0: %v", refs)
	}

	// 跑过一次之后，删除必须显式带上 force：历史会跟着级联删除。
	fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")
	// 触发出来的实例是排队态；这里手动收尾成成功，模拟「跑完了、有历史」的常态。
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`UPDATE instance SET status='success' WHERE pipeline_id=(SELECT id FROM pipeline WHERE name=$1)`,
		"dwd_order_daily"); err != nil {
		t.Fatalf("收尾实例失败: %v", err)
	}
	refs = obj(t, e.ok("GET", "/api/v1/pipelines/dwd_order_daily/refs", nil))
	if fxInt(t, refs, "instances") != 1 {
		t.Fatalf("应有 1 条运行记录: %v", refs)
	}
	msg := e.fail("DELETE", "/api/v1/pipelines/dwd_order_daily", nil, http.StatusConflict)
	if !strings.Contains(msg, "运行记录") {
		t.Fatalf("应提示会清除运行记录，得到 %q", msg)
	}
	// 被拦下时管道还在。
	if items := list(t, e.ok("GET", "/api/v1/pipelines", nil)); len(items) != 1 {
		t.Fatalf("拦下后管道应仍存在，得到 %d 条", len(items))
	}

	// 有排队/运行中的实例时，force 也不放行：执行器还拿着这份版本快照。
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`UPDATE instance SET status='running' WHERE pipeline_id=(SELECT id FROM pipeline WHERE name=$1)`,
		"dwd_order_daily"); err != nil {
		t.Fatalf("构造运行中实例失败: %v", err)
	}
	msg = e.fail("DELETE", "/api/v1/pipelines/dwd_order_daily?force=1", nil, http.StatusConflict)
	if !strings.Contains(msg, "运行中") {
		t.Fatalf("应拦住运行中的管道，得到 %q", msg)
	}

	// 结束实例后 force 生效，历史一并清掉。
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`UPDATE instance SET status='success' WHERE pipeline_id=(SELECT id FROM pipeline WHERE name=$1)`,
		"dwd_order_daily"); err != nil {
		t.Fatalf("结束实例失败: %v", err)
	}
	res := obj(t, e.ok("DELETE", "/api/v1/pipelines/dwd_order_daily?force=1", nil))
	if field(t, res, "deleted") != "dwd_order_daily" {
		t.Fatalf("删除结果不对: %v", res)
	}
	if items := list(t, e.ok("GET", "/api/v1/pipelines", nil)); len(items) != 0 {
		t.Fatalf("删除后列表应为空，得到 %d 条", len(items))
	}
	// 再删一次是 404，不是静默成功——否则前端会以为刚删了一个不存在的对象。
	e.fail("DELETE", "/api/v1/pipelines/dwd_order_daily", nil, http.StatusNotFound)

	// 版本快照随管道级联删除，不留在库里成为孤儿。
	var versions int
	if err := e.svc.Repo.DB.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_version v LEFT JOIN pipeline p ON p.id=v.pipeline_id WHERE p.id IS NULL`).
		Scan(&versions); err != nil {
		t.Fatalf("查询孤儿版本失败: %v", err)
	}
	if versions != 0 {
		t.Fatalf("应无孤儿版本，得到 %d", versions)
	}
}

func TestMetricDeleteAndStructuredApply(t *testing.T) {
	e := newEnv(t)
	fxReadyModel(t, e, "dwd_order")

	// 指标接口现在收结构化对象：页面上改聚合方式不该要求用户拼 YAML。
	metric := obj(t, e.ok("POST", "/api/v1/metrics", map[string]any{
		"metric": map[string]any{
			"name": "gmv", "displayName": "成交额", "model": "dwd_order",
			"agg": "sum", "expr": "amount", "timeColumn": "dt",
			"dimensions": []string{"status"}, "unit": "元",
		},
	}))
	if field(t, metric, "name") != "gmv" || field(t, metric, "displayName") != "成交额" {
		t.Fatalf("结构化入参保存结果不对: %v", metric)
	}

	// 缺 metric 与 yaml 时给出明确报错，而不是存一个空指标。
	e.fail("POST", "/api/v1/metrics", map[string]any{}, http.StatusBadRequest)

	// 被看板引用时默认拦下。
	e.ok("POST", "/api/v1/dashboards", map[string]any{
		"dashboard": map[string]any{
			"name": "trade", "description": "交易看板",
			"cards": []any{map[string]any{"title": "成交额", "metric": "gmv", "days": 30}},
		},
	})
	refs := obj(t, e.ok("GET", "/api/v1/metrics/gmv/refs", nil))
	if ds := list(t, refs["dashboards"]); len(ds) != 1 || ds[0] != "trade" {
		t.Fatalf("refs 应列出引用它的看板: %v", refs)
	}
	msg := e.fail("DELETE", "/api/v1/metrics/gmv", nil, http.StatusConflict)
	if !strings.Contains(msg, "看板") {
		t.Fatalf("应提示看板仍在引用，得到 %q", msg)
	}

	// 确认影响后可以强删；看板本身保留（卡片会显示取数失败，不会连页面一起消失）。
	e.ok("DELETE", "/api/v1/metrics/gmv?force=1", nil)
	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 0 {
		t.Fatalf("删除后指标应为空，得到 %d", len(items))
	}
	if items := list(t, e.ok("GET", "/api/v1/dashboards", nil)); len(items) != 1 {
		t.Fatalf("删指标不应连带删除看板，得到 %d", len(items))
	}
	e.fail("DELETE", "/api/v1/metrics/gmv", nil, http.StatusNotFound)
}

func TestDashboardDelete(t *testing.T) {
	e := newEnv(t)
	fxReadyModel(t, e, "dwd_order")
	e.ok("POST", "/api/v1/metrics", map[string]any{
		"metric": map[string]any{
			"name": "order_cnt", "model": "dwd_order",
			"agg": "count", "expr": "order_id", "timeColumn": "dt",
		},
	})
	e.ok("POST", "/api/v1/dashboards", map[string]any{
		"dashboard": map[string]any{
			"name": "ops", "description": "",
			"cards": []any{map[string]any{"title": "订单量", "metric": "order_cnt", "days": 7}},
		},
	})

	// 看板只删排布，指标必须原样留着。
	e.ok("DELETE", "/api/v1/dashboards/ops", nil)
	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 1 {
		t.Fatalf("删看板不应删除指标，得到 %d 个指标", len(items))
	}
	if items := list(t, e.ok("GET", "/api/v1/dashboards", nil)); len(items) != 0 {
		t.Fatalf("删除后看板应为空，得到 %d", len(items))
	}
	e.fail("DELETE", "/api/v1/dashboards/ops", nil, http.StatusNotFound)

	// viewer 不能删。
	e.ok("POST", "/api/v1/dashboards", map[string]any{
		"dashboard": map[string]any{
			"name": "ops2", "description": "",
			"cards": []any{map[string]any{"title": "订单量", "metric": "order_cnt", "days": 7}},
		},
	})
	e.as("viewer").fail("DELETE", "/api/v1/dashboards/ops2", nil, http.StatusForbidden)
}
