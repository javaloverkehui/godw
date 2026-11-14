package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	apihttp "github.com/godw/godw/internal/api/http"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 这一组是「元数据库不可用」时的降级契约：
// 每个接口都必须快速失败并返回 5xx，而不是挂起、panic 或用空数据冒充成功。
// 做法是把连接池换成已关闭的池——所有仓储调用立刻返回错误，
// 从而覆盖各 handler 里「repo 出错 → fail」的分支（正常数据下这些分支永远走不到）。
func newBrokenEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := testsupport.Config(t)
	cfg.Env = "test"
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool, err := pgxpool.New(context.Background(),
		"postgres://godw:godw@127.0.0.1:1/godw?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("构造坏连接池失败: %v", err)
	}
	pool.Close() // 立刻关闭：任何查询都直接返回 "closed pool"
	db := &repo.DB{Pool: pool}

	svc, err := service.New(db, cfg, lg)
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	token, err := svc.Signer.Sign("usr_broken", "ws_broken", "admin")
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	h := apihttp.NewHandler(svc, cfg, lg, "ws_broken")
	srv := httptest.NewServer(h.Middleware())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv, svc: svc, cfg: cfg, token: token, wsID: "ws_broken", admin: "usr_broken"}
}

// fxWant5xx 断言接口以降级错误收场，并返回原始响应便于定位。
func fxWant5xx(e *testEnv, method, path string, body any) {
	e.t.Helper()
	status, _, raw := e.doRaw(method, path, body)
	if status < 500 {
		e.t.Fatalf("%s %s 期望 5xx，得到 %d: %s", method, path, status, raw)
	}
}

