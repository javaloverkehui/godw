package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/godw/godw/internal/semantic"
)

// 写接口的角色门禁分布在各个 handler 里，靠一个 requireRole 兜住。
// 这里逐条确认「读权限足够、写权限不足」的角色被挡在外面，
// 而不是让请求走到一半才发现越权。
func TestWriteEndpointRoleGates(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	e.ok("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: order_amount
model: dwd_order
agg: sum
expr: amount
timeColumn: dt
`})

	editor := e.as("editor")
	viewer := e.as("viewer")

	// 行策略是脱敏边界，只允许 admin 改。
	editor.fail("POST", "/api/v1/policies/rows", map[string]any{
		"table": "dwd_order", "role": "viewer", "filter": "status = 'paid'",
	}, http.StatusForbidden)
	// 驳回导出意味着替审批人做决定，同样只允许 admin。
	editor.fail("POST", "/api/v1/exports/exp_ghost/reject", map[string]any{"reason": "x"}, http.StatusForbidden)
	// 成员管理是租户级权限。
	editor.fail("DELETE", "/api/v1/members/usr_a", nil, http.StatusForbidden)

	// 告警规则的增删与试算对 editor 开放、viewer 关闭。
	viewer.fail("DELETE", "/api/v1/alert-rules/ghost", nil, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/alert-rules/test", map[string]any{
		"rule": map[string]any{"name": "r", "kind": "failure", "target": "*", "enabled": true},
		"fact": map[string]any{"status": "failed"},
	}, http.StatusForbidden)

	// 采集会产生新的元数据与变更单，viewer 不能触发。
	viewer.fail("POST", "/api/v1/catalog/collect", map[string]any{"datasource": ds}, http.StatusForbidden)

	// 语义层的删除与图表保存同样是写操作。
	viewer.fail("DELETE", "/api/v1/datasets/semantic/whatever", nil, http.StatusForbidden)
	viewer.fail("POST", "/api/v1/charts", map[string]any{"yaml": "name: c"}, http.StatusForbidden)
	viewer.fail("DELETE", "/api/v1/charts/whatever", nil, http.StatusForbidden)
}

// 坏 JSON 必须在任何 IO 之前被拒：这些 handler 的 decode 分支
// 一旦被漏掉，前端发错格式只会拿到 500，排查成本高得多。
func TestDecodeErrorsAreClientErrors(t *testing.T) {
	e := newEnv(t)

	cases := []struct{ path, body string }{
		{"/api/v1/resource-groups", "{"},
		{"/api/v1/retention/run", "{"},
		{"/api/v1/erasure-jobs", "{"},
		{"/api/v1/backfill", "{"},
		{"/api/v1/environments/bind", "{"},
	}
	for _, c := range cases {
		if msg := e.fail("POST", c.path, c.body, http.StatusBadRequest); msg == "" {
			t.Fatalf("%s 的坏 JSON 应带错误信息", c.path)
		}
	}

	// YAML 解析失败同样是 400，并且错误里要指出是哪一类定义。
	msg := e.fail("POST", "/api/v1/charts", map[string]any{"yaml": "type: ["}, http.StatusBadRequest)
	if !strings.Contains(msg, "图表") {
		t.Fatalf("图表 YAML 错误信息不对: %q", msg)
	}
	e.fail("POST", "/api/v1/pipelines/dry-run", map[string]any{"yaml": "nodes: ["}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/pipelines/sample-run", map[string]any{"yaml": "nodes: ["}, http.StatusBadRequest)
}

// 回滚预览必须显式给版本号：缺省时不能默默按「当前版本」比，
// 那样用户会看到一份「没有差异」的假报告。
func TestRollbackPreviewRequiresVersion(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	msg := e.fail("GET", "/api/v1/models/dwd_order/rollback-preview", nil, http.StatusBadRequest)
	if !strings.Contains(msg, "version") {
		t.Fatalf("应提示 version 必填: %q", msg)
	}
	e.fail("GET", "/api/v1/models/dwd_order/rollback-preview?version=abc", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/models/dwd_order/rollback-preview?version=0", nil, http.StatusBadRequest)
}

// 保留策略执行：解码失败与未知模型都要明确报错，绝不能「删了 0 行当成功」。
func TestRetentionRunRejectsBadInput(t *testing.T) {
	e := newEnv(t)

	e.fail("POST", "/api/v1/retention/run", map[string]any{"model": ""}, 0)
	e.fail("POST", "/api/v1/retention/run", map[string]any{"model": "ghost_model"}, 0)
	// 只有 admin 能执行删除。
	e.as("editor").fail("POST", "/api/v1/retention/run", map[string]any{"model": "ghost_model"}, http.StatusForbidden)
}

// 告警规则试算允许省略管道：缺省时用一条演示管道补齐，
// 否则「只想验证表达式对不对」也得先造一条真实运行。
func TestAlertRuleTestFillsDefaultPipeline(t *testing.T) {
	e := newEnv(t)

	tested := obj(t, e.ok("POST", "/api/v1/alert-rules/test", map[string]any{
		"rule": map[string]any{"name": "f", "kind": "failure", "target": "*", "severity": "critical", "enabled": true},
		"fact": map[string]any{"status": "failed"},
	}))
	if !fxBool(t, tested, "triggered") {
		t.Fatalf("失败事实应触发规则: %v", tested)
	}
	ev := fxMap(t, tested, "event")
	if field(t, ev, "pipeline") != "demo_pipeline" {
		t.Fatalf("未给管道时应补默认值: %v", ev)
	}
}

// 公开分享是「凭 token 读」的入口：token 有效但目标已被删除时，
// 必须返回明确错误，而不是 200 + 空数据。
func TestPublicShareTargetMissing(t *testing.T) {
	e := newEnv(t)

	dash := obj(t, e.ok("POST", "/api/v1/shares", map[string]any{"kind": "dashboard", "target": "ghost_dash"}))
	e.fail("GET", "/api/v1/public/share/"+field(t, dash, "token"), nil, http.StatusNotFound)

	metric := obj(t, e.ok("POST", "/api/v1/shares", map[string]any{"kind": "metric", "target": "ghost_metric"}))
	e.fail("GET", "/api/v1/public/share/"+field(t, metric, "token"), nil, http.StatusNotFound)
}

// 数据集编译成 SQL 时会依次校验：数据集存在 → 基础模型已发布 → 数据源可用 → 请求合法。
// 四层任一失败都要有各自的错误，否则「查不出数」时没法判断卡在哪一步。
func TestDatasetSQLFailureLayers(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	// 1) 请求里没给数据集、路径上也没有：直接拒。
	e.fail("POST", "/api/v1/datasets/semantic/query", map[string]any{}, http.StatusNotFound)

	// 建一个正常数据集（sales 引用已发布的 dwd_order）。
	e.ok("POST", "/api/v1/datasets/semantic", map[string]any{"spec": map[string]any{
		"name": "sales", "baseModel": "dwd_order",
		"dimensions": []any{map[string]any{"name": "status", "field": "status", "type": "categorical"}},
		"measures":   []any{map[string]any{"name": "amount_sum", "field": "amount", "agg": "sum"}},
	}})

	// 2) 请求本身非法（未声明的维度）：编译阶段失败。
	e.fail("POST", "/api/v1/datasets/semantic/sales/sql", map[string]any{
		"dimensions": []string{"ghost_dim"},
	}, http.StatusBadRequest)

	// 3) 基础模型未发布：直接落一条指向幽灵模型的数据集，跳过定义期校验。
	if _, err := e.svc.Repo.SaveDataset(context.Background(), e.wsID, semantic.Dataset{
		Name: "orphan", BaseModel: "ghost_model",
	}); err != nil {
		t.Fatalf("写入孤儿数据集失败: %v", err)
	}
	msg := e.fail("POST", "/api/v1/datasets/semantic/orphan/sql", map[string]any{}, http.StatusNotFound)
	if !strings.Contains(msg, "未发布") {
		t.Fatalf("应提示基础模型未发布: %q", msg)
	}

	// 4) 数据源被改名（等价于「被删掉」）：模型还在，连接器却拿不到。
	//    直接改库而不是 DELETE，是为了绕开外键级联把模型一起带走。
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`UPDATE datasource SET name=$1 WHERE workspace_id=$2 AND name=$3`,
		"pgmain_renamed", e.wsID, ds); err != nil {
		t.Fatalf("重命名数据源失败: %v", err)
	}
	e.fail("POST", "/api/v1/datasets/semantic/sales/sql", map[string]any{}, 0)
}

// 数据集删除失败要如实报错；这里用「图表仍引用」之外的场景覆盖仓储错误：
// 库不可用时由 resilience 用例负责，本用例只锁定「有引用 → 409」的语义边界。
func TestDatasetDeleteBlockedByChart(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	e.ok("POST", "/api/v1/datasets/semantic", map[string]any{"spec": map[string]any{
		"name": "sales", "baseModel": "dwd_order",
		"dimensions": []any{map[string]any{"name": "status", "field": "status", "type": "categorical"}},
		"measures":   []any{map[string]any{"name": "amount_sum", "field": "amount", "agg": "sum"}},
	}})
	e.ok("POST", "/api/v1/charts", map[string]any{"spec": map[string]any{
		"name": "c1", "type": "bar", "dataset": "sales",
		"dimensions": []string{"status"}, "measures": []string{"amount_sum"},
		"encoding": map[string]any{"x": "status", "y": "amount_sum"},
	}})

	msg := e.fail("DELETE", "/api/v1/datasets/semantic/sales", nil, http.StatusConflict)
	if !strings.Contains(msg, "图表") {
		t.Fatalf("应提示图表引用: %q", msg)
	}
}
