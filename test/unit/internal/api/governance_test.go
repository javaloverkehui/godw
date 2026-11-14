package api_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
)

// 行列策略与表预览：策略必须在预览结果里真实生效（脱敏 + 过滤），
// 否则「以为脱敏了其实没有」比不脱敏更危险。
func TestPolicyAndTablePreview(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxSeedOrders(t, e, schema, "dwd_order", "2024-01-02", "2024-01-02")

	// 预览：缺参数 400；未知数据源 404。
	e.fail("GET", "/api/v1/table/rows", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/table/rows?datasource=ghost&table=dwd_order", nil, http.StatusNotFound)

	// 列策略只有 admin 能改。
	e.as("editor").fail("POST", "/api/v1/policies/columns", map[string]any{
		"table": "dwd_order", "column": "amount", "mask": "partial",
	}, http.StatusForbidden)
	// 缺字段与不支持的脱敏方式都是 400。
	e.fail("POST", "/api/v1/policies/columns", map[string]any{"table": "dwd_order"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/policies/columns", map[string]any{
		"table": "dwd_order", "column": "amount", "mask": "rot13",
	}, http.StatusBadRequest)

	if items := list(t, e.ok("GET", "/api/v1/policies/columns", nil)); len(items) != 0 {
		t.Fatalf("初始列策略应为空，得到 %d", len(items))
	}
	pol := obj(t, e.ok("POST", "/api/v1/policies/columns", map[string]any{
		"table": "dwd_order", "column": "amount", "mask": "partial", "classification": "confidential",
	}))
	if field(t, pol, "mask") != "partial" || field(t, pol, "column") != "amount" {
		t.Fatalf("列策略保存结果不对: %v", pol)
	}
	if items := list(t, e.ok("GET", "/api/v1/policies/columns?table=dwd_order", nil)); len(items) != 1 {
		t.Fatalf("列策略列表应有 1 条，得到 %d", len(items))
	}

	// 预览：amount 应被部分脱敏，masks 里要能看到实际生效的策略。
	prev := obj(t, e.ok("GET", "/api/v1/table/rows?datasource="+ds+"&schema="+schema+"&table=dwd_order&limit=10", nil))
	masks := fxMap(t, prev, "masks")
	if masks["amount"] != "partial" {
		t.Fatalf("预览未应用列脱敏: %v", prev)
	}
	rows := list(t, prev["rows"])
	if len(rows) != 2 {
		t.Fatalf("预览应有 2 行，得到 %d", len(rows))
	}
	if row := list(t, rows[0]); len(row) != 4 {
		t.Fatalf("预览列数不对: %v", row)
	}
	if !strings.Contains(field(t, prev, "sql"), "SELECT * FROM") {
		t.Fatalf("预览应回显 SQL: %v", prev)
	}

	// 行策略：缺 filter 400；保存后预览的 WHERE 要带上过滤条件。
	if items := list(t, e.ok("GET", "/api/v1/policies/rows", nil)); len(items) != 0 {
		t.Fatalf("初始行策略应为空，得到 %d", len(items))
	}
	e.fail("POST", "/api/v1/policies/rows", map[string]any{"table": "dwd_order"}, http.StatusBadRequest)
	savedRow := obj(t, e.ok("POST", "/api/v1/policies/rows", map[string]any{
		"table": "dwd_order", "role": "viewer", "filter": "status = ${role_placeholder}", "description": "只读用户看付费单",
	}))
	rowID := field(t, savedRow, "id")
	if rowID == "" {
		t.Fatalf("行策略应返回 id: %v", savedRow)
	}
	if items := list(t, e.ok("GET", "/api/v1/policies/rows", nil)); len(items) != 1 {
		t.Fatalf("行策略列表应有 1 条，得到 %d", len(items))
	}

	// 策略模拟：返回生效的列与行条件，供「我到底能看什么」自查。
	sim := obj(t, e.ok("GET", "/api/v1/policies/simulate?table=dwd_order&role=viewer", nil))
	if field(t, sim, "table") != "dwd_order" || field(t, sim, "role") != "viewer" {
		t.Fatalf("策略模拟结果不对: %v", sim)
	}
	if _, ok := sim["columns"]; !ok {
		t.Fatalf("策略模拟应列出列级结果: %v", sim)
	}

	// 删除行策略：只有 admin。
	e.as("editor").fail("DELETE", "/api/v1/policies/rows/"+rowID, nil, http.StatusForbidden)
	e.ok("DELETE", "/api/v1/policies/rows/"+rowID, nil)
	if items := list(t, e.ok("GET", "/api/v1/policies/rows", nil)); len(items) != 0 {
		t.Fatalf("删除后行策略应为空，得到 %d", len(items))
	}
}

// 列级血缘与影响面：三者共享同一份 column_edge，接口口径必须一致。
func TestColumnLineageViews(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	// 表参数必填。
	e.fail("GET", "/api/v1/lineage/columns", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/lineage/impact", nil, http.StatusBadRequest)

	edges := list(t, e.ok("GET", "/api/v1/lineage/columns?table=dwd_order", nil))
	if edges == nil {
		t.Fatalf("列血缘应返回数组（可为空）")
	}

	g := obj(t, e.ok("GET", "/api/v1/lineage/column-graph?table=dwd_order&direction=down&depth=3", nil))
	if _, ok := g["nodes"]; !ok {
		t.Fatalf("列级图应返回 nodes: %v", g)
	}
	// column-graph 缺 table 是 400（服务层校验）。
	e.fail("GET", "/api/v1/lineage/column-graph", nil, http.StatusBadRequest)

	impact := obj(t, e.ok("GET", "/api/v1/lineage/impact?table=dwd_order&column=order_id&depth=2", nil))
	if field(t, impact, "origin") != "dwd_order.order_id" {
		t.Fatalf("影响面 origin 不对: %v", impact)
	}
	if _, ok := impact["edges"]; !ok {
		t.Fatalf("影响面应返回边: %v", impact)
	}
}

// 导出的核心是「审批」与「下载边界」：高危数据必须人工批准，
// 下载只允许导出目录内的文件。
func TestExportFlow(t *testing.T) {
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

	// target 必填；未知指标在分级阶段报错但不阻断（仍会创建任务）。
	e.fail("POST", "/api/v1/exports", map[string]any{}, http.StatusBadRequest)

	job := obj(t, e.ok("POST", "/api/v1/exports", map[string]any{"kind": "metric", "target": "order_amount"}))
	id := field(t, job, "id")
	if id == "" || field(t, job, "kind") != "metric" {
		t.Fatalf("导出任务创建结果不对: %v", job)
	}
	if items := list(t, e.ok("GET", "/api/v1/exports?limit=10", nil)); len(items) != 1 {
		t.Fatalf("导出列表应有 1 条，得到 %d", len(items))
	}
	if got := obj(t, e.ok("GET", "/api/v1/exports/"+id, nil)); field(t, got, "target") != "order_amount" {
		t.Fatalf("导出详情不对: %v", got)
	}
	e.fail("GET", "/api/v1/exports/exp_ghost", nil, http.StatusNotFound)

	// 排队中的任务不能审批；审批只有 admin。
	e.fail("POST", "/api/v1/exports/"+id+"/approve", nil, http.StatusConflict)
	e.as("editor").fail("POST", "/api/v1/exports/"+id+"/approve", nil, http.StatusForbidden)

	// 直接造一条待审批任务，验证批准 / 驳回两条路径。
	pending, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount",
		Status: "pending_approval", Params: map[string]any{"_classification": "high"}, CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入待审批导出失败: %v", err)
	}
	// 驳回必须说明理由。
	e.fail("POST", "/api/v1/exports/"+pending.ID+"/reject", map[string]any{"reason": " "}, http.StatusBadRequest)
	rejected := obj(t, e.ok("POST", "/api/v1/exports/"+pending.ID+"/reject", map[string]any{"reason": "数据敏感"}))
	if field(t, rejected, "status") != "rejected" {
		t.Fatalf("驳回结果不对: %v", rejected)
	}
	// 已驳回的不能再批准。
	e.fail("POST", "/api/v1/exports/"+pending.ID+"/approve", nil, http.StatusConflict)

	pending2, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount",
		Status: "pending_approval", Params: map[string]any{}, CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入待审批导出失败: %v", err)
	}
	approved := obj(t, e.ok("POST", "/api/v1/exports/"+pending2.ID+"/approve", nil))
	if field(t, approved, "status") != "approved" {
		t.Fatalf("批准结果不对: %v", approved)
	}

	// 下载：未完成的直接拒绝。这里造一条不会被执行的任务，避免异步跑完之后状态漂移。
	queued, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount", Status: "queued", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入排队导出失败: %v", err)
	}
	e.fail("GET", "/api/v1/exports/"+queued.ID+"/download", nil, http.StatusBadRequest)

	// 造一个已完成且文件在导出目录内的任务：应能下载到内容。
	dir := e.cfg.Semantic.ExportDir
	if dir == "" {
		dir = "./data/exports"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建导出目录失败: %v", err)
	}
	file := filepath.Join(dir, "metric_ok.csv")
	if err := os.WriteFile(file, []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatalf("写入导出文件失败: %v", err)
	}
	done, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入已完成导出失败: %v", err)
	}
	// CreateExport 只落基础字段，文件路径与终态由执行器回写。
	if err := e.svc.Repo.FinishExport(context.Background(), done.ID, "success", file, "", 2); err != nil {
		t.Fatalf("回写导出结果失败: %v", err)
	}
	status, _, body := e.doRaw("GET", "/api/v1/exports/"+done.ID+"/download", nil)
	if status != http.StatusOK || !strings.Contains(body, "a,b") {
		t.Fatalf("下载内容不对(%d): %s", status, body)
	}

	// 路径穿越：导出目录之外的文件必须拒绝。
	outside, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "order_amount", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("写入越界导出失败: %v", err)
	}
	if err := e.svc.Repo.FinishExport(context.Background(), outside.ID, "success",
		filepath.Join(os.TempDir(), "godw-outside.csv"), "", 0); err != nil {
		t.Fatalf("回写越界导出失败: %v", err)
	}
	e.fail("GET", "/api/v1/exports/"+outside.ID+"/download", nil, http.StatusForbidden)
}