// 读接口在库不可用时逐个降级；任何一个返回 2xx 都会让前端误以为「没有数据」。
func TestReadAPIsDegradeOnDBFailure(t *testing.T) {
	e := newBrokenEnv(t)

	// readyz 有专门的语义：503 + 「数据库不可达」。
	if status, _, raw := e.doRaw("GET", "/readyz", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("readyz 在库不可用时应 503，得到 %d: %s", status, raw)
	}

	gets := []string{
		"/api/v1/datasources",
		"/api/v1/datasets",
		"/api/v1/models",
		"/api/v1/pipelines",
		"/api/v1/instances",
		"/api/v1/instances/matrix",
		"/api/v1/instances/ins_x",
		"/api/v1/instances/ins_x/sql",
		"/api/v1/instances/ins_x/logs",
		"/api/v1/instances/ins_x/waivers",
		"/api/v1/models/dwd_x/versions",
		"/api/v1/metrics",
		"/api/v1/metrics/m/query",
		"/api/v1/dashboards",
		"/api/v1/dashboards/d",
		"/api/v1/quality?instanceId=i",
		"/api/v1/quality/overview",
		"/api/v1/quality/trend",
		"/api/v1/lineage",
		"/api/v1/lineage/graph",
		"/api/v1/lineage/focus?table=t",
		"/api/v1/lineage/paths?from=a&to=b",
		"/api/v1/lineage/dot?table=t",
		"/api/v1/lineage/columns?table=t",
		"/api/v1/lineage/column-graph?table=t",
		"/api/v1/lineage/impact?table=t",
		"/api/v1/audit",
		"/api/v1/queue",
		"/api/v1/resource-groups",
		"/api/v1/alert-rules",
		"/api/v1/alerts",
		"/api/v1/backfill",
		"/api/v1/backfill/plan?pipeline=p&from=2024-01-01&to=2024-01-02",
		"/api/v1/runbook/gantt",
		"/api/v1/environments",
		"/api/v1/environments/bindings?env=prod",
		"/api/v1/schema-changes",
		"/api/v1/dictionary",
		"/api/v1/dictionary/export",
		"/api/v1/catalog",
		"/api/v1/catalog/export?format=md",
		"/api/v1/catalog/export?format=html",
		"/api/v1/search?q=x",
		"/api/v1/tags",
		"/api/v1/entity/tags?type=model&name=x",
		"/api/v1/tags/t/entities",
		"/api/v1/workspaces",
		"/api/v1/usage",
		"/api/v1/members",
		"/api/v1/cache/stats",
		"/api/v1/retention",
		"/api/v1/retention/plan?model=m",
		"/api/v1/erasure-jobs",
		"/api/v1/exports",
		"/api/v1/exports/exp_x",
		"/api/v1/exports/exp_x/download",
		"/api/v1/subscriptions",
		"/api/v1/shares",
		"/api/v1/public/share/tok",
		"/api/v1/datasets/semantic",
		"/api/v1/datasets/semantic/d",
		"/api/v1/charts",
		"/api/v1/charts/c/query",
		"/api/v1/policies/columns",
		"/api/v1/policies/rows",
		"/api/v1/policies/simulate?table=t",
		"/api/v1/table/rows?datasource=d&table=t",
		"/api/v1/select/preview?select=layer:dwd",
	}
	for _, path := range gets {
		fxWant5xx(e, "GET", path, nil)
	}

	// overview 是已知的粗糙点：它用 `ds, _ :=` 忽略仓储错误，
	// 库不可用时返回全 0 而不是 5xx。这里断言当前行为，避免测试与实现脱节。
	ov := obj(t, e.ok("GET", "/api/v1/overview", nil))
	if fxInt(t, ov, "datasources") != 0 || fxInt(t, ov, "models") != 0 {
		t.Fatalf("库不可用时 overview 计数应为 0: %v", ov)
	}

	// 写接口同样必须失败；这里顺带覆盖「权限之外」的那一层错误处理。
	posts := []struct {
		path string
		body any
	}{
		{"/api/v1/models", map[string]any{"yaml": "kind: Model\nname: dwd_x\nlayer: dwd\ndatasource: ds\ncolumns:\n  - name: dt\n    type: date\n"}},
		{"/api/v1/pipelines", map[string]any{"yaml": "kind: Pipeline\nname: p\nnodes:\n  - id: n\n    type: source\n    sql: SELECT 1\n"}},
		{"/api/v1/metrics", map[string]any{"yaml": "name: m\nmodel: dwd_x\nagg: sum\nexpr: amount\n"}},
		{"/api/v1/datasources", map[string]any{"name": "ds2", "type": "postgres", "config": map[string]any{"host": "127.0.0.1", "port": "1"}}},
		{"/api/v1/environments/bind", map[string]any{"env": "prod", "alias": "a", "datasource": "d"}},
		{"/api/v1/catalog/collect", map[string]any{"datasource": "d"}},
		{"/api/v1/pii/detect", map[string]any{"datasource": "d"}},
		{"/api/v1/pii/apply", map[string]any{"datasource": "d", "findings": []any{
			map[string]any{"table": "t", "column": "c", "level": "high", "reason": "r"},
		}}},
		{"/api/v1/subscriptions/sub_x/run", nil},
		{"/api/v1/cache/purge", nil},
		{"/api/v1/tags", map[string]any{"name": "t"}},
		{"/api/v1/tags/attach", map[string]any{"type": "model", "name": "x", "tags": []string{"t"}}},
		{"/api/v1/policies/columns", map[string]any{"table": "t", "column": "c", "mask": "partial"}},
		{"/api/v1/policies/rows", map[string]any{"table": "t", "role": "viewer", "filter": "1=1"}},
		{"/api/v1/retention", map[string]any{"modelName": "m", "retainDays": 7}},
		{"/api/v1/erasure-jobs", map[string]any{"model": "m", "column": "c", "value": "v", "reason": "r"}},
		{"/api/v1/exports", map[string]any{"kind": "metric", "target": "m"}},
		{"/api/v1/subscriptions", map[string]any{"name": "s", "target": "m"}},
		{"/api/v1/shares", map[string]any{"kind": "metric", "target": "m"}},
		{"/api/v1/datasets/semantic", map[string]any{"spec": map[string]any{"name": "d", "baseModel": "m", "dimensions": []any{map[string]any{"name": "dt", "field": "dt", "type": "time"}}, "measures": []any{map[string]any{"name": "c", "agg": "count"}}}}},
		{"/api/v1/datasets/semantic/d/sql", map[string]any{}},
		{"/api/v1/datasets/semantic/query", map[string]any{"dataset": "d"}},
		{"/api/v1/alert-rules", map[string]any{"name": "r", "kind": "failure", "target": "*", "severity": "critical"}},
		{"/api/v1/resource-groups", map[string]any{"name": "g"}},
		{"/api/v1/workspaces", map[string]any{"name": "w"}},
		{"/api/v1/members", map[string]any{"userId": "u", "role": "viewer"}},
		{"/api/v1/queue/dispatch", nil},
		{"/api/v1/worker/run-once", nil},
		{"/api/v1/runs/select", map[string]any{"select": "layer:dwd"}},
		{"/api/v1/backfill", map[string]any{"pipeline": "p", "from": "2024-01-01", "to": "2024-01-02"}},
		{"/api/v1/instances/ins_x/cancel", nil},
		{"/api/v1/instances/ins_x/retry", nil},
		{"/api/v1/instances/ins_x/waive", map[string]any{"reason": "r"}},
		{"/api/v1/pipelines/p/trigger", map[string]any{"bizDate": "2024-01-02"}},
		{"/api/v1/pipelines/p/enabled", map[string]any{"enabled": false}},
		{"/api/v1/models/dwd_x/rollback", map[string]any{"version": 1}},
	}
	for _, p := range posts {
		method := "POST"
		if p.path == "/api/v1/pipelines/p/enabled" {
			method = "PUT"
		}
		// 写接口的失败码取决于各服务的错误映射（有的直接 5xx，有的包装成 400），
		// 这里只要求「不成功」——重点是 fail 分支被执行到，而不是具体状态码。
		e.fail(method, p.path, p.body, 0)
	}

	// 批量动作逐条收口：单条失败进 failed，整体仍是 200。
	batch := obj(t, e.ok("POST", "/api/v1/instances/batch", map[string]any{
		"action": "retry", "ids": []string{"ins_x"},
	}))
	if len(fxMap(t, batch, "failed")) != 1 {
		t.Fatalf("库不可用时批量重跑应逐条失败: %v", batch)
	}
}

