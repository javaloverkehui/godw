package service_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// 未分区的表做「按日期删除」会全表扫描，计划必须显式 skip 并说明原因，
// 而不是生成一条看起来正确、实际很贵的 DELETE。
func TestPlanRetentionSkipsUnpartitioned(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_ret_skip", "dw", "dwd_ret_skip")
	spec.PartitionBy = nil
	publishModel(t, svc, ws.ID, spec)
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_skip", RetainDays: 30, Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}

	plan, err := svc.PlanRetention(ctxBG(), ws.ID, "dwd_ret_skip")
	if err != nil {
		t.Fatalf("PlanRetention 失败: %v", err)
	}
	if plan.Action != "skip" || plan.Note == "" || len(plan.Statements) != 0 {
		t.Fatalf("未分区模型应跳过: %+v", plan)
	}
	// 跳过时执行必须报参数错误，不能返回 0 行让人误以为「已清理」。
	if _, err := svc.RunRetention(ctxBG(), ws.ID, "dwd_ret_skip", "alice"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("跳过计划执行应报错，实际 %v", err)
	}
}

// 归档模式要同时给出 COPY 与 DELETE，且截止日期按保留天数换算。
func TestPlanRetentionArchiveStatements(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_ret_plan", "dw", "dwd_ret_plan"))
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_plan", RetainDays: 10, ArchiveTo: "s3://bucket/dwd", Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}

	plan, err := svc.PlanRetention(ctxBG(), ws.ID, "dwd_ret_plan")
	if err != nil {
		t.Fatalf("PlanRetention 失败: %v", err)
	}
	wantCutoff := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	if plan.Action != "archive" || plan.Cutoff != wantCutoff || plan.RetainDays != 10 {
		t.Fatalf("归档计划不对: %+v (期望 cutoff=%s)", plan, wantCutoff)
	}
	if len(plan.Statements) != 3 {
		t.Fatalf("归档应有三条语句: %+v", plan.Statements)
	}
	if !strings.Contains(plan.Statements[0], "s3://bucket/dwd") {
		t.Fatalf("归档目标必须写进计划: %+v", plan.Statements)
	}
	if !strings.HasPrefix(plan.Statements[1], "COPY") {
		t.Fatalf("第二条应为归档 COPY: %+v", plan.Statements)
	}
	if !strings.Contains(plan.Statements[2], "DELETE FROM") || !strings.Contains(plan.Statements[2], wantCutoff) {
		t.Fatalf("第三条应为带截止日期的 DELETE: %+v", plan.Statements)
	}
	// 表名要按 schema 限定，避免多 schema 同名表误删。
	if !strings.Contains(plan.Statements[2], `"dw"."dwd_ret_plan"`) {
		t.Fatalf("表名应带 schema 引号: %s", plan.Statements[2])
	}

	// 无归档时只有一条 DELETE。
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_plan", RetainDays: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("更新保留策略失败: %v", err)
	}
	plan, _ = svc.PlanRetention(ctxBG(), ws.ID, "dwd_ret_plan")
	if plan.Action != "delete" || len(plan.Statements) != 1 || !strings.Contains(plan.Statements[0], "DELETE") {
		t.Fatalf("无归档应只有 DELETE: %+v", plan)
	}
}

