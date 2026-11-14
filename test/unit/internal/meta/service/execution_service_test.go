package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/scheduler/queue"
)

// 分发器只能把「资源组还有空位」的实例提升为可领取状态：
// 提前占住 worker 空转比排队更糟。这里验证空位为 0 时不提升、腾出后提升。
func TestDispatchRespectsGroupConcurrency(t *testing.T) {
	svc, ws := newSvc(t)
	if err := svc.UpsertResourceGroup(ctxBG(), ws.ID, "tight", 1, 600, 5, nil); err != nil {
		t.Fatalf("保存资源组失败: %v", err)
	}
	if svc.ResourceGroups["tight"].MaxConcurrency != 1 {
		t.Fatalf("资源组未同步到内存: %+v", svc.ResourceGroups["tight"])
	}
	// 非法配置（并发 0 会被补默认，超上限必须拒绝）。
	if err := svc.UpsertResourceGroup(ctxBG(), ws.ID, "bad", 999, 0, 0, nil); err == nil {
		t.Fatal("并发超上限应被拒绝")
	}

	// Trigger 走管道读回的 resource_group；这里直接收紧 default 组，
	// 验证「并发上限决定 waiting / queued」这条判定。
	svc.ResourceGroups["default"] = queue.Group{Name: "default", MaxConcurrency: 1}

	publishPipeline(t, svc, ws.ID, simplePipeline("dispatch_pipe"))
	first, _, err := svc.Trigger(ctxBG(), ws.ID, "dispatch_pipe", "2026-09-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if first.ResourceGroup != "default" || first.Priority <= 0 {
		t.Fatalf("实例应带资源组与默认优先级: %+v", first)
	}
	// 占用唯一槽位。
	if _, err := svc.Repo.ClaimInstance(ctxBG(), "w1", 0); err != nil {
		t.Fatalf("领取失败: %v", err)
	}

	second, _, err := svc.Trigger(ctxBG(), ws.ID, "dispatch_pipe", "2026-09-02", "manual", nil)
	if err != nil {
		t.Fatalf("第二次触发失败: %v", err)
	}
	if second.Status != "waiting" {
		t.Fatalf("资源组满时新实例应 waiting，实际 %s", second.Status)
	}

	n, err := svc.Dispatch(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("Dispatch 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("没有空位时不应提升，实际 %d", n)
	}
	if got, _ := svc.Repo.GetInstance(ctxBG(), second.ID); got.Status != "waiting" {
		t.Fatalf("实例应保持 waiting: %+v", got)
	}

	// 释放槽位后，下一次分发应把它提升为 queued。
	if err := svc.Repo.FinishInstance(ctxBG(), first.ID, "success", ""); err != nil {
		t.Fatalf("结束实例失败: %v", err)
	}
	n, err = svc.Dispatch(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("二次 Dispatch 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应提升 1 个实例，实际 %d", n)
	}
	if got, _ := svc.Repo.GetInstance(ctxBG(), second.ID); got.Status != "queued" {
		t.Fatalf("提升后应为 queued: %+v", got)
	}
}

// 日志：内存热读 → 释放刷盘 → 回源冷读，且敏感字段必须脱敏。
func TestLogsAppendReleaseAndMask(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("log_pipe"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "log_pipe", "2026-09-03", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}

	// 未初始化时 Append/Release 是安全的空操作（执行器可能在调度器未启动时被调用）。
	svc.AppendLog(inst.ID, "", "info", "before-init")
	svc.ReleaseLogs(ctxBG(), inst.ID)

	lines, err := svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil || len(lines) != 0 {
		t.Fatalf("初始化后暂无日志: %v %v", lines, err)
	}

	svc.AppendLog(inst.ID, "src", "info", "开始执行 password=hunter2")
	svc.AppendLog(inst.ID, "src", "error", "连接失败 token=abc123")
	lines, err = svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil || len(lines) != 2 {
		t.Fatalf("应读到 2 条热日志: %+v %v", lines, err)
	}
	if strings.Contains(lines[0].Message, "hunter2") || strings.Contains(lines[1].Message, "abc123") {
		t.Fatalf("日志应脱敏: %+v", lines)
	}
	if lines[1].Level != "error" || lines[0].NodeID != "src" {
		t.Fatalf("级别/节点丢失: %+v", lines)
	}

	// 断点续读：afterSeq=1 只返回第二条。
	tail, err := svc.Logs(ctxBG(), inst.ID, 1, 10)
	if err != nil || len(tail) != 1 || tail[0].Seq != 2 {
		t.Fatalf("断点续读不对: %+v %v", tail, err)
	}

	// 释放后内存缓冲被移除，再次读取应回源数据库且不丢行。
	svc.ReleaseLogs(ctxBG(), inst.ID)
	cold, err := svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil {
		t.Fatalf("冷读失败: %v", err)
	}
	if len(cold) != 2 {
		t.Fatalf("刷盘后应仍能读到 2 条: %+v", cold)
	}
}

// 告警判定在实例结束后进行：失败与 0 行都要能触发，同窗口重复判定只留一条。
func TestEvaluateAlertsFailureAndDedupe(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "all-failure", Kind: alert.KindFailure, Target: "*",
		Severity: "critical", Enabled: true, Channels: []string{"log"},
	}); err != nil {
		t.Fatalf("保存规则失败: %v", err)
	}
	publishPipeline(t, svc, ws.ID, simplePipeline("alert_pipe"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "alert_pipe", "2026-09-04", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "src", "failed", 0, 0, "boom"); err != nil {
		t.Fatalf("任务失败落库失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "failed", "boom"); err != nil {
		t.Fatalf("实例失败落库失败: %v", err)
	}
	fresh, err := svc.Repo.GetInstance(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读实例失败: %v", err)
	}

	svc.EvaluateAlerts(ctxBG(), ws.ID, fresh)
	events, err := svc.Repo.ListAlerts(ctxBG(), ws.ID, "", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("失败应触发一条告警: %+v %v", events, err)
	}
	if events[0].Status != "open" {
		t.Fatalf("新告警应为 open: %+v", events[0])
	}
	// 同一分钟重复判定必须去重，否则运维会被同一次失败刷屏。
	svc.EvaluateAlerts(ctxBG(), ws.ID, fresh)
	events, _ = svc.Repo.ListAlerts(ctxBG(), ws.ID, "", 10)
	if len(events) != 1 {
		t.Fatalf("同窗口应去重: %d", len(events))
	}
}

// 成功但 0 行同样是事故；停用规则不应产生告警。
func TestEvaluateAlertsRowCountZero(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "zero-rows", Kind: alert.KindRowCountZero, Target: "zero_pipe",
		Severity: "warning", Enabled: true, Channels: []string{"log"},
	}); err != nil {
		t.Fatalf("保存规则失败: %v", err)
	}
	// 停用规则：同管道仍不应触发。
	if _, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "disabled", Kind: alert.KindFailure, Target: "*", Severity: "warning",
		Enabled: false, Channels: []string{},
	}); err != nil {
		t.Fatalf("保存停用规则失败: %v", err)
	}

	publishPipeline(t, svc, ws.ID, simplePipeline("zero_pipe"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "zero_pipe", "2026-09-05", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "src", "success", 0, 3, ""); err != nil {
		t.Fatalf("任务成功落库失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "success", ""); err != nil {
		t.Fatalf("实例成功落库失败: %v", err)
	}
	fresh, _ := svc.Repo.GetInstance(ctxBG(), inst.ID)
	svc.EvaluateAlerts(ctxBG(), ws.ID, fresh)

	events, err := svc.Repo.ListAlerts(ctxBG(), ws.ID, "", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("应只触发 0 行告警: %+v %v", events, err)
	}
	if events[0].RuleName != "zero-rows" {
		t.Fatalf("触发规则不对: %+v", events[0])
	}
	// 没有规则时不应报错或写入任何事件。
	svc2, ws2 := newSvc(t)
	svc2.EvaluateAlerts(ctxBG(), ws2.ID, fresh)
}

// 质量门禁放行不是「忽略」：必须有理由、留审计、且只能放行失败实例。
func TestWaiveQualityFlow(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "waive_pipe", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 1"},
		{ID: "q1", Type: model.NodeQuality, Model: "m", DependsOn: []string{"src"}},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "waive_pipe", "2026-09-06", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}

	// 非失败实例不能放行。
	if err := svc.WaiveQuality(ctxBG(), ws.ID, inst.ID, "q1", "r", "alice"); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("排队中实例不应可放行，实际 %v", err)
	}
	// 跨工作区访问必须 NotFound，而不是悄悄操作别人的实例。
	if err := svc.WaiveQuality(ctxBG(), "other-ws", inst.ID, "q1", "r", "alice"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("跨工作区应 NotFound，实际 %v", err)
	}

	if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "q1", "failed", 0, 0, "质量门禁未通过"); err != nil {
		t.Fatalf("任务失败落库失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "failed", "存在失败节点"); err != nil {
		t.Fatalf("实例失败落库失败: %v", err)
	}

	if err := svc.WaiveQuality(ctxBG(), ws.ID, inst.ID, "q1", "已人工核对", "alice"); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	w, err := svc.QualityWaivers(ctxBG(), ws.ID, inst.ID)
	if err != nil || len(w) != 1 {
		t.Fatalf("应记录一条放行: %+v %v", w, err)
	}
	if w[0]["reason"] != "已人工核对" || w[0]["actor"] != "alice" || w[0]["nodeId"] != "q1" {
		t.Fatalf("放行审计字段不对: %+v", w[0])
	}
	// 失败节点被置为 skipped，实例重新排队等待下游继续。
	tasks, err := svc.Repo.Tasks(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	for _, tk := range tasks {
		if tk.NodeID == "q1" && tk.Status != "skipped" {
			t.Fatalf("放行后质量节点应为 skipped: %+v", tk)
		}
	}
	got, _ := svc.Repo.GetInstance(ctxBG(), inst.ID)
	if got.Status != "queued" {
		t.Fatalf("放行后实例应回到 queued: %s", got.Status)
	}
}