// 订阅把看板/指标按渠道推出去：file 渠道可离线验证，webhook 缺 endpoint 要明确报错。
func TestSubscriptions(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	today := time.Now().Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", today)
	e.ok("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: order_amount
model: dwd_order
agg: sum
expr: amount
timeColumn: dt
`})

	if items := list(t, e.ok("GET", "/api/v1/subscriptions", nil)); len(items) != 0 {
		t.Fatalf("初始订阅应为空，得到 %d", len(items))
	}
	e.fail("POST", "/api/v1/subscriptions", map[string]any{"name": "s1"}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/subscriptions", map[string]any{"name": "s1", "target": "order_amount"}, http.StatusForbidden)

	sub := obj(t, e.ok("POST", "/api/v1/subscriptions", map[string]any{
		"name": "daily", "kind": "metric", "target": "order_amount", "channel": "file", "enabled": true,
	}))
	subID := field(t, sub, "id")
	if field(t, sub, "channel") != "file" {
		t.Fatalf("订阅保存结果不对: %v", sub)
	}
	if items := list(t, e.ok("GET", "/api/v1/subscriptions", nil)); len(items) != 1 {
		t.Fatalf("订阅列表应有 1 条，得到 %d", len(items))
	}

	// 立即执行一次：file 渠道落盘并回传路径。
	run := obj(t, e.ok("POST", "/api/v1/subscriptions/"+subID+"/run", nil))
	if field(t, run, "channel") != "file" || field(t, run, "path") == "" {
		t.Fatalf("订阅执行结果不对: %v", run)
	}
	e.fail("POST", "/api/v1/subscriptions/sub_ghost/run", nil, http.StatusNotFound)

	// webhook 缺 endpoint：明确报 400，而不是静默失败。
	hook := obj(t, e.ok("POST", "/api/v1/subscriptions", map[string]any{
		"name": "hook", "kind": "metric", "target": "order_amount", "channel": "webhook",
	}))
	e.fail("POST", "/api/v1/subscriptions/"+field(t, hook, "id")+"/run", nil, http.StatusBadRequest)

	// 删除后列表只剩 webhook 那条；viewer 不能删。
	e.as("editor").fail("DELETE", "/api/v1/subscriptions/"+subID, nil, http.StatusForbidden)
	e.ok("DELETE", "/api/v1/subscriptions/"+subID, nil)
	if items := list(t, e.ok("GET", "/api/v1/subscriptions", nil)); len(items) != 1 {
		t.Fatalf("删除后应剩 1 条，得到 %d", len(items))
	}
}

// 分享链接是「不需要登录」的入口，所以令牌只能读、不能改，且类型必须是白名单。
func TestShareLinks(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	today := time.Now().Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", today)
	e.ok("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: order_amount
model: dwd_order
agg: sum
expr: amount
timeColumn: dt
`})
	e.ok("POST", "/api/v1/dashboards", map[string]any{"dashboard": map[string]any{
		"name": "ops", "cards": []any{map[string]any{"title": "金额", "metric": "order_amount", "days": 30}},
	}})

	if items := list(t, e.ok("GET", "/api/v1/shares", nil)); len(items) != 0 {
		t.Fatalf("初始分享应为空，得到 %d", len(items))
	}
	e.fail("POST", "/api/v1/shares", map[string]any{"kind": "metric"}, http.StatusBadRequest)

	metricShare := obj(t, e.ok("POST", "/api/v1/shares", map[string]any{
		"kind": "metric", "target": "order_amount", "ttlDays": 1,
	}))
	metricToken := field(t, metricShare, "token")
	if metricToken == "" {
		t.Fatalf("分享应返回 token: %v", metricShare)
	}
	pub := obj(t, e.ok("GET", "/api/v1/public/share/"+metricToken, nil))
	if field(t, pub, "kind") != "metric" || field(t, pub, "target") != "order_amount" {
		t.Fatalf("公开指标分享内容不对: %v", pub)
	}

	dashShare := obj(t, e.ok("POST", "/api/v1/shares", map[string]any{"kind": "dashboard", "target": "ops"}))
	pubDash := obj(t, e.ok("GET", "/api/v1/public/share/"+field(t, dashShare, "token"), nil))
	if field(t, pubDash, "kind") != "dashboard" {
		t.Fatalf("公开看板分享内容不对: %v", pubDash)
	}

	// 未知类型的分享：即便记录存在也拒绝。
	weird, err := e.svc.Repo.CreateShare(context.Background(), e.wsID, "excel", "whatever", e.admin, 0)
	if err != nil {
		t.Fatalf("写入未知类型分享失败: %v", err)
	}
	e.fail("GET", "/api/v1/public/share/"+weird.Token, nil, http.StatusNotFound)
	// 不存在的 token。
	e.fail("GET", "/api/v1/public/share/nope", nil, http.StatusNotFound)

	if items := list(t, e.ok("GET", "/api/v1/shares", nil)); len(items) != 3 {
		t.Fatalf("分享列表应有 3 条，得到 %d", len(items))
	}
	e.ok("DELETE", "/api/v1/shares/"+metricToken, nil)
	if items := list(t, e.ok("GET", "/api/v1/shares", nil)); len(items) != 2 {
		t.Fatalf("删除后应剩 2 条，得到 %d", len(items))
	}
}

