package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 图形化向导走的是「结构化 spec」而不是 YAML 字符串。
// 这几条用例守的是契约本身：spec 能被接受、能落成与 YAML 等价的对象，
// 以及指标在保存前可以先试算——向导里「改口径立刻看曲线」全靠它。
func TestApplyModelFromSpec(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()

	spec := map[string]any{
		"name":       "dwd_spec_order",
		"layer":      "dwd",
		"datasource": ds,
		"schema":     schema,
		"columns": []any{
			map[string]any{"name": "order_id", "type": "bigint", "nullable": false},
			map[string]any{"name": "amount", "type": "decimal", "nullable": true},
			map[string]any{"name": "dt", "type": "date", "nullable": false},
		},
		"primaryKey":  []string{"order_id"},
		"partitionBy": []string{"dt"},
	}

	// 不带 yaml，只带 spec：向导生成的请求就是这个形状。
	m := obj(t, e.ok("POST", "/api/v1/models", map[string]any{
		"spec": spec, "publish": true, "apply": true,
	}))
	if field(t, m, "name") != "dwd_spec_order" {
		t.Fatalf("模型名不对: %v", m)
	}
	if v := fxInt(t, m, "activeVersion"); v != 1 {
		t.Fatalf("发布后应 activeVersion=1，得到 %d", v)
	}
	if cols := list(t, obj(t, m["spec"])["columns"]); len(cols) != 3 {
		t.Fatalf("列数应为 3，得到 %d", len(cols))
	}
	if ddl := field(t, m, "ddl"); !strings.Contains(ddl, "dwd_spec_order") {
		t.Fatalf("应回显建表 DDL: %q", ddl)
	}

	// spec 路径同样要过分层校验：dwd 缺 dt 不能因为「不是 YAML」就放行。
	e.fail("POST", "/api/v1/models", map[string]any{"spec": map[string]any{
		"name": "dwd_no_dt", "layer": "dwd", "datasource": ds, "schema": schema,
		"columns": []any{map[string]any{"name": "order_id", "type": "bigint", "nullable": false}},
	}, "publish": true}, http.StatusBadRequest)

	// 列非法（空名）也要被拦下。
	e.fail("POST", "/api/v1/models", map[string]any{"spec": map[string]any{
		"name": "dwd_bad_col", "layer": "dwd", "datasource": ds, "schema": schema,
		"columns": []any{map[string]any{"name": "", "type": "bigint"}},
	}, "publish": true}, http.StatusBadRequest)
}

func TestApplyPipelineFromSpec(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	sqlText := "SELECT order_id, amount, status, dt FROM " + schema + ".dwd_order WHERE dt = ${biz_date}"
	spec := map[string]any{
		"kind":     "Pipeline",
		"name":     "dwd_order_spec_daily",
		"schedule": "0 2 * * *",
		"timezone": "Asia/Shanghai",
		"params": []any{
			map[string]any{"name": "biz_date", "type": "date", "required": true},
		},
		"nodes": []any{
			map[string]any{
				"id": "dwd_order", "type": "sql", "datasource": ds, "model": "dwd_order",
				"writeMode": "upsert", "sql": sqlText,
			},
		},
	}

	p := obj(t, e.ok("POST", "/api/v1/pipelines", map[string]any{"spec": spec, "publish": true}))
	if field(t, p, "name") != "dwd_order_spec_daily" || fxInt(t, p, "activeVersion") != 1 {
		t.Fatalf("管道发布结果不对: %v", p)
	}

	// 回读：节点 SQL 与写入模式要原样保留，否则向导里看到的和实际跑的不是一回事。
	items := list(t, e.ok("GET", "/api/v1/pipelines", nil))
	if len(items) != 1 {
		t.Fatalf("应有 1 条管道，得到 %d", len(items))
	}
	node := obj(t, list(t, obj(t, items[0])["spec"].(map[string]any)["nodes"])[0])
	if field(t, node, "sql") != sqlText {
		t.Fatalf("节点 SQL 未原样保留: %v", node["sql"])
	}
	if field(t, node, "writeMode") != "upsert" {
		t.Fatalf("写入模式未保留: %v", node["writeMode"])
	}

	// 触发一次，确认 spec 路径产出的定义能真的排进队列。
	res := obj(t, e.ok("POST", "/api/v1/pipelines/dwd_order_spec_daily/trigger", map[string]any{"bizDate": "2024-01-02"}))
	if !fxBool(t, res, "created") {
		t.Fatalf("触发应新建实例: %v", res)
	}
}

func TestPreviewMetric(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02")

	// 未保存的定义直接试算：向导第三步每改一次口径就调它。
	series := obj(t, e.ok("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{
			"name": "order_amount", "model": "dwd_order", "agg": "sum", "expr": "amount",
			"timeColumn": "dt", "dimensions": []string{"status"},
		},
		"query": map[string]any{"from": "2024-01-01", "to": "2024-01-03", "granularity": "day", "limit": 50},
	}))
	points := list(t, series["points"])
	if len(points) != 1 {
		t.Fatalf("应有 1 个数据点，得到 %d: %v", len(points), series)
	}
	if v, ok := obj(t, points[0])["value"]; !ok || v == nil {
		t.Fatalf("数据点应有值: %v", points[0])
	}

	// 试算不能落库：列表仍为空。
	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 0 {
		t.Fatalf("试算不应保存指标，列表得到 %d 条", len(items))
	}

	// 口径非法与模型未发布都必须在保存前就报错。
	e.fail("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{"name": "bad", "model": "dwd_order", "agg": "median", "expr": "amount"},
	}, http.StatusBadRequest)
	msg := e.fail("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{"name": "ghost", "model": "no_such", "agg": "sum", "expr": "amount"},
	}, http.StatusBadRequest)
	if !strings.Contains(msg, "未发布") {
		t.Fatalf("应提示模型未发布，得到 %q", msg)
	}
}
