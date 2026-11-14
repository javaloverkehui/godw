package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/testsupport"
)

// 序列化失败必须让整次写入失败，而不是把半个对象落库：
// 这些字段是 JSONB 列，值里出现函数/通道时 json.Marshal 会直接报错。
func TestWritesRejectUnserializablePayload(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	bad := map[string]any{"fn": func() {}}

	if _, err := r.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: ws.ID, Name: "bad_cfg", Type: "postgres", Config: bad,
	}, nil); err == nil {
		t.Fatal("config 无法序列化时应报错")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM datasource WHERE name='bad_cfg'`); n != 0 {
		t.Fatalf("失败的写入不应落库: %d", n)
	}

	p := seedPipeline(t, r, ws.ID, "p1")
	if _, _, err := r.CreateInstance(ctx, model.Instance{
		WorkspaceID: ws.ID, PipelineID: p.ID, PipelineVer: 1, BizDate: "2024-01-01",
		TriggerType: "manual", IdempotencyKey: "k-bad", Params: bad,
	}); err == nil {
		t.Fatal("params 无法序列化时应报错")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM instance`); n != 0 {
		t.Fatalf("失败的实例创建不应落库: %d", n)
	}

	if _, err := r.CreateExport(ctx, model.ExportJob{
		WorkspaceID: ws.ID, Kind: "table", Target: "t", Params: bad,
	}); err == nil {
		t.Fatal("导出参数无法序列化时应报错")
	}
	if _, err := r.SaveSubscription(ctx, ws.ID, model.Subscription{
		Name: "sub", Kind: "metric", Target: "gmv", Config: bad,
	}); err == nil {
		t.Fatal("订阅配置无法序列化时应报错")
	}
	if err := r.Audit(ctx, model.AuditLog{
		WorkspaceID: ws.ID, Action: "x", Detail: bad,
	}); err == nil {
		t.Fatal("审计详情无法序列化时应报错")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM audit_log`); n != 0 {
		t.Fatalf("失败的审计不应落库: %d", n)
	}
}

// 外键/约束失败必须整体回滚：替换列血缘先 DELETE 再 INSERT，
// 中途失败时旧边不能被删掉。
func TestTransactionRollbackOnConstraintViolation(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	if err := r.ReplaceColumnLineage(ctx, ws.ID, "dwd_order", []model.ColumnEdge{
		{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"},
	}); err != nil {
		t.Fatalf("准备列血缘失败: %v", err)
	}
	// 工作区不存在：DELETE 无效果，INSERT 触发外键错误。
	if err := r.ReplaceColumnLineage(ctx, "ws_missing", "dwd_order", []model.ColumnEdge{
		{UpstreamTable: "ods_order", UpstreamColumn: "id", DownstreamColumn: "order_id"},
	}); err == nil {
		t.Fatal("工作区不存在时应报错")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_column WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("原有列血缘不应被破坏: %d", n)
	}

	// 模型保存同样是事务：DDL/版本插入失败时，模型行与版本都不该出现。
	badSpec := model.ModelSpec{Kind: "model", Name: "bad_layer", Layer: "not_a_layer"}
	if _, err := r.SaveModel(ctx, ws.ID, badSpec, "", "", "tester", true); err == nil {
		t.Fatal("非法分层应被 CHECK 约束拒绝")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM model WHERE name='bad_layer'`); n != 0 {
		t.Fatalf("失败后不应残留模型行: %d", n)
	}

	// 已有模型改成非法分层：更新语句失败，原记录保持不变。
	if _, err := r.SaveModel(ctx, ws.ID, model.ModelSpec{Kind: "model", Name: "keep", Layer: "ods"}, "", "", "t", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	if _, err := r.SaveModel(ctx, ws.ID, model.ModelSpec{Kind: "model", Name: "keep", Layer: "bogus"}, "", "", "t", false); err == nil {
		t.Fatal("非法分层应被拒绝")
	}
	if layer := queryStr(t, db, `SELECT layer FROM model WHERE name='keep'`); layer != "ods" {
		t.Fatalf("失败的更新不应改动原记录: %q", layer)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM model_version WHERE model_id=(SELECT id FROM model WHERE name='keep')`); n != 1 {
		t.Fatalf("失败的更新不应追加版本: %d", n)
	}

	// 管道创建失败时不留半条记录。
	if _, err := r.SavePipeline(ctx, "ws_missing", model.PipelineSpec{Name: "orphan"}, "t", true); err == nil {
		t.Fatal("工作区不存在时应报错")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM pipeline WHERE name='orphan'`); n != 0 {
		t.Fatalf("失败后不应残留管道: %d", n)
	}

	// 任务写入是批量：实例不存在时整批失败。
	if err := r.AddTasks(ctx, "ins_missing", []model.NodeSpec{{ID: "n1", Type: "sql"}}); err == nil {
		t.Fatal("实例不存在时写任务应失败")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM task_instance`); n != 0 {
		t.Fatalf("失败后不应残留任务: %d", n)
	}

	// 行策略与环境的写入口也要靠外键兜底。
	if _, err := r.SaveRowPolicy(ctx, "ws_missing", model.RowPolicy{TableName: "t", FilterExpr: "1=1"}); err == nil {
		t.Fatal("工作区不存在时保存行策略应失败")
	}
	if err := r.EnsureEnvironment(ctx, "ws_missing", "prod", ""); err == nil {
		t.Fatal("工作区不存在时创建环境应失败")
	}
}

// 结构变更的 detail 不带 " -> " 时（历史数据或人工写入），From/To 应保持空，
// 不能把整段文本塞进 From。
func TestListSchemaChangesToleratesBadDetail(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	execSQL(t, db, `INSERT INTO schema_change(id,workspace_id,datasource,schema_name,table_name,kind,column_name,detail,breaking)
		VALUES('sch_1',$1,'pg','public','orders','add_table','','',false)`, ws.ID)

	list, err := r.ListSchemaChanges(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列结构变更失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条变更: %+v", list)
	}
	if list[0].From != "" || list[0].To != "" {
		t.Fatalf("无分隔符的 detail 不应拆出 From/To: %+v", list[0])
	}
}

// 趋势天数超过上限时被钳制到 90 天；结果仍然正确。
func TestQualityTrendClampsDays(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	run := seedInstance(t, r, instSample(ws.ID, p.ID, "k", "2024-01-01", "success"))
	if err := r.SaveQualityResults(ctx, run.ID, "n1", []qualityRow{
		{RuleName: "r", Observed: "0", Expected: "0", Severity: "error", Passed: true},
	}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	if trend, err := r.QualityTrend(ctx, ws.ID, 1000); err != nil || len(trend) != 1 {
		t.Fatalf("超大天数应被钳制且仍能查到今天的桶: %d %v", len(trend), err)
	}
	// 0 天回落到 14 天。
	if trend, err := r.QualityTrend(ctx, ws.ID, 0); err != nil || len(trend) != 1 {
		t.Fatalf("0 天应回落默认值: %d %v", len(trend), err)
	}
}

// Close 之后连接池不可用：CLI 的优雅退出依赖它真的关掉连接。
func TestDBClose(t *testing.T) {
	db := testsupport.DB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("关闭前应可 Ping: %v", err)
	}
	db.Close()
	if err := db.Ping(ctx); err == nil {
		t.Fatal("关闭后 Ping 应失败")
	}
	if _, _, err := db.Version(ctx); err == nil {
		t.Fatal("关闭后查询应失败")
	}
}