// 保留策略必须真的删掉过期数据并保留窗口内的数据，同时写审计。
func TestRunRetentionDeletesExpiredRows(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2))`, p.table("dwd_ret_t"))
	old := time.Now().AddDate(0, 0, -40).Format("2006-01-02")
	recent := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	p.exec(t, `INSERT INTO %s VALUES ('%s',1,1),('%s',2,2),('%s',3,3)`, p.table("dwd_ret_t"), old, old, recent)

	spec := modelSpec(ds.Name, "dwd_ret_run", p.schema, "dwd_ret_t")
	publishModel(t, svc, ws.ID, spec)
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_run", RetainDays: 30, Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}

	n, err := svc.RunRetention(ctxBG(), ws.ID, "dwd_ret_run", "alice")
	if err != nil {
		t.Fatalf("RunRetention 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 行过期数据，实际 %d", n)
	}
	var left int
	if err := p.conn.QueryRow(ctxBG(), `SELECT count(*) FROM `+p.table("dwd_ret_t")).Scan(&left); err != nil {
		t.Fatalf("统计剩余行失败: %v", err)
	}
	if left != 1 {
		t.Fatalf("窗口内数据不应被删，实际剩 %d 行", left)
	}
	audits, err := svc.Repo.ListAudit(ctxBG(), ws.ID, 10)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "retention.run" && a.Actor == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("保留策略执行必须留审计: %+v", audits)
	}
}

// 删除权：列必须真实存在，原因必填；实际删除走同一套 SQL 拼装，
// 单引号必须转义，否则主体值里的 ' 会把语句截断。
func TestErasureRequestAndRun(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, user_id text)`, p.table("dwd_ers_t"))
	p.exec(t, `INSERT INTO %s VALUES (now(),1,'u1'),(now(),2,'u1'),(now(),3,'u2')`, p.table("dwd_ers_t"))

	spec := modelSpec(ds.Name, "dwd_ers_run", p.schema, "dwd_ers_t")
	spec.Columns = append(spec.Columns, model.ColumnSpec{Name: "user_id", Type: "string"})
	publishModel(t, svc, ws.ID, spec)

	if _, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_ers_run", "", "u1", "r", "alice"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺主体列应被拒绝，实际 %v", err)
	}
	if _, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_ers_run", "user_id", "u1", " ", "alice"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺原因应被拒绝，实际 %v", err)
	}
	if _, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_ers_run", "not_a_col", "u1", "r", "alice"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("不存在的列应被拒绝，实际 %v", err)
	}

	// 走导出入口提交，异步执行由 waitErasure 轮询到终态。
	job, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_ers_run", "user_id", "u1", "用户申请", "alice")
	if err != nil {
		t.Fatalf("提交删除请求失败: %v", err)
	}
	done := waitErasure(t, svc, ws.ID, job.ID)
	if done.Status != "done" || done.RowsDeleted != 2 {
		t.Fatalf("删除结果不对: %+v", done)
	}
	var leftU1, leftU2 int
	_ = p.conn.QueryRow(ctxBG(), `SELECT count(*) FROM `+p.table("dwd_ers_t")+` WHERE user_id='u1'`).Scan(&leftU1)
	_ = p.conn.QueryRow(ctxBG(), `SELECT count(*) FROM `+p.table("dwd_ers_t")+` WHERE user_id='u2'`).Scan(&leftU2)
	if leftU1 != 0 || leftU2 != 1 {
		t.Fatalf("只应删除目标主体: u1=%d u2=%d", leftU1, leftU2)
	}

	// 主体值里带单引号时，语句仍然合法（转义成两个单引号）。
	p.exec(t, `INSERT INTO %s VALUES (now(),4,'o''brien')`, p.table("dwd_ers_t"))
	job2, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_ers_run", "user_id", "o'brien", "转义用例", "alice")
	if err != nil {
		t.Fatalf("提交删除请求失败: %v", err)
	}
	done2 := waitErasure(t, svc, ws.ID, job2.ID)
	if done2.Status != "done" || done2.RowsDeleted != 1 {
		t.Fatalf("带引号的主体值应正常删除: %+v", done2)
	}
}

// waitErasure 轮询异步任务到终态。删除是 goroutine 里跑的，
// 不能假设 RequestErasure 返回时已完成。
func waitErasure(t *testing.T, svc *service.Service, wsID, id string) model.ErasureJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := svc.Repo.ListErasureJobs(ctxBG(), wsID, 50)
		if err != nil {
			t.Fatalf("查询删除任务失败: %v", err)
		}
		for _, j := range jobs {
			if j.ID == id && (j.Status == "done" || j.Status == "failed") {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("删除任务 %s 未在超时内完成", id)
	return model.ErasureJob{}
}

// 导出的分级必须基于「实际会读到的物理表」，而不是用户传入的 target 字符串，
// 否则审批页看到的依据可以被随手编造的 target 骗过去。
func TestExportTablesAndSensitivity(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_exp", "dw", "dwd_exp"))

	m, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("orders", "dwd_exp"))
	if err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_exp", "dwd_exp", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, dashboardFor("dash_exp", m.Name)); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	tables, err := svc.ExportTables(ctxBG(), ws.ID, "metric", "orders")
	if err != nil || len(tables) != 1 || tables[0] != "dwd_exp" {
		t.Fatalf("指标导出表解析不对: %v %v", tables, err)
	}
	tables, err = svc.ExportTables(ctxBG(), ws.ID, "dashboard", "dash_exp")
	if err != nil || len(tables) != 1 || tables[0] != "dwd_exp" {
		t.Fatalf("看板导出表解析不对: %v %v", tables, err)
	}
	tables, err = svc.ExportTables(ctxBG(), ws.ID, "dataset", "ds_exp")
	if err != nil || len(tables) != 1 || tables[0] != "dwd_exp" {
		t.Fatalf("数据集导出表解析不对: %v %v", tables, err)
	}
	if _, err := svc.ExportTables(ctxBG(), ws.ID, "metric", "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的指标应报 NotFound，实际 %v", err)
	}

	// 没有策略时分级为 none。
	level, _, err := svc.ExportSensitivity(ctxBG(), ws.ID, "metric", "orders")
	if err != nil || level != "none" {
		t.Fatalf("无策略应为 none: %q %v", level, err)
	}
	// 表上有一条 secret 列 → 整个导出升为 high。
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_exp", ColumnName: "amount", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}
	level, tables, err = svc.ExportSensitivity(ctxBG(), ws.ID, "metric", "orders")
	if err != nil || level != "high" || !hasStr(tables, "dwd_exp") {
		t.Fatalf("分级应为 high: %q %v %v", level, tables, err)
	}
	_ = fmt.Sprint(level)
}
