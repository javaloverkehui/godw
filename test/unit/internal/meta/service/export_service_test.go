package service_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// waitStatus 轮询导出任务直到终态，避免异步协程在用例结束后才写库。
func waitStatus(t *testing.T, svc *service.Service, wsID, id string) *model.ExportJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := svc.Repo.GetExport(ctxBG(), wsID, id)
		if err != nil {
			t.Fatalf("读导出任务失败: %v", err)
		}
		switch job.Status {
		case "success", "failed", "rejected":
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("导出任务超时未完成: %s", id)
	return nil
}

// high 级数据不允许直接执行：必须先落成 pending_approval，并把分级的依据
// （级别 + 涉及的表）一起写进 params，审批页与审计都靠它。
func TestCreateExportRequiresApprovalForHigh(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_exp_high", "dw", "dwd_exp_high"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_high", "dwd_exp_high")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_exp_high", ColumnName: "amount", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}

	job, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "gmv_high", map[string]any{"from": "2026-01-01"}, "alice")
	if err != nil {
		t.Fatalf("CreateExport 失败: %v", err)
	}
	if job.Status != "pending_approval" {
		t.Fatalf("high 级导出应待审批，实际 %s", job.Status)
	}
	if job.Params["_classification"] != "high" {
		t.Fatalf("分级依据应写进 params: %+v", job.Params)
	}
	tables, _ := job.Params["_tables"].([]string)
	if len(tables) != 1 || tables[0] != "dwd_exp_high" {
		t.Fatalf("涉及的表应写进 params: %+v", job.Params)
	}
	if job.Params["from"] != "2026-01-01" {
		t.Fatalf("业务参数不应丢失: %+v", job.Params)
	}
	if job.CreatedBy != "alice" {
		t.Fatalf("创建人丢失: %+v", job)
	}
}

// 普通数据直接排队执行；解析失败不能阻断导出，但任务最终要落在 failed
// 并带上原因，而不是永远停在 running。
func TestCreateExportQueuedRunsAndFails(t *testing.T) {
	svc, ws := newSvc(t)
	job, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "not_exists_metric", nil, "alice")
	if err != nil {
		t.Fatalf("CreateExport 失败: %v", err)
	}
	if job.Status != "queued" {
		t.Fatalf("普通导出应为 queued，实际 %s", job.Status)
	}
	done := waitStatus(t, svc, ws.ID, job.ID)
	if done.Status != "failed" || done.Error == "" {
		t.Fatalf("目标不存在时应失败并留原因: %+v", done)
	}
}

// 审批状态机：只有 pending_approval 能批准/驳回，驳回必须说明理由。
func TestExportApproveRejectStateMachine(t *testing.T) {
	svc, ws := newSvc(t)
	pending := func(target string) *model.ExportJob {
		job, err := svc.Repo.CreateExport(ctxBG(), model.ExportJob{
			WorkspaceID: ws.ID, Kind: "metric", Target: target,
			Params: map[string]any{"_classification": "high"}, Status: "pending_approval", CreatedBy: "alice",
		})
		if err != nil {
			t.Fatalf("建待审批任务失败: %v", err)
		}
		return job
	}

	job := pending("gmv_approve")
	approved, err := svc.ApproveExport(ctxBG(), ws.ID, job.ID, "admin")
	if err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if approved.Status != "approved" {
		t.Fatalf("批准后状态应为 approved，实际 %s", approved.Status)
	}
	// 异步执行会失败（指标不存在），但状态机必须继续推进。
	if done := waitStatus(t, svc, ws.ID, job.ID); done.Status != "failed" {
		t.Fatalf("批准后的执行结果应落库: %+v", done)
	}
	// 已批准的不能再次批准。
	if _, err := svc.ApproveExport(ctxBG(), ws.ID, job.ID, "admin"); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("重复批准应报冲突，实际 %v", err)
	}

	job2 := pending("gmv_reject")
	if _, err := svc.RejectExport(ctxBG(), ws.ID, job2.ID, "  ", "admin"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("空理由应被拒绝，实际 %v", err)
	}
	rejected, err := svc.RejectExport(ctxBG(), ws.ID, job2.ID, "数据过于敏感", "admin")
	if err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if rejected.Status != "rejected" || rejected.Error != "数据过于敏感" {
		t.Fatalf("驳回结果不对: %+v", rejected)
	}
	// 驳回后不能再批准。
	if _, err := svc.ApproveExport(ctxBG(), ws.ID, job2.ID, "admin"); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("驳回后批准应报冲突，实际 %v", err)
	}
	if _, err := svc.RejectExport(ctxBG(), "other-ws", job2.ID, "x", "admin"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("跨工作区应 NotFound，实际 %v", err)
	}

	audits, err := svc.Repo.ListAudit(ctxBG(), ws.ID, 20)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	var sawApprove, sawReject bool
	for _, a := range audits {
		if a.Action == "export.approve" {
			sawApprove = true
		}
		if a.Action == "export.reject" {
			sawReject = true
		}
	}
	if !sawApprove || !sawReject {
		t.Fatalf("审批动作必须留审计: %+v", audits)
	}
}