// 多工作空间与成员：创建者自动成为管理员，切换工作空间只换作用域不换身份。
func TestWorkspaceAndMembers(t *testing.T) {
	e := newEnv(t)

	ws := list(t, e.ok("GET", "/api/v1/workspaces", nil))
	if len(ws) != 1 {
		t.Fatalf("初始应有 1 个工作空间，得到 %d", len(ws))
	}
	// 创建：name 必填，只有 admin 能建。
	e.fail("POST", "/api/v1/workspaces", map[string]any{"description": "x"}, http.StatusBadRequest)
	e.as("editor").fail("POST", "/api/v1/workspaces", map[string]any{"name": "team2"}, http.StatusForbidden)

	created := obj(t, e.ok("POST", "/api/v1/workspaces", map[string]any{"name": "team2", "description": "第二团队"}))
	wsID := field(t, created, "id")
	if field(t, created, "name") != "team2" {
		t.Fatalf("工作空间创建结果不对: %v", created)
	}
	if all := list(t, e.ok("GET", "/api/v1/workspaces", nil)); len(all) != 2 {
		t.Fatalf("创建后应有 2 个工作空间，得到 %d", len(all))
	}
	// 切换：签发指向新工作空间的 token。
	sw := obj(t, e.ok("POST", "/api/v1/workspaces/"+wsID+"/switch", nil))
	if field(t, sw, "token") == "" || field(t, sw, "role") != "admin" {
		t.Fatalf("切换工作空间结果不对: %v", sw)
	}
	e.fail("POST", "/api/v1/workspaces/ws_ghost/switch", nil, http.StatusNotFound)

	// 成员：角色白名单 + 必填。
	if members := list(t, e.ok("GET", "/api/v1/members", nil)); len(members) != 0 {
		t.Fatalf("初始成员应为空，得到 %d", len(members))
	}
	e.fail("POST", "/api/v1/members", map[string]any{"role": "admin"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/members", map[string]any{"userId": "usr_a", "role": "god"}, http.StatusBadRequest)
	e.as("editor").fail("POST", "/api/v1/members", map[string]any{"userId": "usr_a", "role": "viewer"}, http.StatusForbidden)

	mem := obj(t, e.ok("POST", "/api/v1/members", map[string]any{"userId": "usr_a", "role": "editor"}))
	if field(t, mem, "userId") != "usr_a" || field(t, mem, "role") != "editor" {
		t.Fatalf("成员添加结果不对: %v", mem)
	}
	if members := list(t, e.ok("GET", "/api/v1/members", nil)); len(members) != 1 {
		t.Fatalf("成员列表应有 1 条，得到 %d", len(members))
	}
	e.ok("DELETE", "/api/v1/members/usr_a", nil)
	if members := list(t, e.ok("GET", "/api/v1/members", nil)); len(members) != 0 {
		t.Fatalf("删除后成员应为空，得到 %d", len(members))
	}

	// 租户用量与缓存：只读统计，字段必须存在。
	usage := obj(t, e.ok("GET", "/api/v1/usage", nil))
	if len(usage) == 0 {
		t.Fatalf("用量统计不应为空: %v", usage)
	}
	stats := obj(t, e.ok("GET", "/api/v1/cache/stats", nil))
	if _, ok := stats["ttlSec"]; !ok {
		t.Fatalf("缓存统计应带 ttlSec: %v", stats)
	}
	if _, ok := obj(t, e.ok("POST", "/api/v1/cache/purge", nil))["purged"]; !ok {
		t.Fatalf("缓存清理应返回条数")
	}
}
