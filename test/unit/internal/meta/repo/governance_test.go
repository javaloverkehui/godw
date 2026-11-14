package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 列级血缘是一次事务内的整表替换：要么全换，要么不动，不能出现半更新。
func TestReplaceColumnLineage(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	edges := []model.ColumnEdge{
		{UpstreamTable: "ods_order", UpstreamColumn: "amount", DownstreamColumn: "amount", NodeID: "n1"},
		{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"},
		// 缺列的边应被跳过。
		{UpstreamTable: "", UpstreamColumn: "x", DownstreamColumn: "y"},
		{UpstreamTable: "ods_order", UpstreamColumn: "", DownstreamColumn: "z"},
		{UpstreamTable: "ods_order", UpstreamColumn: "w", DownstreamColumn: ""},
	}
	if err := r.ReplaceColumnLineage(ctx, ws.ID, "dwd_order", edges); err != nil {
		t.Fatalf("写列血缘失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_column WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("应只写入 2 条完整边，实际 %d", n)
	}

	got, err := r.ColumnLineage(ctx, ws.ID, "dwd_order", "")
	if err != nil {
		t.Fatalf("查列血缘失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条边，实际 %d: %+v", len(got), got)
	}
	// 按下游列名排序：amount 在 order_id 之前。
	if got[0].DownstreamColumn != "amount" || got[0].UpstreamColumn != "amount" || got[0].NodeID != "n1" {
		t.Fatalf("边字段不对: %+v", got[0])
	}
	if got[1].DownstreamColumn != "order_id" || got[1].UpstreamColumn != "id" {
		t.Fatalf("第二条边不对: %+v", got[1])
	}

	// 按列过滤。
	only, err := r.ColumnLineage(ctx, ws.ID, "dwd_order", "amount")
	if err != nil {
		t.Fatalf("按列过滤失败: %v", err)
	}
	if len(only) != 1 || only[0].DownstreamColumn != "amount" {
		t.Fatalf("列过滤结果不对: %+v", only)
	}
	if none, err := r.ColumnLineage(ctx, ws.ID, "dwd_order", "nope"); err != nil || len(none) != 0 {
		t.Fatalf("不存在的列应返回空集合: %v %v", none, err)
	}

	// 整表替换会清掉旧边。
	if err := r.ReplaceColumnLineage(ctx, ws.ID, "dwd_order", []model.ColumnEdge{
		{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"},
	}); err != nil {
		t.Fatalf("重写列血缘失败: %v", err)
	}
	got, err = r.ColumnLineage(ctx, ws.ID, "dwd_order", "")
	if err != nil {
		t.Fatalf("复查列血缘失败: %v", err)
	}
	if len(got) != 1 || got[0].DownstreamColumn != "order_id" {
		t.Fatalf("替换后应只剩 1 条: %+v", got)
	}

	// 自引用边（同表同列）在查询时被过滤掉，但仍然会落库——
	// 它是列映射的合法中间产物，展示层不该显示成「自己依赖自己」。
	if err := r.ReplaceColumnLineage(ctx, ws.ID, "dwd_order", []model.ColumnEdge{
		{UpstreamTable: "dwd_order", UpstreamColumn: "id", DownstreamColumn: "id"},
	}); err != nil {
		t.Fatalf("写自引用边失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_column WHERE downstream_table='dwd_order'`); n != 1 {
		t.Fatalf("自引用边应落库: %d", n)
	}
	if got, err := r.ColumnLineage(ctx, ws.ID, "dwd_order", ""); err != nil || len(got) != 0 {
		t.Fatalf("自引用边不应出现在查询结果里: %v %v", got, err)
	}

	// 全量列边供整图渲染，字段顺序是 upstream → downstream。
	if err := r.ReplaceColumnLineage(ctx, other.ID, "dws_trade", []model.ColumnEdge{
		{UpstreamTable: "dwd_order", UpstreamColumn: "amount", DownstreamColumn: "gmv"},
	}); err != nil {
		t.Fatalf("其他工作区写血缘失败: %v", err)
	}
	all, err := r.AllColumnEdges(ctx, other.ID)
	if err != nil {
		t.Fatalf("取全量列边失败: %v", err)
	}
	if len(all) != 1 || all[0].UpstreamTable != "dwd_order" || all[0].DownstreamTable != "dws_trade" ||
		all[0].UpstreamColumn != "amount" || all[0].DownstreamColumn != "gmv" {
		t.Fatalf("全量列边字段不对: %+v", all)
	}
	if got, err := r.AllColumnEdges(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空集合: %v %v", got, err)
	}
}

// 影响分析沿列血缘向下递归：depth 控制层数，深度外的下游不应出现。
func TestImpactAnalysisDepth(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	// ods.x → dwd.x → dws.x → ads.x
	for _, e := range []struct{ down, upTable string }{
		{"dwd", "ods"}, {"dws", "dwd"}, {"ads", "dws"},
	} {
		if err := r.ReplaceColumnLineage(ctx, ws.ID, e.down+"_t", []model.ColumnEdge{
			{UpstreamTable: e.upTable + "_t", UpstreamColumn: "x", DownstreamColumn: "x", NodeID: e.down},
		}); err != nil {
			t.Fatalf("准备 %s 血缘失败: %v", e.down, err)
		}
	}

	// 默认 10 层：三跳全中。
	all, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "x", 0)
	if err != nil {
		t.Fatalf("影响分析失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("应命中 3 张下游表，实际 %d: %+v", len(all), all)
	}
	// 按下游表名排序：ads < dwd < dws。
	if all[0].DownstreamTable != "ads_t" || all[1].DownstreamTable != "dwd_t" || all[2].DownstreamTable != "dws_t" {
		t.Fatalf("下游顺序不对: %+v", all)
	}

	// 只看一跳。
	one, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "x", 1)
	if err != nil {
		t.Fatalf("一跳影响分析失败: %v", err)
	}
	if len(one) != 1 || one[0].DownstreamTable != "dwd_t" {
		t.Fatalf("depth=1 应只返回直接下游: %+v", one)
	}
	// 两跳。
	two, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "x", 2)
	if err != nil {
		t.Fatalf("两跳影响分析失败: %v", err)
	}
	if len(two) != 2 {
		t.Fatalf("depth=2 应返回两层: %+v", two)
	}
	// 超过 10 的深度被钳制成 10，结果与全量一致。
	if capped, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "x", 99); err != nil || len(capped) != 3 {
		t.Fatalf("超大 depth 应钳制: %d %v", len(capped), err)
	}

	// 指定列不存在时返回空集合；不指定列则匹配该表所有列。
	if none, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "y", 10); err != nil || len(none) != 0 {
		t.Fatalf("不存在的列应返回空集合: %v %v", none, err)
	}
	byTable, err := r.ImpactAnalysis(ctx, ws.ID, "ods_t", "", 10)
	if err != nil {
		t.Fatalf("按表影响分析失败: %v", err)
	}
	if len(byTable) != 3 {
		t.Fatalf("按表应命中 3 条: %+v", byTable)
	}
	// 末级表没有下游。
	if leaf, err := r.ImpactAnalysis(ctx, ws.ID, "ads_t", "", 10); err != nil || len(leaf) != 0 {
		t.Fatalf("末级表应没有下游: %v %v", leaf, err)
	}
}

