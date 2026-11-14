package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 管道的默认值（时区/并发/资源组/优先级）直接决定调度行为：
// 并发为 0 会让 worker 永远不派发，优先级为 0 会让队列排序错乱，所以逐个钉住。
func TestSavePipelineDefaultsAndVersions(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	p, err := r.SavePipeline(ctx, ws.ID, model.PipelineSpec{
		Name: "daily_order",
		// 故意把各字段留空/置 0，验证默认值补齐。
	}, "alice", false)
	if err != nil {
		t.Fatalf("保存管道失败: %v", err)
	}
	if p.ID == "" || !p.Enabled {
		t.Fatalf("新建管道应默认启用: %+v", p)
	}
	if p.Timezone != "Asia/Shanghai" || p.Concurrency != 1 || p.ResourceGroup != "default" || p.Priority != 5 {
		t.Fatalf("默认值不对: %+v", p)
	}
	if p.ActiveVersion != 0 {
		t.Fatalf("未发布时不应有生效版本: %d", p.ActiveVersion)
	}

	// 第二版发布：active_version 切到 2，调度才会认。
	created := savePipeline(t, r, ws.ID, model.PipelineSpec{
		Name: "daily_order", Description: "日更", Schedule: "0 2 * * *",
		Timezone: "UTC", Concurrency: 3, ResourceGroup: "etl", Priority: 1,
	}, true)
	if created.ID != p.ID {
		t.Fatalf("同名管道应复用 id: %s != %s", created.ID, p.ID)
	}
	if created.ActiveVersion != 2 {
		t.Fatalf("发布后生效版本应为 2，实际 %d", created.ActiveVersion)
	}
	if created.Schedule != "0 2 * * *" || created.Timezone != "UTC" || created.Concurrency != 3 {
		t.Fatalf("更新未生效: %+v", created)
	}

	// 版本快照要两份，且旧版本归档。
	if n := queryInt(t, db, `SELECT count(*) FROM pipeline_version WHERE pipeline_id=$1`, p.ID); n != 2 {
		t.Fatalf("应有 2 个版本快照，实际 %d", n)
	}
	if s := queryStr(t, db, `SELECT status FROM pipeline_version WHERE pipeline_id=$1 AND version=2`, p.ID); s != "published" {
		t.Fatalf("v2 应是 published，实际 %q", s)
	}
	if s := queryStr(t, db, `SELECT status FROM pipeline_version WHERE pipeline_id=$1 AND version=1`, p.ID); s != "draft" {
		t.Fatalf("从未发布的 v1 应保持 draft，实际 %q", s)
	}

	got, spec, version, err := r.PipelineByName(ctx, ws.ID, "daily_order")
	if err != nil {
		t.Fatalf("按名取管道失败: %v", err)
	}
	if got.ID != p.ID || version != 2 {
		t.Fatalf("按名取回的信息不对: %+v v=%d", got, version)
	}
	if spec == nil || spec.Schedule != "0 2 * * *" || spec.ResourceGroup != "etl" {
		t.Fatalf("生效 spec 不对: %+v", spec)
	}

	// 历史版本快照必须能单独取回：重跑历史实例依赖它。
	v1, err := r.PipelineVersionSpec(ctx, p.ID, 1)
	if err != nil {
		t.Fatalf("取 v1 快照失败: %v", err)
	}
	if v1.Name != "daily_order" || v1.Schedule != "" {
		t.Fatalf("v1 快照不应被 v2 覆盖: %+v", v1)
	}
	if _, err := r.PipelineVersionSpec(ctx, p.ID, 9); err == nil {
		t.Fatal("不存在的版本应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	if _, _, _, err := r.PipelineByName(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的管道应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 跨工作区同名管道不可见。
	other := seedWorkspace(t, r, "other")
	if _, _, _, err := r.PipelineByName(ctx, other.ID, "daily_order"); err == nil {
		t.Fatal("跨工作区不应查到管道")
	}
}

// ListPipelines 按名称排序；只有生效版本才附带 spec。
func TestListPipelinesAndEnabledFilter(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	// b_pipe：有 cron 且已发布 → 应进入调度列表。
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "b_pipe", Schedule: "0 * * * *"}, true)
	// a_draft：草稿，没有生效版本 → 不进调度列表。
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "a_draft", Schedule: "0 * * * *"}, false)
	// c_nosched：发布了但没有 cron，只能手动触发。
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "c_nosched"}, true)

	list, err := r.ListPipelines(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列管道失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 条管道，实际 %d", len(list))
	}
	if list[0].Name != "a_draft" || list[1].Name != "b_pipe" || list[2].Name != "c_nosched" {
		t.Fatalf("应按名称排序: %+v", list)
	}
	if list[0].Spec != nil {
		t.Fatalf("草稿不应带 spec: %+v", list[0])
	}
	if list[1].Spec == nil || list[1].Spec.Schedule != "0 * * * *" {
		t.Fatalf("生效版本应带 spec: %+v", list[1].Spec)
	}

	enabled, err := r.ListEnabledPipelines(ctx)
	if err != nil {
		t.Fatalf("列启用管道失败: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Name != "b_pipe" {
		t.Fatalf("只有已发布且有 cron 的管道应被调度: %+v", enabled)
	}
	if enabled[0].ResourceGroup != "default" || enabled[0].Priority != 5 || enabled[0].Timezone != "Asia/Shanghai" {
		t.Fatalf("调度视图字段不对: %+v", enabled[0])
	}

	// 关掉后立刻从调度列表消失，但列表页仍能看到。
	if err := r.SetPipelineEnabled(ctx, ws.ID, "b_pipe", false); err != nil {
		t.Fatalf("停用管道失败: %v", err)
	}
	if enabled, err := r.ListEnabledPipelines(ctx); err != nil || len(enabled) != 0 {
		t.Fatalf("停用后不应再被调度: %v %v", enabled, err)
	}
	if p := queryInt(t, r.DB, `SELECT CASE WHEN enabled THEN 1 ELSE 0 END FROM pipeline WHERE name='b_pipe'`); p != 0 {
		t.Fatalf("enabled 列应为 false，实际 %d", p)
	}
	if err := r.SetPipelineEnabled(ctx, ws.ID, "b_pipe", true); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	// 停用不存在的管道要给出 NotFound，而不是静默成功。
	if err := r.SetPipelineEnabled(ctx, ws.ID, "nope", true); err == nil {
		t.Fatal("不存在的管道应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
}

// 指标与看板都是「同工作区同名覆盖」，指标还受 agg 的 CHECK 约束。
func TestMetricAndDashboardUpsert(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	m := metricSample("gmv")
	saved, err := r.SaveMetric(ctx, ws.ID, m)
	if err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if saved.ID == "" || saved.WorkspaceID != ws.ID {
		t.Fatalf("指标应回填 ID 与工作区: %+v", saved)
	}

	// 同名覆盖：描述与维度更新，行数不变。
	m.DisplayName = "成交额 GMV"
	m.Dimensions = []string{"channel"}
	if _, err := r.SaveMetric(ctx, ws.ID, m); err != nil {
		t.Fatalf("覆盖指标失败: %v", err)
	}
	// 另一个指标用来验证排序。
	if _, err := r.SaveMetric(ctx, ws.ID, metricSample("a_users")); err != nil {
		t.Fatalf("保存第二个指标失败: %v", err)
	}
	// 另一个工作区同名指标互不影响。
	if _, err := r.SaveMetric(ctx, other.ID, metricSample("gmv")); err != nil {
		t.Fatalf("其他工作区保存指标失败: %v", err)
	}

	list, err := r.ListMetrics(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列指标失败: %v", err)
	}
	if len(list) != 2 || list[0].Name != "a_users" || list[1].Name != "gmv" {
		t.Fatalf("应按名称排序且只含本工作区: %+v", list)
	}
	if list[1].DisplayName != "成交额 GMV" || len(list[1].Dimensions) != 1 || list[1].Dimensions[0] != "channel" {
		t.Fatalf("覆盖未生效: %+v", list[1])
	}

	got, err := r.MetricByName(ctx, ws.ID, "gmv")
	if err != nil {
		t.Fatalf("按名取指标失败: %v", err)
	}
	if got.ModelName != "dwd_trade" || got.Agg != "sum" || got.Expr != "amount" {
		t.Fatalf("指标字段不对: %+v", got)
	}
	if _, err := r.MetricByName(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的指标应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// agg 只能是枚举值，SQL 层必须有约束兜底。
	bad := metricSample("bad_agg")
	bad.Agg = "median"
	if _, err := r.SaveMetric(ctx, ws.ID, bad); err == nil {
		t.Fatal("非法聚合方式应被 CHECK 约束拒绝")
	}

	// 看板同理：同名覆盖，列表按名称排序。
	if _, err := r.SaveDashboard(ctx, ws.ID, dashboardSample("ops", "运维首页")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	if _, err := r.SaveDashboard(ctx, ws.ID, dashboardSample("core", "核心指标")); err != nil {
		t.Fatalf("保存第二个看板失败: %v", err)
	}
	d, err := r.SaveDashboard(ctx, ws.ID, dashboardSample("ops", "运维首页 v2"))
	if err != nil {
		t.Fatalf("覆盖看板失败: %v", err)
	}
	if d.WorkspaceID != ws.ID || len(d.Cards) != 1 {
		t.Fatalf("看板应回填字段: %+v", d)
	}

	dashboards, err := r.ListDashboards(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列看板失败: %v", err)
	}
	if len(dashboards) != 2 || dashboards[0].Name != "core" || dashboards[1].Name != "ops" {
		t.Fatalf("应按名称排序: %+v", dashboards)
	}
	if dashboards[1].Description != "运维首页 v2" {
		t.Fatalf("覆盖未生效: %+v", dashboards[1])
	}
	if len(dashboards[1].Cards) != 1 || dashboards[1].Cards[0].Metric != "gmv" {
		t.Fatalf("卡片应往返: %+v", dashboards[1].Cards)
	}

	gotDash, err := r.DashboardByName(ctx, ws.ID, "ops")
	if err != nil {
		t.Fatalf("按名取看板失败: %v", err)
	}
	if gotDash.Description != "运维首页 v2" {
		t.Fatalf("按名取回内容不对: %+v", gotDash)
	}
	if _, err := r.DashboardByName(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的看板应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
}
