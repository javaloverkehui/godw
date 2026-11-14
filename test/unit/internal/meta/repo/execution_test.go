package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/obs/logstore"
)

// 告警规则按名称 upsert：同一条规则改阈值/通道不应该产生第二行，
// 否则巡检会重复触发同一条告警。
func TestAlertRuleUpsertAndList(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	rule := alert.Rule{
		Name: "sla_daily", Kind: alert.KindDeadline, Target: "p1", Deadline: "08:00",
		Severity: "critical", Channels: []string{"webhook"}, Enabled: true,
	}
	saved, err := r.SaveAlertRule(ctx, ws.ID, rule)
	if err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	if saved.ID == "" || saved.WorkspaceID != ws.ID {
		t.Fatalf("应回填 ID 与工作区: %+v", saved)
	}

	// 同名更新：阈值与通道变化，行数不变，且保留原 ID。
	rule.Threshold = 30
	rule.Channels = []string{"webhook", "email"}
	updated, err := r.SaveAlertRule(ctx, ws.ID, rule)
	if err != nil {
		t.Fatalf("更新告警规则失败: %v", err)
	}
	// upsert 命中时不改 id：库里的行仍是第一次那条（返回值里的 ID 是新生成的，
	// 落库时被 ON CONFLICT 忽略，用例据此断言真实行为）。
	if got := queryStr(t, db, `SELECT id FROM alert_rule WHERE workspace_id=$1 AND name='sla_daily'`, ws.ID); got != saved.ID {
		t.Fatalf("同名 upsert 应保留原 id: %s != %s", got, saved.ID)
	}
	if updated.Name != "sla_daily" {
		t.Fatalf("返回值应带上规则名: %+v", updated)
	}
	// 另一个规则用来验证排序。
	if _, err := r.SaveAlertRule(ctx, ws.ID, alert.Rule{
		Name: "a_volume", Kind: alert.KindVolumeDrop, Target: "*", Threshold: 0.5,
		Severity: "warning", Channels: []string{}, Enabled: false,
	}); err != nil {
		t.Fatalf("保存第二条规则失败: %v", err)
	}
	// 跨工作区同名规则互不影响。
	if _, err := r.SaveAlertRule(ctx, other.ID, rule); err != nil {
		t.Fatalf("其他工作区保存失败: %v", err)
	}

	rules, err := r.ListAlertRules(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列告警规则失败: %v", err)
	}
	if len(rules) != 2 || rules[0].Name != "a_volume" || rules[1].Name != "sla_daily" {
		t.Fatalf("应按名称排序且只含本工作区: %+v", rules)
	}
	if rules[1].Kind != alert.KindDeadline || rules[1].Threshold != 30 || rules[1].Deadline != "08:00" {
		t.Fatalf("规则字段不对: %+v", rules[1])
	}
	if len(rules[1].Channels) != 2 || rules[1].Channels[1] != "email" {
		t.Fatalf("通道应往返: %+v", rules[1].Channels)
	}
	if rules[0].Enabled {
		t.Fatalf("停用状态应保留: %+v", rules[0])
	}
	if rules[1].WorkspaceID != ws.ID {
		t.Fatalf("列表应回填工作区: %+v", rules[1])
	}
	if n := queryInt(t, db, `SELECT count(*) FROM alert_rule WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("upsert 不应产生多余行: %d", n)
	}

	// kind 受 CHECK 约束。
	if _, err := r.SaveAlertRule(ctx, ws.ID, alert.Rule{
		Name: "bad", Kind: "unknown", Target: "*", Channels: []string{},
	}); err == nil {
		t.Fatal("非法告警类型应被拒绝")
	}

	if err := r.DeleteAlertRule(ctx, ws.ID, "a_volume"); err != nil {
		t.Fatalf("删除规则失败: %v", err)
	}
	if rules, err := r.ListAlertRules(ctx, ws.ID); err != nil || len(rules) != 1 {
		t.Fatalf("删除后应剩 1 条: %v %v", rules, err)
	}
	// 删不存在的规则不报错（幂等删除）。
	if err := r.DeleteAlertRule(ctx, ws.ID, "nope"); err != nil {
		t.Fatalf("删除不存在的规则应成功: %v", err)
	}
}

// 告警事件按 dedupe_key 去重：同窗口内只产生一条，重复触发返回已存在的那条。
func TestEmitAlertDedupe(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	rule, err := r.SaveAlertRule(ctx, ws.ID, alert.Rule{
		Name: "fail_p1", Kind: alert.KindFailure, Target: "p1", Severity: "critical",
		Channels: []string{}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("准备规则失败: %v", err)
	}

	first, created, err := r.EmitAlert(ctx, ws.ID, alert.Event{
		RuleID: rule.ID, RuleName: rule.Name, Pipeline: "p1", InstanceID: "ins_1",
		Severity: "critical", Title: "p1 执行失败", Detail: "SQL error", DedupeKey: "p1:fail:2024-01-01",
	})
	if err != nil {
		t.Fatalf("发送告警失败: %v", err)
	}
	if !created {
		t.Fatal("首次发送 created 应为 true")
	}
	if first.Status != "open" || first.Notified || first.CreatedAt.IsZero() {
		t.Fatalf("新事件状态不对: %+v", first)
	}
	if first.DedupeKey != "p1:fail:2024-01-01" || first.Severity != "critical" {
		t.Fatalf("事件字段不对: %+v", first)
	}

	second, created, err := r.EmitAlert(ctx, ws.ID, alert.Event{
		RuleID: rule.ID, RuleName: rule.Name, Pipeline: "p1", InstanceID: "ins_2",
		Severity: "critical", Title: "又一个失败", DedupeKey: "p1:fail:2024-01-01",
	})
	if err != nil {
		t.Fatalf("重复发送不应报错: %v", err)
	}
	if created {
		t.Fatal("去重命中时 created 应为 false")
	}
	if second.ID != first.ID || second.Title != "p1 执行失败" || second.InstanceID != "ins_1" {
		t.Fatalf("应返回原事件而不是新事件: %+v", second)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM alert_event`); n != 1 {
		t.Fatalf("去重后只应有 1 条事件: %d", n)
	}

	// dedupe_key 为空时不参与去重（部分唯一索引），可以有任意多条。
	for i := 0; i < 2; i++ {
		if _, created, err := r.EmitAlert(ctx, ws.ID, alert.Event{
			RuleID: rule.ID, RuleName: rule.Name, Severity: "warning", Title: "无去重键",
		}); err != nil || !created {
			t.Fatalf("空 dedupe_key 每次都应新建: %v %v", created, err)
		}
	}
	if n := queryInt(t, db, `SELECT count(*) FROM alert_event`); n != 3 {
		t.Fatalf("应共有 3 条事件，实际 %d", n)
	}

	// rule_id 外键必须存在。
	if _, _, err := r.EmitAlert(ctx, ws.ID, alert.Event{RuleID: "alr_missing", Title: "x"}); err == nil {
		t.Fatal("规则不存在应触发外键错误")
	}
}

