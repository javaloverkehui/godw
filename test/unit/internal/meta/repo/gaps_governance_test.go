package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 本文件覆盖治理面：列级血缘、列/行策略、导出、订阅、分享、成员、工作区列表。
// 这些接口的共同风险是「读到一半失败」：半份策略会让越权查询跑出去，
// 所以扫描/第二条语句失败都必须原样抛给上层。

// 替换列级血缘时先删后插；删除失败必须中止，否则新旧血缘会混在一起，
// 影响分析会算出一张表依赖它自己。
func TestReplaceColumnLineageFailsWhenDeleteRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	// 先落一条边：行级触发器只在真的删到行时才触发。
	if err := r.ReplaceColumnLineage(context.Background(), ws.ID, "dwd_order",
		[]model.ColumnEdge{{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"}}); err != nil {
		t.Fatalf("写入列级血缘失败: %v", err)
	}
	failOn(t, db, "lineage_column", "DELETE", "")

	err := r.ReplaceColumnLineage(context.Background(), ws.ID, "dwd_order",
		[]model.ColumnEdge{{UpstreamTable: "ods_order", UpstreamColumn: "amount", DownstreamColumn: "amount"}})
	wantErr(t, err, "注入故障: lineage_column delete")

	if n := queryInt(t, db, `SELECT count(*) FROM lineage_column`); n != 1 {
		t.Fatalf("删除失败时旧血缘必须原样保留，实际 %d 行", n)
	}
}

// 列级血缘的 node_id 为 NULL 时必须报错：脱敏规则靠它定位到执行节点。
func TestColumnLineageFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.ReplaceColumnLineage(context.Background(), ws.ID, "dwd_order",
		[]model.ColumnEdge{{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id", NodeID: "n1"}}); err != nil {
		t.Fatalf("写入列级血缘失败: %v", err)
	}
	nullify(t, db, "lineage_column", "node_id")

	_, err := r.ColumnLineage(context.Background(), ws.ID, "dwd_order", "")
	wantErr(t, err, "NULL")
}

// 影响分析的递归结果里 node_id 为 NULL 时必须报错，不能算出一批「不知道来源」的影响表。
func TestImpactAnalysisFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.ReplaceColumnLineage(context.Background(), ws.ID, "dws_gmv",
		[]model.ColumnEdge{{UpstreamTable: "dwd_order", UpstreamColumn: "amount", DownstreamColumn: "gmv", NodeID: "n1"}}); err != nil {
		t.Fatalf("写入列级血缘失败: %v", err)
	}
	nullify(t, db, "lineage_column", "node_id")

	_, err := r.ImpactAnalysis(context.Background(), ws.ID, "dwd_order", "amount", 3)
	wantErr(t, err, "NULL")
}

// 列策略的更新时间列漂移时必须报错：脱敏策略读错等于没脱敏。
func TestListColumnPoliciesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.SaveColumnPolicy(context.Background(), ws.ID, model.ColumnPolicy{
		TableName: "orders", ColumnName: "phone", Classification: "secret", MaskType: "phone",
	}); err != nil {
		t.Fatalf("写入列策略失败: %v", err)
	}
	textColumn(t, db, "column_policy", "updated_at")

	_, err := r.ListColumnPolicies(context.Background(), ws.ID, "")
	wantErr(t, err, "scan")
}

// 行策略的创建时间列漂移时必须报错。
func TestListRowPoliciesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveRowPolicy(context.Background(), ws.ID, model.RowPolicy{
		TableName: "orders", FilterExpr: "tenant_id = 1",
	}); err != nil {
		t.Fatalf("写行策略失败: %v", err)
	}
	textColumn(t, db, "row_policy", "created_at")

	_, err := r.ListRowPolicies(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 过滤条件为 NULL 时 RowFiltersFor 必须报错：
// 把 NULL 当空条件会让行级权限直接失效（变成查全表）。
func TestRowFiltersForFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveRowPolicy(context.Background(), ws.ID, model.RowPolicy{
		TableName: "orders", FilterExpr: "tenant_id = 1",
	}); err != nil {
		t.Fatalf("写行策略失败: %v", err)
	}
	nullify(t, db, "row_policy", "filter_expr")

	_, err := r.RowFiltersFor(context.Background(), ws.ID, "orders", "analyst")
	wantErr(t, err, "NULL")
}

// 导出任务的行数列漂移时必须报错：下载页显示「0 行」会让人以为导出失败。
func TestListExportsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: ws.ID, Kind: "metric", Target: "gmv",
	}); err != nil {
		t.Fatalf("创建导出任务失败: %v", err)
	}
	textColumn(t, db, "export_job", "row_count")

	_, err := r.ListExports(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 订阅列表的创建时间列漂移时必须报错。
func TestListSubscriptionsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveSubscription(context.Background(), ws.ID, model.Subscription{
		Name: "s", Target: "gmv", Enabled: true,
	}); err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	textColumn(t, db, "subscription", "created_at")

	_, err := r.ListSubscriptions(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 调度器扫描启用订阅时，上次运行时间列漂移必须报错——
// 静默跳过会让订阅永远不再推送。
func TestListEnabledSubscriptionsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	sub, err := r.SaveSubscription(context.Background(), ws.ID, model.Subscription{
		Name: "s", Target: "gmv", Enabled: true,
	})
	if err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	// 先写一次运行时间，否则该列为 NULL，改成文本也扫不出错。
	if err := r.MarkSubscriptionRun(context.Background(), sub.ID, "success"); err != nil {
		t.Fatalf("标记订阅运行失败: %v", err)
	}
	textColumn(t, db, "subscription", "last_run_at")

	_, err = r.ListEnabledSubscriptions(context.Background())
	wantErr(t, err, "scan")
}

// 分享链接的创建时间列漂移时必须报错。
func TestListSharesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.CreateShare(context.Background(), ws.ID, "dashboard", "d", "u", 0); err != nil {
		t.Fatalf("创建分享失败: %v", err)
	}
	textColumn(t, db, "share_link", "created_at")

	_, err := r.ListShares(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 成员列表的加入时间列漂移时必须报错：成员页看不清谁什么时候进来的，
// 权限回收就无从审计。
func TestListMembersFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.AddMember(context.Background(), ws.ID, "u1", "viewer"); err != nil {
		t.Fatalf("添加成员失败: %v", err)
	}
	textColumn(t, db, "workspace_member", "created_at")

	_, err := r.ListMembers(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 工作区列表的创建时间列漂移时必须报错（多租户门户的第一屏）。
func TestListWorkspacesForFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	seedWorkspace(t, r, "w")
	textColumn(t, db, "workspace", "created_at")

	_, err := r.ListWorkspacesFor(context.Background(), "admin", "admin")
	wantErr(t, err, "scan")
}

// 列级血缘整图导出：上游表列在主键里，出现 NULL 说明约束被摘掉，
// 这种脏数据必须让字典接口报错，而不是输出一条空边。
func TestAllColumnEdgesFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.ReplaceColumnLineage(context.Background(), ws.ID, "dwd_order",
		[]model.ColumnEdge{{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"}}); err != nil {
		t.Fatalf("写入列级血缘失败: %v", err)
	}
	nullifyKeyColumn(t, db, "lineage_column", "upstream_table")

	_, err := r.AllColumnEdges(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}
