package service_test

import (
	"testing"

	"github.com/godw/godw/internal/pkg/errs"
)

// 补数最关键的两个风险是「重复跑」和「不知道跑到哪了」：
// Plan 必须先算账（哪些日期已有实例），Backfill 必须固化批次，
// BackfillProgress 必须能把实例状态汇总回批次。
func TestPlanBackfillCountsExisting(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("bf_plan"))

	plan, err := svc.PlanBackfill(ctxBG(), ws.ID, "bf_plan", "2026-01-01", "2026-01-03")
	if err != nil {
		t.Fatalf("PlanBackfill 失败: %v", err)
	}
	if plan.Total != 3 || plan.Missing != 3 || plan.Existing != 0 {
		t.Fatalf("三天全缺时计数不对: %+v", plan)
	}

	// 已有实例（哪怕是失败/排队）也算「已存在」，补数默认跳过，这是防重复的核心。
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "bf_plan", "2026-01-02", "manual", nil); err != nil {
		t.Fatalf("手动触发失败: %v", err)
	}
	plan, err = svc.PlanBackfill(ctxBG(), ws.ID, "bf_plan", "2026-01-01", "2026-01-03")
	if err != nil {
		t.Fatalf("二次 PlanBackfill 失败: %v", err)
	}
	if plan.Existing != 1 || plan.Missing != 2 || !hasStr(plan.Skipped, "2026-01-02") {
		t.Fatalf("应跳过已有日期: %+v", plan)
	}
	containsAll(t, "待补日期", plan.Dates, []string{"2026-01-01", "2026-01-03"})

	if _, err := svc.PlanBackfill(ctxBG(), ws.ID, "bf_plan", "2026-02-01", "2026-01-01"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("反向区间应被拒绝，实际 %v", err)
	}
}

// 补 30 天卡在第 3 天是最糟的体验：单个日期失败必须收集起来继续跑。
func TestBackfillCreatesBatchAndSkipsExisting(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("bf_run"))
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "bf_run", "2026-03-02", "manual", nil); err != nil {
		t.Fatalf("预置实例失败: %v", err)
	}

	b, err := svc.Backfill(ctxBG(), ws.ID, "bf_run", "2026-03-01", "2026-03-03", "回补历史", "alice", false)
	if err != nil {
		t.Fatalf("Backfill 失败: %v", err)
	}
	if b.Total != 2 || b.Created != 2 || b.Skipped != 1 {
		t.Fatalf("批次计数不对（应为 2 新建 / 1 跳过）: %+v", b)
	}
	if b.CreatedBy != "alice" || b.Reason != "回补历史" {
		t.Fatalf("审计字段丢失: %+v", b)
	}
	list, err := svc.ListBackfills(ctxBG(), ws.ID, 10)
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("批次应可列出: %v %v", list, err)
	}

	// 同样范围再补一次：没有可补日期，必须明确拒绝而不是静默生成空批次。
	if _, err := svc.Backfill(ctxBG(), ws.ID, "bf_run", "2026-03-01", "2026-03-03", "", "alice", false); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("全跳过时应报冲突，实际 %v", err)
	}

	// 缺 pipeline 参数的报错文案要能直接定位，不能落成 500。
	if _, err := svc.Backfill(ctxBG(), ws.ID, "", "2026-03-01", "2026-03-01", "", "alice", false); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 pipeline 应报参数错误，实际 %v", err)
	}
}

// 一次补一年以上几乎一定是手滑；上限保护必须挡在触发之前。
func TestBackfillRejectsTooWideRange(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("bf_wide"))
	_, err := svc.Backfill(ctxBG(), ws.ID, "bf_wide", "2024-01-01", "2025-01-05", "", "alice", false)
	if !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("367 天应被拒绝，实际 %v", err)
	}
}

// 全部日期都触发失败时必须整体失败：否则会得到一个 Total>0 但 Created=0 的
// 批次，看板上显示「已提交」，实际一天都没排上。
func TestBackfillAllDatesFail(t *testing.T) {
	svc, ws := newSvc(t)
	// 管道存在但未发布 → Trigger 会对每个日期返回「管道未发布」。
	if _, err := svc.Repo.SavePipeline(ctxBG(), ws.ID, simplePipeline("bf_unpub"), "tester", false); err != nil {
		t.Fatalf("保存未发布管道失败: %v", err)
	}
	_, err := svc.Backfill(ctxBG(), ws.ID, "bf_unpub", "2026-04-01", "2026-04-02", "", "alice", false)
	if !errs.Is(err, errs.CodeInternal) {
		t.Fatalf("全部失败应整体报错，实际 %v", err)
	}
}

// 进度必须从实例状态反查，而不是相信前端自己数。
func TestBackfillProgressAggregatesInstances(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("bf_prog"))
	b, err := svc.Backfill(ctxBG(), ws.ID, "bf_prog", "2026-05-01", "2026-05-03", "", "alice", false)
	if err != nil {
		t.Fatalf("Backfill 失败: %v", err)
	}

	// 队列中的实例算「运行中」，进度为 0。
	prog, err := svc.BackfillProgress(ctxBG(), ws.ID, b.ID)
	if err != nil {
		t.Fatalf("BackfillProgress 失败: %v", err)
	}
	if prog.Total != 3 || prog.Running != 3 || prog.Done != 0 || prog.Percent != 0 {
		t.Fatalf("全部排队时进度不对: %+v", prog)
	}

	insts, err := svc.Repo.ListInstances(ctxBG(), ws.ID, 10)
	if err != nil || len(insts) != 3 {
		t.Fatalf("应生成 3 个实例，实际 %d: %v", len(insts), err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), insts[0].ID, "success", ""); err != nil {
		t.Fatalf("置成功失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), insts[1].ID, "failed", "分区缺失"); err != nil {
		t.Fatalf("置失败失败: %v", err)
	}

	prog, err = svc.BackfillProgress(ctxBG(), ws.ID, b.ID)
	if err != nil {
		t.Fatalf("二次 BackfillProgress 失败: %v", err)
	}
	if prog.Success != 1 || prog.Failed != 1 || prog.Running != 1 || prog.Done != 2 {
		t.Fatalf("混合状态统计不对: %+v", prog)
	}
	if !hasStr(prog.FailDetail, "分区缺失") {
		t.Fatalf("失败原因应回填到批次: %+v", prog.FailDetail)
	}
	wantPercent := 2.0 * 100 / 3.0
	if prog.Percent < wantPercent-0.01 || prog.Percent > wantPercent+0.01 {
		t.Fatalf("百分比应按 Done/Total 计算，得到 %v", prog.Percent)
	}
}
