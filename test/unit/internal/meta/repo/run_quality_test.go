package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
)

type qualityRow = struct {
	RuleName, Observed, Expected, Severity string
	Passed                                 bool
}

// 质量结果按 (实例, 节点) 追加写入，看板按规则聚合。
// 这里同时验证「写入读回一致」和「聚合口径（失败优先排序、通过率）」。
func TestQualityResultsAndOverview(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	p := seedPipeline(t, r, ws.ID, "p1")
	otherP := seedPipeline(t, r, other.ID, "p1")

	run1 := seedInstance(t, r, instSample(ws.ID, p.ID, "r1", "2024-01-01", "failed"))
	run2 := seedInstance(t, r, instSample(ws.ID, p.ID, "r2", "2024-01-02", "success"))
	foreign := seedInstance(t, r, instSample(other.ID, otherP.ID, "r3", "2024-01-01", "success"))

	if err := r.SaveQualityResults(ctx, run1.ID, "n1", []qualityRow{
		{RuleName: "id_not_null", Observed: "3", Expected: "0", Severity: "error", Passed: false},
		{RuleName: "amount_range", Observed: "0", Expected: "0", Severity: "warn", Passed: true},
	}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	if err := r.SaveQualityResults(ctx, run2.ID, "n1", []qualityRow{
		{RuleName: "id_not_null", Observed: "0", Expected: "0", Severity: "error", Passed: true},
	}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	// 其他工作区的结果不能混进来。
	if err := r.SaveQualityResults(ctx, foreign.ID, "n1", []qualityRow{
		{RuleName: "id_not_null", Observed: "9", Expected: "0", Severity: "error", Passed: false},
	}); err != nil {
		t.Fatalf("写入其他工作区结果失败: %v", err)
	}
	// 空结果集是合法的 no-op。
	if err := r.SaveQualityResults(ctx, run1.ID, "n2", nil); err != nil {
		t.Fatalf("空结果集应成功: %v", err)
	}

	got, err := r.QualityResults(ctx, run1.ID)
	if err != nil {
		t.Fatalf("读质量结果失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条结果，实际 %d", len(got))
	}
	byRule := map[string]model.QualityResult{}
	for _, q := range got {
		byRule[q.RuleName] = q
	}
	if byRule["id_not_null"].Passed || byRule["id_not_null"].Observed != "3" || byRule["id_not_null"].Severity != "error" {
		t.Fatalf("失败结果字段不对: %+v", byRule["id_not_null"])
	}
	if !byRule["amount_range"].Passed || byRule["amount_range"].Expected != "0" {
		t.Fatalf("通过结果字段不对: %+v", byRule["amount_range"])
	}
	if byRule["id_not_null"].InstanceID != run1.ID || byRule["id_not_null"].NodeID != "n1" {
		t.Fatalf("结果未关联实例/节点: %+v", byRule["id_not_null"])
	}
	// 不存在的实例没有任何结果，返回空集合。
	if empty, err := r.QualityResults(ctx, "ins_missing"); err != nil || len(empty) != 0 {
		t.Fatalf("不存在的实例应返回空集合: %v %v", empty, err)
	}

	// 概览只统计本工作区，按失败数倒序。
	overview, err := r.QualityOverview(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("质量概览失败: %v", err)
	}
	if len(overview) != 2 {
		t.Fatalf("应有 2 条规则，实际 %d: %+v", len(overview), overview)
	}
	if overview[0]["rule"] != "id_not_null" {
		t.Fatalf("失败数多的规则应排最前: %+v", overview)
	}
	if overview[0]["total"] != int64(2) || overview[0]["passed"] != int64(1) || overview[0]["failed"] != int64(1) {
		t.Fatalf("聚合计数不对: %+v", overview[0])
	}
	if rate, ok := overview[0]["passRate"].(float64); !ok || rate != 0.5 {
		t.Fatalf("通过率应为 0.5: %+v", overview[0]["passRate"])
	}
	if overview[0]["lastAt"] == nil {
		t.Fatalf("应带最近一次时间: %+v", overview[0])
	}
	if overview[1]["rule"] != "amount_range" || overview[1]["failed"] != int64(0) {
		t.Fatalf("第二条规则不对: %+v", overview[1])
	}

	// 趋势按天聚合，今天只有一桶。
	trend, err := r.QualityTrend(ctx, ws.ID, 7)
	if err != nil {
		t.Fatalf("质量趋势失败: %v", err)
	}
	if len(trend) != 1 {
		t.Fatalf("同一天应有 1 个桶，实际 %d: %+v", len(trend), trend)
	}
	if trend[0]["date"] != time.Now().Format("2006-01-02") {
		t.Fatalf("日期桶不对: %v", trend[0]["date"])
	}
	if trend[0]["total"] != int64(3) || trend[0]["passed"] != int64(2) {
		t.Fatalf("趋势计数不对: %+v", trend[0])
	}
	if rate, ok := trend[0]["passRate"].(float64); !ok || rate < 0.66 || rate > 0.67 {
		t.Fatalf("趋势通过率应约为 0.667: %+v", trend[0]["passRate"])
	}
	// 非法天数回落 14 天，仍能查到今天的桶。
	if trend, err := r.QualityTrend(ctx, ws.ID, 0); err != nil || len(trend) != 1 {
		t.Fatalf("非法天数应回落默认值: %d %v", len(trend), err)
	}
	// 其他工作区看不到本工作区的结果。
	if trend, err := r.QualityTrend(ctx, ws.ID, 1); err != nil {
		t.Fatalf("趋势查询失败: %v", err)
	} else if trend[0]["total"] != int64(3) {
		t.Fatalf("不应混入其他工作区: %+v", trend[0])
	}
}

// ReplaceLineage 是「全量替换某下游表的边」：旧的清掉、空名与自环跳过、重复边幂等。
// AddLineage 只增不删，模型发布时用它避免清掉管道阶段的边。
func TestLineageReplaceAndAdd(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if err := r.ReplaceLineage(ctx, ws.ID, "dwd_order", []string{"ods_order", "ods_user", "ods_order", "", "dwd_order"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	// 空串、自环与重复边都应被过滤，只剩两条。
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge WHERE workspace_id=$1 AND downstream='dwd_order'`, ws.ID); n != 2 {
		t.Fatalf("应有 2 条边，实际 %d", n)
	}

	graph, err := r.LineageGraph(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取血缘图失败: %v", err)
	}
	if len(graph) != 2 {
		t.Fatalf("血缘图应有 2 条边: %+v", graph)
	}

	// 只替换下游 dwd_order 的边，其他下游不受影响。
	if err := r.ReplaceLineage(ctx, ws.ID, "dws_trade", []string{"dwd_order"}); err != nil {
		t.Fatalf("写下游血缘失败: %v", err)
	}
	if err := r.ReplaceLineage(ctx, ws.ID, "dwd_order", []string{"ods_order"}); err != nil {
		t.Fatalf("重写血缘失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("替换后应共 2 条边（dwd_order 一条 + dws_trade 一条），实际 %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge WHERE downstream='dwd_order' AND upstream='ods_user'`); n != 0 {
		t.Fatalf("旧边应被清掉: %d", n)
	}

	// AddLineage 不清旧边。
	if err := r.AddLineage(ctx, ws.ID, "dwd_order", []string{"ods_user", "ods_order", "dwd_order"}); err != nil {
		t.Fatalf("追加血缘失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge WHERE downstream='dwd_order'`); n != 2 {
		t.Fatalf("追加后应是 2 条（重复边幂等、自环跳过），实际 %d", n)
	}
	// 跨工作区隔离。
	if err := r.AddLineage(ctx, other.ID, "dwd_order", []string{"ods_order"}); err != nil {
		t.Fatalf("其他工作区追加失败: %v", err)
	}
	if graph, err := r.LineageGraph(ctx, other.ID); err != nil || len(graph) != 1 {
		t.Fatalf("其他工作区应独立: %v %v", graph, err)
	}
}

// 审计日志：detail 是 JSON，读回要还原成 map；列表按自增 id 倒序，limit 生效。
func TestAuditLogWriteAndList(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	if err := r.Audit(ctx, model.AuditLog{
		WorkspaceID: ws.ID,
		Actor:       "alice",
		Action:      "model.publish",
		Resource:    "model/dwd_order",
		Detail:      map[string]any{"version": float64(2), "ddl": "CREATE TABLE ..."},
		RequestID:   "req-1",
	}); err != nil {
		t.Fatalf("写审计失败: %v", err)
	}
	if err := r.Audit(ctx, model.AuditLog{
		WorkspaceID: ws.ID, Actor: "bob", Action: "pipeline.disable", Resource: "pipeline/p1",
	}); err != nil {
		t.Fatalf("写第二条审计失败: %v", err)
	}
	if err := r.Audit(ctx, model.AuditLog{
		WorkspaceID: "ws_other", Actor: "carol", Action: "x", Resource: "y",
	}); err != nil {
		t.Fatalf("写其他工作区审计失败: %v", err)
	}

	logs, err := r.ListAudit(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列审计失败: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("应有 2 条审计，实际 %d", len(logs))
	}
	// 按 id 倒序：后写的那条在前。
	if logs[0].Actor != "bob" || logs[0].Action != "pipeline.disable" {
		t.Fatalf("应按 id 倒序: %+v", logs[0])
	}
	if logs[1].Actor != "alice" || logs[1].RequestID != "req-1" || logs[1].Resource != "model/dwd_order" {
		t.Fatalf("审计字段不对: %+v", logs[1])
	}
	if logs[1].Detail["ddl"] != "CREATE TABLE ..." || logs[1].Detail["version"] != float64(2) {
		t.Fatalf("detail 应还原为 map: %+v", logs[1].Detail)
	}
	if logs[1].CreatedAt.IsZero() || logs[1].ID == 0 {
		t.Fatalf("应回填 id 与时间: %+v", logs[1])
	}
	// 没写 detail 时序列化出的是 JSON null，读回为 nil（不是空对象）。
	if logs[0].Detail != nil {
		t.Fatalf("未提供 detail 应读回 nil: %+v", logs[0].Detail)
	}

	if only, err := r.ListAudit(ctx, ws.ID, 1); err != nil || len(only) != 1 || only[0].Actor != "bob" {
		t.Fatalf("limit 未生效: %+v %v", only, err)
	}
	if all, err := r.ListAudit(ctx, ws.ID, 0); err != nil || len(all) != 2 {
		t.Fatalf("非法 limit 应回落默认值: %d %v", len(all), err)
	}
}
