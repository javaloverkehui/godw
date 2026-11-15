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

	// 日志管理器随 Service 构造即就绪：没人打开过日志页时也不能丢日志。
	svc.AppendLog(inst.ID, "", "", "info", "before-init")
	svc.ReleaseLogs(ctxBG(), inst.ID)

	lines, err := svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil || len(lines) != 1 || lines[0].InstanceID != inst.ID {
		t.Fatalf("释放前写入的日志应已落库并带实例标识: %+v %v", lines, err)
	}

	svc.AppendLog(inst.ID, "src", "tsk_src", "info", "开始执行 password=hunter2")
	svc.AppendLog(inst.ID, "src", "tsk_src", "error", "连接失败 token=abc123")
	lines, err = svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil || len(lines) != 3 {
		t.Fatalf("应读到 3 条（含释放前那条）: %+v %v", lines, err)
	}
	// 重跑续号：释放后新建的缓冲必须接着库里的 seq，而不是从 1 重来。
	if lines[1].Seq != 2 || lines[2].Seq != 3 {
		t.Fatalf("seq 应从库内最大值续号: %+v", lines)
	}
	if strings.Contains(lines[1].Message, "hunter2") || strings.Contains(lines[2].Message, "abc123") {
		t.Fatalf("日志应脱敏: %+v", lines)
	}
	if lines[2].Level != "error" || lines[1].NodeID != "src" || lines[1].TaskID != "tsk_src" {
		t.Fatalf("级别/节点/任务丢失: %+v", lines)
	}

	// 断点续读：afterSeq=2 只返回最后一条。
	tail, err := svc.Logs(ctxBG(), inst.ID, 2, 10)
	if err != nil || len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("断点续读不对: %+v %v", tail, err)
	}

	// 释放后内存缓冲被移除，再次读取应回源数据库且不丢行。
	svc.ReleaseLogs(ctxBG(), inst.ID)
	cold, err := svc.Logs(ctxBG(), inst.ID, 0, 10)
	if err != nil {
		t.Fatalf("冷读失败: %v", err)
	}
	if len(cold) != 3 {
		t.Fatalf("刷盘后应仍能读到 3 条: %+v", cold)
	}
	if cold[0].InstanceID != inst.ID || cold[1].TaskID != "tsk_src" {
		t.Fatalf("冷读应恢复实例/任务标识: %+v", cold)
	}
}

// 执行日志必须「从开始到结束」完整落库，并且每一行都能对上是哪个实例、哪个任务。
// 这条用例守住的是排查体验：没人打开日志页时也不能丢日志，taskId 不能空。
func TestRunOnceWritesStructuredLogs(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "log_full", Nodes: []model.NodeSpec{
		{ID: "b", Type: model.NodeBranch},
	}}
	publishPipeline(t, svc, ws.ID, pipe)

	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "log_full", "2026-09-07", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "w1"); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// 执行结束后缓冲已释放，这里读到的是真正落库的内容。
	lines, err := svc.Logs(ctxBG(), inst.ID, 0, 1000)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	if len(lines) < 5 {
		t.Fatalf("日志应覆盖开始/计划/节点开始/节点成功/结束，实际 %d 条: %+v", len(lines), lines)
	}
	tasks, err := svc.Repo.Tasks(ctxBG(), inst.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("读任务失败: %+v %v", tasks, err)
	}
	taskID := tasks[0].ID
	var hasStart, hasPlan, hasNodeStart, hasNodeOK, hasEnd bool
	for _, l := range lines {
		if l.InstanceID != inst.ID {
			t.Fatalf("每行都要带实例标识: %+v", l)
		}
		switch {
		case strings.Contains(l.Message, "实例开始"):
			hasStart = true
			if l.TaskID != "" {
				t.Fatalf("实例级日志不应带 taskId: %+v", l)
			}
		case strings.Contains(l.Message, "执行计划"):
			hasPlan = true
		case l.NodeID == "b" && strings.Contains(l.Message, "开始执行"):
			hasNodeStart = true
			if l.TaskID != taskID {
				t.Fatalf("节点日志的 taskId 应等于 task_instance.id: %+v", l)
			}
		case l.NodeID == "b" && strings.Contains(l.Message, "执行成功"):
			hasNodeOK = true
			if l.TaskID != taskID || l.Level != "info" {
				t.Fatalf("成功日志级别/任务不对: %+v", l)
			}
		case strings.Contains(l.Message, "实例结束"):
			hasEnd = true
			if !strings.Contains(l.Message, "status=success") {
				t.Fatalf("结束日志应带最终状态: %+v", l)
			}
		}
	}
	if !hasStart || !hasPlan || !hasNodeStart || !hasNodeOK || !hasEnd {
		t.Fatalf("日志阶段不完整 start=%v plan=%v nodeStart=%v nodeOK=%v end=%v: %+v",
			hasStart, hasPlan, hasNodeStart, hasNodeOK, hasEnd, lines)
	}
}

// 失败路径同样要有 ERROR 行，并且带实例/任务标识——这正是「筛 ERROR 却暂无日志」的反面。
func TestRunOnceFailureLogsCarryIDs(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "log_fail", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSource, Datasource: "missing_ds", SQL: "SELECT 1"},
	}}
	publishPipeline(t, svc, ws.ID, pipe)

	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "log_fail", "2026-09-08", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "w1"); err == nil {
		t.Fatal("数据源不存在时应执行失败")
	}
	lines, err := svc.Logs(ctxBG(), inst.ID, 0, 1000)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	tasks, _ := svc.Repo.Tasks(ctxBG(), inst.ID)
	if len(tasks) != 1 {
		t.Fatalf("应有一个任务: %+v", tasks)
	}
	var errLines, nodeErrLines int
	for _, l := range lines {
		if l.Level != "error" {
			continue
		}
		errLines++
		if l.InstanceID != inst.ID {
			t.Fatalf("ERROR 行必须带实例标识: %+v", l)
		}
		if l.NodeID != "" {
			nodeErrLines++
			if l.TaskID != tasks[0].ID {
				t.Fatalf("节点 ERROR 行必须带任务标识: %+v", l)
			}
		}
	}
	if errLines == 0 || nodeErrLines == 0 {
		t.Fatalf("失败执行应留下 ERROR 日志（含节点级）: %+v", lines)
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