// 指标导出要产出可读 CSV：表头、行数、文件落盘三者一致。
func TestRunExportMetricWritesCSV(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_exp_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,10,'east'),('2026-08-01',2,5,'west'),('2026-08-02',3,7,'east')`, p.table("dwd_exp_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_exp_run", p.schema, "dwd_exp_t"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_export", "dwd_exp_run")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}

	job, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "gmv_export",
		map[string]any{"from": "2026-08-01", "to": "2026-08-03", "granularity": "day"}, "alice")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	got := waitStatus(t, svc, ws.ID, job.ID)
	if got.Status != "success" || got.RowCount != 2 {
		t.Fatalf("导出应成功且 2 天各一行: %+v", got)
	}
	raw, err := os.ReadFile(got.FilePath)
	if err != nil {
		t.Fatalf("导出文件应存在: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || lines[0] != "date,value" {
		t.Fatalf("CSV 内容不对: %q", lines)
	}
	if !strings.Contains(lines[1], "2026-08-01") {
		t.Fatalf("应按日期升序输出: %q", lines)
	}

	// 带维度时表头多一列，且每行带上维度值。
	job2, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "gmv_export",
		map[string]any{"from": "2026-08-01", "to": "2026-08-03", "dimension": "region"}, "alice")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	got2 := waitStatus(t, svc, ws.ID, job2.ID)
	if got2.Status != "success" || got2.RowCount != 3 {
		t.Fatalf("按维度导出应 3 行: %+v", got2)
	}
	raw2, _ := os.ReadFile(got2.FilePath)
	if !strings.Contains(string(raw2), "date,dimension,value") {
		t.Fatalf("维度导出表头不对: %q", string(raw2))
	}
}

// 不支持的导出类型必须在执行时失败并清理临时文件，不能留下半成品。
func TestRunExportUnsupportedKind(t *testing.T) {
	svc, ws := newSvc(t)
	job, err := svc.CreateExport(ctxBG(), ws.ID, "unknown", "x", nil, "alice")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	got := waitStatus(t, svc, ws.ID, job.ID)
	if got.Status != "failed" || !strings.Contains(got.Error, "不支持的导出类型") {
		t.Fatalf("不支持类型应失败: %+v", got)
	}
	if got.FilePath != "" {
		t.Fatalf("失败时不应留下文件路径: %+v", got)
	}
}

// 看板导出是宽表：每行一个日期，每列一张卡片。
func TestRunExportDashboardWideTable(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_dash_t"))
	// 看板卡片的窗口由 RunsCards 按「今天往前 N 天」计算，数据必须落在近几天内。
	d1 := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	d2 := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	p.exec(t, `INSERT INTO %s VALUES ('%s',1,10,'east'),('%s',2,20,'east')`, p.table("dwd_dash_t"), d1, d2)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dash_run", p.schema, "dwd_dash_t"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_dash", "dwd_dash_run")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, dashboardFor("dash_export", "gmv_dash")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	job, err := svc.CreateExport(ctxBG(), ws.ID, "dashboard", "dash_export", nil, "alice")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	got := waitStatus(t, svc, ws.ID, job.ID)
	if got.Status != "success" || got.RowCount != 2 {
		t.Fatalf("看板导出应 2 行: %+v", got)
	}
	raw, _ := os.ReadFile(got.FilePath)
	head := strings.Split(strings.Split(string(raw), "\n")[0], ",")
	if len(head) != 2 || head[0] != "date" || head[1] != "GMV" {
		t.Fatalf("宽表表头应为 date + 卡片标题: %q", head)
	}
}

// 订阅的 file 渠道把结果落成 JSON 并更新运行状态；未知渠道必须明确报错。
func TestRunSubscriptionFileAndUnknownChannel(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_sub_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,10,'east')`, p.table("dwd_sub_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_sub_run", p.schema, "dwd_sub_t"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_sub", "dwd_sub_run")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}

	sub, err := svc.Repo.SaveSubscription(ctxBG(), ws.ID, model.Subscription{
		Name: "gmv_file", Kind: "metric", Target: "gmv_sub", Channel: "file", Enabled: true,
		Config: map[string]any{"days": float64(7)},
	})
	if err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	res, err := svc.RunSubscription(ctxBG(), ws.ID, *sub)
	if err != nil {
		t.Fatalf("RunSubscription 失败: %v", err)
	}
	path, _ := res["path"].(string)
	if path == "" {
		t.Fatalf("应返回文件路径: %+v", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("订阅文件应落盘: %v", err)
	}

	bad := model.Subscription{Name: "bad", Kind: "metric", Target: "gmv_sub", Channel: "pigeon"}
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, bad); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未知渠道应报参数错误，实际 %v", err)
	}
	// webhook 缺 endpoint 也要在发请求前拒绝。
	badWebhook := model.Subscription{Name: "wh", Kind: "metric", Target: "gmv_sub", Channel: "webhook"}
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, badWebhook); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 endpoint 应报参数错误，实际 %v", err)
	}
}