// 列策略是 upsert：同一列反复配置只更新分级与脱敏方式，updated_at 随之刷新。
func TestColumnPolicyUpsertAndFilter(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if err := r.SaveColumnPolicy(ctx, ws.ID, model.ColumnPolicy{
		TableName: "users", ColumnName: "phone", Classification: "confidential", MaskType: "phone",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}
	if err := r.SaveColumnPolicy(ctx, ws.ID, model.ColumnPolicy{
		TableName: "users", ColumnName: "id", Classification: "public", MaskType: "none",
	}); err != nil {
		t.Fatalf("保存第二条列策略失败: %v", err)
	}
	if err := r.SaveColumnPolicy(ctx, other.ID, model.ColumnPolicy{
		TableName: "users", ColumnName: "phone", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("其他工作区保存失败: %v", err)
	}

	// 同一列改分级：行数不变，值更新。
	if err := r.SaveColumnPolicy(ctx, ws.ID, model.ColumnPolicy{
		TableName: "users", ColumnName: "phone", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("更新列策略失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM column_policy WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("upsert 不应新增行: %d", n)
	}

	policies, err := r.ListColumnPolicies(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("列策略查询失败: %v", err)
	}
	if len(policies) != 2 || policies[0].ColumnName != "id" || policies[1].ColumnName != "phone" {
		t.Fatalf("应按表名、列名排序: %+v", policies)
	}
	if policies[1].Classification != "secret" || policies[1].MaskType != "full" {
		t.Fatalf("更新未生效: %+v", policies[1])
	}
	if policies[1].WorkspaceID != ws.ID || policies[1].UpdatedAt.IsZero() {
		t.Fatalf("应回填工作区与时间: %+v", policies[1])
	}

	only, err := r.ListColumnPolicies(ctx, ws.ID, "users")
	if err != nil {
		t.Fatalf("按表过滤失败: %v", err)
	}
	if len(only) != 2 {
		t.Fatalf("users 应有 2 条策略: %+v", only)
	}
	if none, err := r.ListColumnPolicies(ctx, ws.ID, "orders"); err != nil || len(none) != 0 {
		t.Fatalf("不存在的表应返回空集合: %v %v", none, err)
	}

	// AllColumnPolicies 等价于不带表过滤的列表。
	all, err := r.AllColumnPolicies(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取全部列策略失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应返回 2 条: %+v", all)
	}
}

// 行策略：角色缺省是通配 "*"，查询时把角色自己的策略与通配策略一起返回。
func TestRowPolicyLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	p, err := r.SaveRowPolicy(ctx, ws.ID, model.RowPolicy{
		TableName: "orders", FilterExpr: "region = '${region}'", Description: "区域隔离",
	})
	if err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}
	if p.ID == "" || p.Role != "*" || p.WorkspaceID != ws.ID {
		t.Fatalf("应回填 ID 并默认通配角色: %+v", p)
	}
	if _, err := r.SaveRowPolicy(ctx, ws.ID, model.RowPolicy{
		TableName: "orders", Role: "analyst", FilterExpr: "org_id = 1",
	}); err != nil {
		t.Fatalf("保存角色策略失败: %v", err)
	}
	// 同 (表, 角色) 再保存是更新。
	if _, err := r.SaveRowPolicy(ctx, ws.ID, model.RowPolicy{
		TableName: "orders", Role: "analyst", FilterExpr: "org_id = 2", Description: "更新后",
	}); err != nil {
		t.Fatalf("更新行策略失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM row_policy WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("upsert 不应新增行: %d", n)
	}

	list, err := r.ListRowPolicies(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列行策略失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条策略: %+v", list)
	}
	// ORDER BY table_name, role：'*' 排在 'analyst' 前面。
	if list[0].Role != "*" || list[0].FilterExpr != "region = '${region}'" {
		t.Fatalf("第一条应是通配策略: %+v", list[0])
	}
	if list[1].Role != "analyst" || list[1].FilterExpr != "org_id = 2" || list[1].Description != "更新后" {
		t.Fatalf("角色策略更新未生效: %+v", list[1])
	}
	if list[0].CreatedAt.IsZero() || list[0].WorkspaceID != ws.ID {
		t.Fatalf("应回填时间与工作区: %+v", list[0])
	}

	// 角色过滤：analyst 拿到自己的 + 通配的；其他人只拿通配的。
	filters, err := r.RowFiltersFor(ctx, ws.ID, "orders", "analyst")
	if err != nil {
		t.Fatalf("取角色过滤条件失败: %v", err)
	}
	if len(filters) != 2 {
		t.Fatalf("analyst 应拿到 2 条过滤条件: %+v", filters)
	}
	filters, err = r.RowFiltersFor(ctx, ws.ID, "orders", "viewer")
	if err != nil {
		t.Fatalf("取角色过滤条件失败: %v", err)
	}
	if len(filters) != 1 || filters[0] != "region = '${region}'" {
		t.Fatalf("viewer 只应拿到通配策略: %+v", filters)
	}
	if none, err := r.RowFiltersFor(ctx, ws.ID, "users", "analyst"); err != nil || len(none) != 0 {
		t.Fatalf("无策略的表应返回空: %v %v", none, err)
	}

	// 删除：按 id，删不到要报 NotFound。
	if err := r.DeleteRowPolicy(ctx, ws.ID, list[1].ID); err != nil {
		t.Fatalf("删除行策略失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM row_policy WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("删除后应剩 1 条: %d", n)
	}
	if err := r.DeleteRowPolicy(ctx, ws.ID, "rpl_missing"); err == nil {
		t.Fatal("删除不存在的策略应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 跨工作区删不掉别人的策略。
	other := seedWorkspace(t, r, "other")
	if err := r.DeleteRowPolicy(ctx, other.ID, list[0].ID); err == nil {
		t.Fatal("跨工作区删除应报 NotFound")
	}
}

// 查询缓存：命中计数只统计未过期条目，清理要么按过期时间、要么按工作区整片清。
func TestQueryCache(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	// 未命中不是错误。
	if payload, ok, err := r.CacheGet(ctx, "k_missing"); err != nil || ok || payload != nil {
		t.Fatalf("未命中应返回 (nil,false,nil): %v %v %v", payload, ok, err)
	}

	if err := r.CachePut(ctx, "k1", ws.ID, []byte(`{"total":1}`), time.Minute); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	payload, ok, err := r.CacheGet(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("应命中缓存: %v %v", ok, err)
	}
	if string(payload) != `{"total": 1}` {
		t.Fatalf("payload 应往返（JSONB 会重排空白）: %s", payload)
	}
	// 再取一次，命中计数累加。
	if _, ok, err := r.CacheGet(ctx, "k1"); err != nil || !ok {
		t.Fatalf("第二次取缓存失败: %v %v", ok, err)
	}
	if n := queryInt(t, db, `SELECT hit_count FROM query_cache WHERE cache_key='k1'`); n != 2 {
		t.Fatalf("命中计数应为 2，实际 %d", n)
	}

	// 同 key 覆盖写会把命中计数重置为 0（旧结果的命中数不再代表新结果）。
	if err := r.CachePut(ctx, "k1", ws.ID, []byte(`{"total":2}`), time.Minute); err != nil {
		t.Fatalf("覆盖写缓存失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT hit_count FROM query_cache WHERE cache_key='k1'`); n != 0 {
		t.Fatalf("覆盖写应重置命中计数: %d", n)
	}

	// 已过期的不算命中。
	if err := r.CachePut(ctx, "expired", ws.ID, []byte(`1`), -time.Minute); err != nil {
		t.Fatalf("写过期缓存失败: %v", err)
	}
	if _, ok, err := r.CacheGet(ctx, "expired"); err != nil || ok {
		t.Fatalf("过期缓存不应命中: %v %v", ok, err)
	}

	// 统计只算本工作区未过期条目。
	stats, err := r.CacheStats(ctx, ws.ID)
	if err != nil {
		t.Fatalf("缓存统计失败: %v", err)
	}
	if stats["entries"] != int64(1) {
		t.Fatalf("条目数应为 1（过期的不算）: %+v", stats)
	}
	if stats["hits"] != int64(0) {
		t.Fatalf("覆盖写后命中数应为 0: %+v", stats)
	}
	if stats["bytes"].(int64) <= 0 {
		t.Fatalf("字节数应大于 0: %+v", stats)
	}

	// 按过期时间清理：只删过期那条。
	purged, err := r.CachePurge(ctx)
	if err != nil {
		t.Fatalf("清理过期缓存失败: %v", err)
	}
	if purged != 1 {
		t.Fatalf("应清理 1 条过期缓存，实际 %d", purged)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM query_cache WHERE cache_key='k1'`); n != 1 {
		t.Fatalf("未过期缓存应保留: %d", n)
	}

	// 按工作区清理：管道跑完后整片失效。
	if err := r.CachePut(ctx, "k2", ws.ID, []byte(`2`), time.Minute); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	if err := r.CachePut(ctx, "k3", other.ID, []byte(`3`), time.Minute); err != nil {
		t.Fatalf("写其他工作区缓存失败: %v", err)
	}
	n, err := r.CachePurgeByWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("按工作区清缓存失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应清掉 2 条，实际 %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM query_cache WHERE workspace_id=$1`, other.ID); n != 1 {
		t.Fatalf("其他工作区缓存应保留: %d", n)
	}
}

// 导出任务的状态机：queued → (审批) → running → success/failed；
// 驳回与完成都要落 finished_at，其余状态不动它。
func TestExportJobLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	e, err := r.CreateExport(ctx, model.ExportJob{
		WorkspaceID: ws.ID, Kind: "table", Target: "dwd_order",
		Params: map[string]any{"format": "csv"}, CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("创建导出任务失败: %v", err)
	}
	if e.ID == "" || e.Status != "queued" || e.CreatedAt.IsZero() {
		t.Fatalf("应回填 ID、默认状态与时间: %+v", e)
	}

	got, err := r.GetExport(ctx, ws.ID, e.ID)
	if err != nil {
		t.Fatalf("读导出任务失败: %v", err)
	}
	if got.Kind != "table" || got.Target != "dwd_order" || got.CreatedBy != "alice" {
		t.Fatalf("导出任务字段不对: %+v", got)
	}
	if got.Params["format"] != "csv" {
		t.Fatalf("参数应往返: %+v", got.Params)
	}
	if got.FilePath != "" || got.RowCount != 0 || got.FinishedAt != nil {
		t.Fatalf("初始状态不应有产物: %+v", got)
	}
	if _, err := r.GetExport(ctx, ws.ID, "exp_missing"); err == nil {
		t.Fatal("不存在的任务应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 跨工作区不可见。
	if _, err := r.GetExport(ctx, other.ID, e.ID); err == nil {
		t.Fatal("跨工作区不应查到导出任务")
	}

	// 驳回：状态变化并落 finished_at。
	if err := r.SetExportStatus(ctx, e.ID, "rejected", "含敏感列"); err != nil {
		t.Fatalf("驳回导出失败: %v", err)
	}
	if msg := queryStr(t, db, `SELECT error FROM export_job WHERE id=$1`, e.ID); msg != "含敏感列" {
		t.Fatalf("驳回原因应落库: %q", msg)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM export_job WHERE id=$1 AND finished_at IS NOT NULL`, e.ID); n != 1 {
		t.Fatalf("驳回应落结束时间")
	}

	// 换一个任务走成功路径：审批通过不落 finished_at，完成才落。
	ok, err := r.CreateExport(ctx, model.ExportJob{WorkspaceID: ws.ID, Kind: "metric", Target: "gmv"})
	if err != nil {
		t.Fatalf("创建第二个导出任务失败: %v", err)
	}
	if err := r.SetExportStatus(ctx, ok.ID, "approved", ""); err != nil {
		t.Fatalf("审批导出失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM export_job WHERE id=$1 AND finished_at IS NOT NULL`, ok.ID); n != 0 {
		t.Fatalf("审批通过不应落结束时间")
	}
	if err := r.FinishExport(ctx, ok.ID, "success", "/tmp/gmv.csv", "", 42); err != nil {
		t.Fatalf("完成导出失败: %v", err)
	}
	finished, err := r.GetExport(ctx, ws.ID, ok.ID)
	if err != nil {
		t.Fatalf("读导出任务失败: %v", err)
	}
	if finished.Status != "success" || finished.FilePath != "/tmp/gmv.csv" || finished.RowCount != 42 || finished.FinishedAt == nil {
		t.Fatalf("完成状态不对: %+v", finished)
	}

	// 失败路径同样落错误与结束时间。
	if err := r.FinishExport(ctx, ok.ID, "failed", "", "磁盘写满", 0); err != nil {
		t.Fatalf("标记导出失败失败: %v", err)
	}
	if msg := queryStr(t, db, `SELECT error FROM export_job WHERE id=$1`, ok.ID); msg != "磁盘写满" {
		t.Fatalf("失败原因应落库: %q", msg)
	}

	// 列表按创建时间倒序；limit 直接下推（不做兜底），0 表示不返回任何行。
	list, err := r.ListExports(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列导出任务失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != ok.ID || list[1].ID != e.ID {
		t.Fatalf("应按创建时间倒序: %+v", list)
	}
	if list[1].Status != "rejected" {
		t.Fatalf("驳回状态应读回: %+v", list[1])
	}
	if one, err := r.ListExports(ctx, ws.ID, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit=1 未生效: %v %v", one, err)
	}
	if zero, err := r.ListExports(ctx, ws.ID, 0); err != nil || len(zero) != 0 {
		t.Fatalf("limit=0 应返回空集合: %v %v", zero, err)
	}
	if got, err := r.ListExports(ctx, other.ID, 10); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区不应看到任务: %v %v", got, err)
	}
}

// 订阅：按名 upsert、记录最近一次推送结果；调度器只扫启用的。
func TestSubscriptionLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	s, err := r.SaveSubscription(ctx, ws.ID, model.Subscription{
		Name: "gmv_daily", Kind: "metric", Target: "gmv", Channel: "webhook",
		Endpoint: "https://hook.example/gmv", Schedule: "0 9 * * *", Enabled: true,
		Config: map[string]any{"format": "markdown"},
	})
	if err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	if s.ID == "" || s.WorkspaceID != ws.ID {
		t.Fatalf("应回填 ID 与工作区: %+v", s)
	}
	// 同名更新：endpoint 与启用状态变化，行数不变。
	if _, err := r.SaveSubscription(ctx, ws.ID, model.Subscription{
		Name: "gmv_daily", Kind: "metric", Target: "gmv", Channel: "webhook",
		Endpoint: "https://hook.example/v2", Schedule: "0 10 * * *", Enabled: false,
	}); err != nil {
		t.Fatalf("更新订阅失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM subscription WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("upsert 不应新增行: %d", n)
	}
	// 另建一个启用的订阅，用来验证调度器扫描与排序。
	if _, err := r.SaveSubscription(ctx, other.ID, model.Subscription{
		Name: "a_ops", Kind: "dashboard", Target: "ops", Channel: "file", Enabled: true,
	}); err != nil {
		t.Fatalf("其他工作区保存订阅失败: %v", err)
	}

	list, err := r.ListSubscriptions(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列订阅失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应只有 1 条订阅: %+v", list)
	}
	if list[0].Endpoint != "https://hook.example/v2" || list[0].Schedule != "0 10 * * *" || list[0].Enabled {
		t.Fatalf("更新未生效: %+v", list[0])
	}
	// upsert 是整份覆盖：第二次没传 config，所以配置被写空。
	// 调用方要改配置必须传完整的 config，这也是 service 层每次读改写的原因。
	if len(list[0].Config) != 0 {
		t.Fatalf("未传 config 时应被覆盖为空: %+v", list[0].Config)
	}
	if _, err := r.SaveSubscription(ctx, ws.ID, model.Subscription{
		Name: "gmv_daily", Kind: "metric", Target: "gmv", Channel: "webhook",
		Enabled: false, Config: map[string]any{"format": "csv"},
	}); err != nil {
		t.Fatalf("回写 config 失败: %v", err)
	}
	list, err = r.ListSubscriptions(ctx, ws.ID)
	if err != nil {
		t.Fatalf("复查订阅失败: %v", err)
	}
	if list[0].Config["format"] != "csv" {
		t.Fatalf("config 应往返: %+v", list[0].Config)
	}

	// 记录推送结果。
	if err := r.MarkSubscriptionRun(ctx, s.ID, "success"); err != nil {
		t.Fatalf("记录推送结果失败: %v", err)
	}
	if status := queryStr(t, db, `SELECT last_status FROM subscription WHERE id=$1`, s.ID); status != "success" {
		t.Fatalf("最近状态应落库: %q", status)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM subscription WHERE id=$1 AND last_run_at IS NOT NULL`, s.ID); n != 1 {
		t.Fatalf("应记录最近推送时间")
	}

	// 调度器视角：跨工作区、只含启用的，按 workspace,name 排序。
	enabled, err := r.ListEnabledSubscriptions(ctx)
	if err != nil {
		t.Fatalf("列启用订阅失败: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Name != "a_ops" || enabled[0].WorkspaceID != other.ID {
		t.Fatalf("只应返回启用的订阅: %+v", enabled)
	}
	// 重新启用后应出现在列表里，且两个订阅按工作区+名称排序。
	if _, err := r.SaveSubscription(ctx, ws.ID, model.Subscription{
		Name: "gmv_daily", Kind: "metric", Target: "gmv", Channel: "webhook", Enabled: true,
	}); err != nil {
		t.Fatalf("重新启用订阅失败: %v", err)
	}
	enabled, err = r.ListEnabledSubscriptions(ctx)
	if err != nil {
		t.Fatalf("复查启用订阅失败: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("应有 2 条启用订阅: %+v", enabled)
	}

	// 删除：删不到要报 NotFound。
	if err := r.DeleteSubscription(ctx, ws.ID, s.ID); err != nil {
		t.Fatalf("删除订阅失败: %v", err)
	}
	if list, err := r.ListSubscriptions(ctx, ws.ID); err != nil || len(list) != 0 {
		t.Fatalf("删除后应为空: %v %v", list, err)
	}
	if err := r.DeleteSubscription(ctx, ws.ID, s.ID); err == nil {
		t.Fatal("重复删除应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
}

// 分享链接：token 随机、可设过期；过期后访问等同于不存在。
func TestShareLinkLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	s, err := r.CreateShare(ctx, ws.ID, "dashboard", "ops", "alice", time.Hour)
	if err != nil {
		t.Fatalf("创建分享失败: %v", err)
	}
	if s.Token == "" || s.WorkspaceID != ws.ID || s.CreatedAt.IsZero() {
		t.Fatalf("应回填 token 与时间: %+v", s)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.After(time.Now()) {
		t.Fatalf("指定 TTL 时应有未来过期时间: %+v", s.ExpiresAt)
	}

	got, err := r.GetShare(ctx, s.Token)
	if err != nil {
		t.Fatalf("取分享失败: %v", err)
	}
	if got.Kind != "dashboard" || got.Target != "ops" || got.CreatedBy != "alice" {
		t.Fatalf("分享字段不对: %+v", got)
	}

	// 无 TTL 的链接永不过期。
	forever, err := r.CreateShare(ctx, ws.ID, "metric", "gmv", "bob", 0)
	if err != nil {
		t.Fatalf("创建永久分享失败: %v", err)
	}
	if forever.ExpiresAt != nil {
		t.Fatalf("TTL=0 不应有过期时间: %+v", forever.ExpiresAt)
	}
	if _, err := r.GetShare(ctx, forever.Token); err != nil {
		t.Fatalf("永久链接应可访问: %v", err)
	}

	// 非正 TTL 视为「永久」，不会写成过去时间。
	noTTL, err := r.CreateShare(ctx, ws.ID, "metric", "gmv", "bob", -time.Minute)
	if err != nil {
		t.Fatalf("创建分享失败: %v", err)
	}
	if noTTL.ExpiresAt != nil {
		t.Fatalf("非正 TTL 不应设置过期时间: %+v", noTTL.ExpiresAt)
	}
	// 已过期：用过期的行验证 GetShare 的过滤。
	execSQL(t, db, `UPDATE share_link SET expires_at = now() - interval '1 minute' WHERE token=$1`, noTTL.Token)
	if _, err := r.GetShare(ctx, noTTL.Token); err == nil {
		t.Fatal("过期链接不应可访问")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 但列表里仍然保留（便于审计与手动清理）。
	shares, err := r.ListShares(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列分享失败: %v", err)
	}
	if len(shares) != 3 {
		t.Fatalf("应有 3 条分享，实际 %d", len(shares))
	}
	// 按创建时间倒序：最后创建的那条在最前。
	if shares[0].Token != noTTL.Token {
		t.Fatalf("应按创建时间倒序: %+v", shares)
	}
	if got, err := r.ListShares(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区不应看到分享: %v %v", got, err)
	}

	if _, err := r.GetShare(ctx, "shr_missing"); err == nil {
		t.Fatal("不存在的 token 应报错")
	}

	// 删除：重复删不报错，但记录确实消失。
	if err := r.DeleteShare(ctx, ws.ID, s.Token); err != nil {
		t.Fatalf("删除分享失败: %v", err)
	}
	if err := r.DeleteShare(ctx, ws.ID, s.Token); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM share_link WHERE token=$1`, s.Token); n != 0 {
		t.Fatalf("分享应被删除: %d", n)
	}
	// 跨工作区删不掉别人的分享。
	if err := r.DeleteShare(ctx, other.ID, forever.Token); err != nil {
		t.Fatalf("跨工作区删除应静默无效果: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM share_link WHERE token=$1`, forever.Token); n != 1 {
		t.Fatalf("跨工作区不应删除: %d", n)
	}
}
