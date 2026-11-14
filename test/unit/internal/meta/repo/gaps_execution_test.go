package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/obs/logstore"
)

// 本文件覆盖执行面：告警规则/事件、资源组、运行日志回读、放行记录。

// 告警阈值列改成文本（结构漂移）时规则列表必须报错：
// 阈值读错会让「数据量跌 20%」变成一条永不触发的规则。
func TestListAlertRulesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveAlertRule(context.Background(), ws.ID, alertRuleSample()); err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	textColumn(t, db, "alert_rule", "threshold")

	_, err := r.ListAlertRules(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 去重键命中但回查失败时，EmitAlert 必须报错：
// 静默返回空事件会让上层以为「已去重」而不再发送。
func TestEmitAlertFailsWhenDedupeLookupBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	// 触发器返回 NULL：INSERT 被丢弃，INSERT ... RETURNING 一行都不返回，
	// EmitAlert 因此走「去重命中，回查原事件」的分支；此时表里也没有这行。
	swallowInsert(t, db, "alert_event")

	_, created, err := r.EmitAlert(context.Background(), ws.ID, alertEventSample())
	wantErr(t, err, "no rows")
	if created {
		t.Fatal("写入被丢弃时不应报告「新建成功」")
	}
}

// 已推送标记列漂移时告警列表必须报错。
func TestListAlertsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	rule, err := r.SaveAlertRule(context.Background(), ws.ID, alertRuleSample())
	if err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	// 事件有 rule_id 外键，必须挂到刚建出来的规则上。
	if _, _, err := r.EmitAlert(context.Background(), ws.ID, alert.Event{
		RuleID: rule.ID, RuleName: rule.Name, Title: "t",
	}); err != nil {
		t.Fatalf("写告警事件失败: %v", err)
	}
	textColumn(t, db, "alert_event", "notified")

	_, err = r.ListAlerts(context.Background(), ws.ID, "", 10)
	wantErr(t, err, "scan")
}

// 资源组的引擎列表被写成文本（结构漂移）时资源组列表必须报错，
// 否则调度器会以为「没有引擎限制」，把任务发到不支持的执行引擎上。
func TestListResourceGroupsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.UpsertResourceGroup(context.Background(), ws.ID, "default", 4, 3600, 10, []string{"trino"}); err != nil {
		t.Fatalf("写资源组失败: %v", err)
	}
	textColumn(t, db, "resource_group", "engines")

	_, err := r.ListResourceGroups(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 日志回读的索引列缺失时必须报错：日志页宁可直接失败，
// 也不要显示「没有日志」——那会把排查方向带偏。
func TestLogReadAfterFailsWhenSeqColumnMissing(t *testing.T) {
	_, db := isolatedRepo(t)
	renameColumn(t, db, "run_log", "seq", "seq_drift")

	_, err := repo.LogSink{DB: db}.ReadAfter(context.Background(), "ins", 0, 10)
	wantErr(t, err, "seq")
}

// 日志写入时间列漂移时必须报错（前端要靠它排序展示）。
func TestLogReadAfterFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, instanceSample(ws.ID, pl.ID))
	if err := (repo.LogSink{DB: db}).WriteBatch(context.Background(), inst.ID,
		[]logstore.Line{{Seq: 1, Level: "info", Message: "hi"}}); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
	textColumn(t, db, "run_log", "created_at")

	_, err := repo.LogSink{DB: db}.ReadAfter(context.Background(), inst.ID, 0, 10)
	wantErr(t, err, "scan")
}

// 放行记录的创建时间列漂移时必须报错：质量门禁的豁免本身就是审计证据。
func TestListQualityWaiversFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, instanceSample(ws.ID, pl.ID))
	if err := r.RecordQualityWaiver(context.Background(), ws.ID, inst.ID, "n1", "已知问题", "alice"); err != nil {
		t.Fatalf("写放行记录失败: %v", err)
	}
	textColumn(t, db, "quality_waiver", "created_at")

	_, err := r.ListQualityWaivers(context.Background(), ws.ID, inst.ID)
	wantErr(t, err, "scan")
}