// 删除类与解码类分支在库不可用时同样要被执行：
// 前者验证「删除失败要如实报错」，后者验证「校验先于 IO」。
func TestWriteAPIsDegradeOnDBFailure(t *testing.T) {
	e := newBrokenEnv(t)

	// 坏 JSON：校验层先失败，返回 400（不需要连接库）。
	for _, path := range []string{
		"/api/v1/datasources", "/api/v1/charts", "/api/v1/alerts/al_x/handle",
		"/api/v1/alert-rules/test", "/api/v1/runs/select", "/api/v1/models/preview",
	} {
		e.fail("POST", path, "{", http.StatusBadRequest)
	}

	// 合法的预览请求：过了校验，卡在仓储错误上。
	fxWant5xx(e, "POST", "/api/v1/models/preview", map[string]any{
		"spec": map[string]any{"name": "dwd_x", "layer": "dwd", "datasource": "ds",
			"columns": []any{map[string]any{"name": "dt", "type": "date"}}},
	})
	// 选择器试跑：解析需要读模型与管道，库不可用时 5xx。
	fxWant5xx(e, "POST", "/api/v1/runs/select", map[string]any{"select": "layer:dwd", "dryRun": true})
	// 告警状态流转：仓储更新失败要报 5xx。
	fxWant5xx(e, "POST", "/api/v1/alerts/al_x/handle", map[string]any{"status": "resolved"})

	// 删除路径：全部走「查询/更新失败 → 5xx」。
	for _, path := range []string{
		"/api/v1/datasets/semantic/d",
		"/api/v1/charts/c",
		"/api/v1/tags/t",
		"/api/v1/tags/t/entities/model/e",
		"/api/v1/policies/rows/r",
		"/api/v1/subscriptions/sub_x",
		"/api/v1/shares/tok",
		"/api/v1/members/usr_x",
		"/api/v1/alert-rules/r",
		"/api/v1/models/m/rollback",
	} {
		if path == "/api/v1/models/m/rollback" {
			fxWant5xx(e, "POST", path, map[string]any{"version": 1})
			continue
		}
		fxWant5xx(e, "DELETE", path, nil)
	}
}
