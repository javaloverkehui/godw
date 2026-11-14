package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 这一组专门打「参数校验与解码失败」的分支。
// 这类分支最容易在重构时被悄悄删掉——删掉之后接口仍然能跑，只是把 400 变成了 500，
// 前端拿到的提示从「你的 YAML 有问题」退化成「服务内部错误」。
func TestInputValidationBranches(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 建模：YAML 解析失败要落到 400（CodeModelInvalid），不是 500。
	badYAML := map[string]any{"yaml": "kind: ["}
	e.fail("POST", "/api/v1/models/check", badYAML, http.StatusBadRequest)
	e.fail("POST", "/api/v1/models/diff", badYAML, http.StatusBadRequest)
	e.fail("POST", "/api/v1/models/preview", badYAML, http.StatusBadRequest)
	// 脚手架：请求体本身坏掉。
	e.fail("POST", "/api/v1/models/scaffold", "{", http.StatusBadRequest)
	// 回滚预览：version 必须全是数字。
	e.fail("GET", "/api/v1/models/dwd_order/rollback-preview?version=abc", nil, http.StatusBadRequest)

	// dry-run：YAML 解析失败 400；结构不合法（未知节点类型）也要 400。
	e.fail("POST", "/api/v1/pipelines/dry-run", map[string]any{"yaml": "kind: Pipeline"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/pipelines/dry-run", map[string]any{"yaml": `kind: Pipeline
name: bad_pipe
nodes:
  - id: n1
    type: radar
    sql: SELECT 1
`}, http.StatusBadRequest)

	// sample-run：节点指定的数据源不存在 → 404，而不是静默返回空结果。
	e.fail("POST", "/api/v1/pipelines/sample-run", map[string]any{"yaml": `kind: Pipeline
name: ghost_ds
nodes:
  - id: n1
    type: sql
    datasource: ghost
    sql: SELECT 1 AS n
`}, http.StatusNotFound)
	e.fail("POST", "/api/v1/pipelines/sample-run", "{", http.StatusBadRequest)

	// 质量放行：请求体坏掉。
	e.fail("POST", "/api/v1/instances/"+id+"/waive", "{", http.StatusBadRequest)

	// 元数据采集 / PII：请求体坏掉，以及 PII 采纳时缺字段。
	e.fail("POST", "/api/v1/catalog/collect", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/pii/detect", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/pii/apply", "{", http.StatusBadRequest)
}

// 语义层与治理接口的解码失败分支：同样是「把 500 拉回 400」的守门行为。
func TestGovernanceValidationBranches(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	// 数据集 / 图表：坏 JSON 与 YAML 形态的图表保存。
	e.fail("POST", "/api/v1/datasets/semantic", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/datasets/semantic/query", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/datasets/semantic/sales/sql", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/charts", "{", http.StatusBadRequest)

	// 先建一个可用数据集，再用 YAML 形态保存图表（高级模式入口）。
	e.ok("POST", "/api/v1/datasets/semantic", map[string]any{"yaml": `name: sales
baseModel: dwd_order
dimensions:
  - name: status
    field: status
    type: categorical
measures:
  - name: amount_sum
    field: amount
    agg: sum
`})
	e.ok("POST", "/api/v1/charts", map[string]any{"yaml": `name: sales_yaml
type: table
dataset: sales
dimensions: [status]
measures: [amount_sum]
`})
	e.ok("DELETE", "/api/v1/charts/sales_yaml", nil)

	// 各类配置写入：坏 JSON 一律 400。
	e.fail("POST", "/api/v1/policies/columns", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/policies/rows", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/subscriptions", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/members", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/workspaces", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/retention", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/alert-rules", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/tags", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/tags/attach", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/exports", "{", http.StatusBadRequest)
	e.fail("POST", "/api/v1/shares", "{", http.StatusBadRequest)

	// 导出的下载与驳回：未知 id 与坏 JSON。
	e.fail("GET", "/api/v1/exports/exp_ghost/download", nil, http.StatusNotFound)
	job, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount",
		Status: "pending_approval", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入待审批导出失败: %v", err)
	}
	e.fail("POST", "/api/v1/exports/"+job.ID+"/reject", "{", http.StatusBadRequest)

	// 切换工作空间：非成员且非 admin 必须被拒。
	other, err := e.svc.Repo.CreateWorkspace(context.Background(), "team-x", "")
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	e.as("editor").fail("POST", "/api/v1/workspaces/"+other.ID+"/switch", nil, http.StatusForbidden)
	// 显式传入 user 时，策略模拟用传入的身份而不是当前身份。
	sim := obj(t, e.ok("GET", "/api/v1/policies/simulate?table=dwd_order&user=alice&role=viewer", nil))
	if field(t, sim, "user") != "alice" {
		t.Fatalf("策略模拟未使用显式 user: %v", sim)
	}
}

// 解码失败与权限门禁是两条最容易被忽略的分支：
// 前者决定「坏请求」是 400 还是 500，后者决定越权是 403 还是悄悄放行。
func TestDecodeAndRoleGateBranches(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	// 坏 JSON：每个写接口都必须给出 400，而不是 500。
	for _, path := range []string{
		"/api/v1/models", "/api/v1/models/preview", "/api/v1/models/check", "/api/v1/models/diff",
		"/api/v1/pipelines", "/api/v1/metrics", "/api/v1/dashboards", "/api/v1/sql/query",
		"/api/v1/pipelines/dry-run", "/api/v1/pipelines/sample-run", "/api/v1/instances/batch",
	} {
		e.fail("POST", path, "{", http.StatusBadRequest)
	}

	// viewer 的写操作一律 403。
	viewer := e.as("viewer")
	viewer.fail("DELETE", "/api/v1/datasources/"+ds, nil, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/datasources/"+ds+"/collect", nil, http.StatusForbidden)
	viewer.fail("PUT", "/api/v1/pipelines/dwd_order_daily/enabled", map[string]any{"enabled": true}, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/pipelines/dwd_order_daily/trigger", map[string]any{"bizDate": "2024-01-02"}, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/models/scaffold", map[string]any{"layer": "dwd", "name": "x"}, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/models/dwd_order/rollback", map[string]any{"version": 1}, http.StatusForbidden)

	// 自助查询的长 SQL：审计只保留前 200 字符，截断分支必须被执行到。
	longSQL := "SELECT 1 AS n /* " + strings.Repeat("x", 300) + " */"
	e.ok("POST", "/api/v1/sql/query", map[string]any{"datasource": ds, "sql": longSQL})

	// 永不命中的 cron：预览返回空列表而不是死循环。
	never := obj(t, e.ok("GET", "/api/v1/schedule/preview?cron=0%200%2031%202%20*", nil))
	if v := never["next"]; v != nil && len(list(t, v)) != 0 {
		t.Fatalf("不可能命中的 cron 不应给出未来时刻: %v", never)
	}
}