// 告警处理流程：open → acknowledged → resolved，并记录处理人；推送标记独立更新。
func TestAlertEventHandlingAndList(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	rule, err := r.SaveAlertRule(ctx, ws.ID, alert.Rule{
		Name: "r1", Kind: alert.KindFailure, Target: "*", Severity: "warning",
		Channels: []string{}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("准备规则失败: %v", err)
	}

	mk := func(title, dedupe string) *repo.AlertEvent {
		t.Helper()
		ev, _, err := r.EmitAlert(ctx, ws.ID, alert.Event{
			RuleID: rule.ID, RuleName: rule.Name, Pipeline: "p1", Severity: "warning",
			Title: title, DedupeKey: dedupe,
		})
		if err != nil {
			t.Fatalf("发送告警失败: %v", err)
		}
		return ev
	}
	a := mk("第一条", "k1")
	b := mk("第二条", "k2")
	// 拉开时间，验证列表倒序。
	execSQL(t, db, `UPDATE alert_event SET created_at = now() - interval '2 hours' WHERE id=$1`, a.ID)
	execSQL(t, db, `UPDATE alert_event SET created_at = now() - interval '1 hour' WHERE id=$1`, b.ID)

	if err := r.MarkAlertNotified(ctx, a.ID); err != nil {
		t.Fatalf("标记已推送失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT CASE WHEN notified THEN 1 ELSE 0 END FROM alert_event WHERE id=$1`, a.ID); n != 1 {
		t.Fatalf("notified 应为 true: %d", n)
	}

	if err := r.HandleAlert(ctx, ws.ID, b.ID, "acknowledged", "alice"); err != nil {
		t.Fatalf("确认告警失败: %v", err)
	}
	if err := r.HandleAlert(ctx, ws.ID, b.ID, "resolved", "bob"); err != nil {
		t.Fatalf("解决告警失败: %v", err)
	}
	var status, by string
	var at *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT status, handled_by, handled_at FROM alert_event WHERE id=$1`, b.ID).
		Scan(&status, &by, &at); err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if status != "resolved" || by != "bob" || at == nil {
		t.Fatalf("处理状态不对: %s %q %v", status, by, at)
	}
	// 跨工作区处理无效：WHERE 带 workspace_id，别人的事件改不动。
	if err := r.HandleAlert(ctx, other.ID, b.ID, "resolved", "carol"); err != nil {
		t.Fatalf("跨工作区处理应静默无效果: %v", err)
	}
	if by := queryStr(t, db, `SELECT handled_by FROM alert_event WHERE id=$1`, b.ID); by != "bob" {
		t.Fatalf("跨工作区不应改进别人事件: %q", by)
	}

	// 列表默认按时间倒序。
	all, err := r.ListAlerts(ctx, ws.ID, "", 10)
	if err != nil {
		t.Fatalf("列告警失败: %v", err)
	}
	if len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("应按时间倒序: %+v", all)
	}
	if all[0].HandledBy != "bob" || all[0].Status != "resolved" {
		t.Fatalf("最新一条应带处理信息: %+v", all[0])
	}
	if !all[1].Notified {
		t.Fatalf("已推送标记应读回: %+v", all[1])
	}

	// 按状态过滤。
	resolved, err := r.ListAlerts(ctx, ws.ID, "resolved", 10)
	if err != nil {
		t.Fatalf("状态过滤失败: %v", err)
	}
	if len(resolved) != 1 || resolved[0].ID != b.ID {
		t.Fatalf("resolved 过滤结果不对: %+v", resolved)
	}
	open, err := r.ListAlerts(ctx, ws.ID, "open", 10)
	if err != nil {
		t.Fatalf("open 过滤失败: %v", err)
	}
	if len(open) != 1 || open[0].ID != a.ID {
		t.Fatalf("open 过滤应只剩未处理那条: %+v", open)
	}
	if none, err := r.ListAlerts(ctx, ws.ID, "silenced", 10); err != nil || len(none) != 0 {
		t.Fatalf("不存在的状态应返回空集合: %v %v", none, err)
	}
	// limit 与默认值。
	if one, err := r.ListAlerts(ctx, ws.ID, "", 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 未生效: %v %v", one, err)
	}
	if two, err := r.ListAlerts(ctx, ws.ID, "", 0); err != nil || len(two) != 2 {
		t.Fatalf("非法 limit 应回落默认值: %v %v", two, err)
	}
	// 跨工作区隔离。
	if got, err := r.ListAlerts(ctx, other.ID, "", 10); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区不应看到事件: %v %v", got, err)
	}
}

// 资源组是调度的限流闸门：nil 引擎必须落成空数组（列是 NOT NULL），
// 同名重复配置只更新，不新增行。
func TestResourceGroupUpsert(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if err := r.UpsertResourceGroup(ctx, ws.ID, "etl", 4, 3600, 10, []string{"postgres", "clickhouse"}); err != nil {
		t.Fatalf("写资源组失败: %v", err)
	}
	// nil 切片不能变成 SQL NULL。
	if err := r.UpsertResourceGroup(ctx, ws.ID, "adhoc", 1, 60, 2, nil); err != nil {
		t.Fatalf("写资源组失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM resource_group WHERE name='adhoc' AND engines='{}'`); n != 1 {
		t.Fatalf("nil 引擎应写成空数组: %d", n)
	}
	// 同名更新。
	if err := r.UpsertResourceGroup(ctx, ws.ID, "etl", 8, 7200, 20, []string{"postgres"}); err != nil {
		t.Fatalf("更新资源组失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM resource_group WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("upsert 不应新增行: %d", n)
	}
	if err := r.UpsertResourceGroup(ctx, other.ID, "etl", 2, 30, 1, nil); err != nil {
		t.Fatalf("其他工作区写资源组失败: %v", err)
	}

	groups, err := r.ListResourceGroups(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列资源组失败: %v", err)
	}
	if len(groups) != 2 || groups[0]["name"] != "adhoc" || groups[1]["name"] != "etl" {
		t.Fatalf("应按名称排序: %+v", groups)
	}
	if groups[1]["maxConcurrency"] != 8 || groups[1]["maxTimeoutSec"] != 7200 || groups[1]["maxPool"] != 20 {
		t.Fatalf("更新未生效: %+v", groups[1])
	}
	if engines, ok := groups[1]["engines"].([]string); !ok || len(engines) != 1 || engines[0] != "postgres" {
		t.Fatalf("引擎列表不对: %#v", groups[1]["engines"])
	}
	if engines, ok := groups[0]["engines"].([]string); !ok || len(engines) != 0 {
		t.Fatalf("空引擎应是空数组: %#v", groups[0]["engines"])
	}
	if got, err := r.ListResourceGroups(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空集合: %v %v", got, err)
	}
}

// 运行日志的落库与回读：seq 决定顺序，afterSeq 用于 SSE 增量拉取。
func TestLogSinkWriteAndRead(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	inst := seedInstance(t, r, instSample(ws.ID, p.ID, "k", "2024-01-01", ""))
	sink := repo.LogSink{DB: db}

	// 空批次直接返回，不碰库。
	if err := sink.WriteBatch(ctx, inst.ID, nil); err != nil {
		t.Fatalf("空批次应成功: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM run_log`); n != 0 {
		t.Fatalf("空批次不应写库: %d", n)
	}

	base := time.Now().Truncate(time.Second)
	lines := []logstore.Line{
		{Seq: 1, NodeID: "n1", Level: "info", Message: "开始执行", At: base},
		{Seq: 2, NodeID: "n1", Level: "warn", Message: "慢查询", At: base.Add(time.Second)},
		{Seq: 3, NodeID: "n2", Level: "error", Message: "失败", At: base.Add(2 * time.Second)},
	}
	if err := sink.WriteBatch(ctx, inst.ID, lines); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM run_log WHERE instance_id=$1`, inst.ID); n != 3 {
		t.Fatalf("应写入 3 行日志，实际 %d", n)
	}

	got, err := sink.ReadAfter(ctx, inst.ID, 0, 10)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应读到 3 行，实际 %d", len(got))
	}
	if got[0].Seq != 1 || got[2].Seq != 3 {
		t.Fatalf("应按 seq 升序: %+v", got)
	}
	if got[1].Level != "warn" || got[1].Message != "慢查询" || got[1].NodeID != "n1" {
		t.Fatalf("日志字段不对: %+v", got[1])
	}
	if !got[0].At.Equal(base) {
		t.Fatalf("时间应往返: %v != %v", got[0].At, base)
	}

	// afterSeq 只取新行。
	after, err := sink.ReadAfter(ctx, inst.ID, 1, 10)
	if err != nil {
		t.Fatalf("增量读日志失败: %v", err)
	}
	if len(after) != 2 || after[0].Seq != 2 {
		t.Fatalf("afterSeq=1 应返回 seq 2、3: %+v", after)
	}
	// limit 生效。
	if one, err := sink.ReadAfter(ctx, inst.ID, 0, 1); err != nil || len(one) != 1 || one[0].Seq != 1 {
		t.Fatalf("limit 未生效: %+v %v", one, err)
	}
	// 其他实例读不到本实例日志。
	if none, err := sink.ReadAfter(ctx, "ins_missing", 0, 10); err != nil || len(none) != 0 {
		t.Fatalf("不存在的实例应返回空集合: %v %v", none, err)
	}
	// 外键约束：实例不存在时写入失败。
	if err := sink.WriteBatch(ctx, "ins_missing", lines[:1]); err == nil {
		t.Fatal("实例不存在时写日志应失败")
	}
}

// 质量放行：记录谁在什么时候放过了哪条门禁，列表按时间倒序。
func TestQualityWaiver(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if err := r.RecordQualityWaiver(ctx, ws.ID, "ins_1", "n1", "历史数据补录", "alice"); err != nil {
		t.Fatalf("记录放行失败: %v", err)
	}
	if err := r.RecordQualityWaiver(ctx, ws.ID, "ins_1", "n2", "紧急上线", "bob"); err != nil {
		t.Fatalf("记录第二条放行失败: %v", err)
	}
	if err := r.RecordQualityWaiver(ctx, ws.ID, "ins_2", "n1", "其他实例", "carol"); err != nil {
		t.Fatalf("记录其他实例放行失败: %v", err)
	}
	execSQL(t, db, `UPDATE quality_waiver SET created_at = now() - interval '1 hour' WHERE node_id='n1' AND instance_id='ins_1'`)

	list, err := r.ListQualityWaivers(ctx, ws.ID, "ins_1")
	if err != nil {
		t.Fatalf("列放行失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ins_1 应有 2 条放行，实际 %d: %+v", len(list), list)
	}
	if list[0]["nodeId"] != "n2" || list[0]["actor"] != "bob" || list[0]["reason"] != "紧急上线" {
		t.Fatalf("应按时间倒序: %+v", list[0])
	}
	if list[1]["nodeId"] != "n1" || list[1]["id"] == nil || list[1]["createdAt"] == nil {
		t.Fatalf("放行字段不完整: %+v", list[1])
	}
	// 其他实例的放行不算进来。
	if got, err := r.ListQualityWaivers(ctx, ws.ID, "ins_2"); err != nil || len(got) != 1 {
		t.Fatalf("ins_2 应只有 1 条: %v %v", got, err)
	}
	if got, err := r.ListQualityWaivers(ctx, other.ID, "ins_1"); err != nil || len(got) != 0 {
		t.Fatalf("跨工作区不应可见: %v %v", got, err)
	}
}
