package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// 语义层把「表」抽象成「维度 + 度量」：数据集、图表、查询三级。
// 这条链路的价值在于编译出的 SQL 与权限注入，接口层要保证字段透传与拒绝理由。
func TestDatasetAndChartFlow(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	today := time.Now().Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", today, today)

	if items := list(t, e.ok("GET", "/api/v1/datasets/semantic", nil)); len(items) != 0 {
		t.Fatalf("初始数据集应为空，得到 %d", len(items))
	}

	// 引用未发布模型 / 缺维度 / 非法度量都要在定义阶段被拒。
	e.fail("POST", "/api/v1/datasets/semantic", map[string]any{"spec": map[string]any{
		"name": "bad", "baseModel": "ghost",
		"dimensions": []any{map[string]any{"name": "status", "field": "status", "type": "categorical"}},
		"measures":   []any{map[string]any{"name": "amount_sum", "field": "amount", "agg": "sum"}},
	}}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/datasets/semantic", map[string]any{"spec": map[string]any{
		"name": "bad", "baseModel": "dwd_order", "dimensions": []any{},
		"measures": []any{map[string]any{"name": "amount_sum", "field": "amount", "agg": "sum"}},
	}}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/datasets/semantic", map[string]any{"yaml": "dimensions: ["}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/datasets/semantic", map[string]any{"yaml": "name: x"}, http.StatusForbidden)

	dataset := map[string]any{
		"name": "sales", "description": "订单销售数据集", "baseModel": "dwd_order",
		"dimensions": []any{
			map[string]any{"name": "status", "field": "status", "type": "categorical"},
			map[string]any{"name": "dt", "field": "dt", "type": "time", "filterable": true},
		},
		"measures": []any{
			map[string]any{"name": "amount_sum", "field": "amount", "agg": "sum", "format": "amount", "unit": "元"},
		},
	}
	saved := obj(t, e.ok("POST", "/api/v1/datasets/semantic", map[string]any{"spec": dataset}))
	if field(t, saved, "name") != "sales" || fxInt(t, saved, "version") != 1 {
		t.Fatalf("数据集保存结果不对: %v", saved)
	}
	if got := obj(t, e.ok("GET", "/api/v1/datasets/semantic/sales", nil)); field(t, got, "baseModel") != "dwd_order" {
		t.Fatalf("数据集详情不对: %v", got)
	}
	e.fail("GET", "/api/v1/datasets/semantic/ghost", nil, http.StatusNotFound)

	// 查询：真跑 SQL 并返回列与行。入口是 /query，数据集名放在 body 里。
	res := obj(t, e.ok("POST", "/api/v1/datasets/semantic/query", map[string]any{
		"dataset": "sales", "dimensions": []string{"status"}, "measures": []string{"amount_sum"}, "limit": 10,
	}))
	if field(t, res, "dataset") != "sales" || fxInt(t, res, "rowCount") != 1 {
		t.Fatalf("数据集查询结果不对: %v", res)
	}
	if cols := list(t, res["columns"]); len(cols) != 2 {
		t.Fatalf("查询列数不对: %v", cols)
	}
	if !strings.Contains(field(t, res, "sql"), "SELECT") {
		t.Fatalf("查询应回显 SQL: %v", res)
	}

	// 只编译不执行：SQL 里有占位符但不会跑。
	built := obj(t, e.ok("POST", "/api/v1/datasets/semantic/sales/sql", map[string]any{
		"dimensions": []string{"status"}, "measures": []string{"amount_sum"},
	}))
	if !strings.Contains(field(t, built, "sql"), "GROUP BY") {
		t.Fatalf("编译结果不对: %v", built)
	}
	e.fail("POST", "/api/v1/datasets/semantic/ghost/sql", map[string]any{}, http.StatusNotFound)

	// 图表：类型清单与保存。
	types := obj(t, e.ok("GET", "/api/v1/charts/types", nil))
	if len(list(t, types["types"])) == 0 {
		t.Fatalf("图表类型清单为空: %v", types)
	}
	if items := list(t, e.ok("GET", "/api/v1/charts", nil)); len(items) != 0 {
		t.Fatalf("初始图表应为空，得到 %d", len(items))
	}
	// 类型不支持 / 数据集不存在都要明确报错。
	e.fail("POST", "/api/v1/charts", map[string]any{"spec": map[string]any{
		"name": "c1", "type": "radar", "dataset": "sales",
	}}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/charts", map[string]any{"spec": map[string]any{
		"name": "c1", "type": "bar", "dataset": "ghost",
	}}, http.StatusNotFound)

	chartRes := obj(t, e.ok("POST", "/api/v1/charts", map[string]any{"spec": map[string]any{
		"name": "sales_by_status", "title": "各状态销售额", "type": "bar", "dataset": "sales",
		"dimensions": []string{"status"}, "measures": []string{"amount_sum"},
		"encoding": map[string]any{"x": "status", "y": "amount_sum"},
	}}))
	chart := obj(t, chartRes["chart"])
	if field(t, chart, "name") != "sales_by_status" {
		t.Fatalf("图表保存结果不对: %v", chartRes)
	}
	if _, ok := chartRes["impact"]; !ok {
		t.Fatalf("图表保存应返回影响面: %v", chartRes)
	}

	// 图表查询：返回系列与点位。
	result := obj(t, e.ok("GET", "/api/v1/charts/sales_by_status/query", nil))
	if field(t, obj(t, result["chart"]), "name") != "sales_by_status" {
		t.Fatalf("图表查询结果不对: %v", result)
	}
	if len(list(t, result["points"])) == 0 {
		t.Fatalf("图表应有数据点: %v", result)
	}
	e.fail("GET", "/api/v1/charts/ghost/query", nil, http.StatusNotFound)

	// 数据集被图表引用时禁止删除：先提示影响面，再解除引用。
	e.fail("DELETE", "/api/v1/datasets/semantic/sales", nil, http.StatusConflict)
	e.ok("DELETE", "/api/v1/charts/sales_by_status", nil)
	e.ok("DELETE", "/api/v1/datasets/semantic/sales", nil)
	if items := list(t, e.ok("GET", "/api/v1/datasets/semantic", nil)); len(items) != 0 {
		t.Fatalf("删除后数据集应为空，得到 %d", len(items))
	}
}
