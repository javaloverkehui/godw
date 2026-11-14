package service

import (
	"context"
	"fmt"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
)

// =============================================================================
// T0906 补数（backfill）
//
// 补数最容易出事的地方不是「跑不起来」，而是「跑重了」和「不知道跑到哪了」。
// 因此这里做三件事：
//  1. 跑之前先算账（Plan）：一共几天、已有实例几天、真要补几天；
//  2. 提交后把范围固化成 batch，进度按 batch 反查实例，而不是靠前端自己数；
//  3. 已有实例的日期默认跳过（幂等由 Trigger 的 idempotency_key 兜底），
//     需要强制重跑必须显式 force，避免误把已成功的分区再跑一遍。
// =============================================================================

// PlanBackfill 计算日期范围内需要补的日期。
func (s *Service) PlanBackfill(ctx context.Context, workspaceID, pipeline, from, to string) (*model.BackfillPlan, error) {
	dates, err := enumerateDates(from, to)
	if err != nil {
		return nil, err
	}
	plan := &model.BackfillPlan{Pipeline: pipeline, From: from, To: to, Total: len(dates), Dates: []string{}, Skipped: []string{}}
	existing, err := s.Repo.ExistingBizDates(ctx, workspaceID, pipeline, from, to)
	if err != nil {
		return nil, err
	}
	for _, d := range dates {
		if existing[d] {
			plan.Existing++
			plan.Skipped = append(plan.Skipped, d)
			continue
		}
		plan.Dates = append(plan.Dates, d)
	}
	plan.Missing = len(plan.Dates)
	return plan, nil
}

// Backfill 提交补数。
func (s *Service) Backfill(ctx context.Context, workspaceID, pipeline, from, to, reason, actor string, force bool) (*model.BackfillBatch, error) {
	if pipeline == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "缺少 pipeline")
	}
	plan, err := s.PlanBackfill(ctx, workspaceID, pipeline, from, to)
	if err != nil {
		return nil, err
	}
	runDates := plan.Dates
	skipped := plan.Existing
	if force {
		// 强制模式：把原本跳过的日期也一起跑，但只跑已有实例的日期。
		runDates = append([]string{}, plan.Dates...)
		runDates = append(runDates, plan.Skipped...)
		skipped = 0
	}
	if len(runDates) == 0 {
		return nil, errs.New(errs.CodeConflict, "该日期范围内没有需要补数的分区（如需重跑请勾选强制）")
	}
	// 上限保护：一次补 366 天以上几乎一定是手滑，先拦住。
	if len(runDates) > 366 {
		return nil, errs.New(errs.CodeInvalidArgument, fmt.Sprintf("一次最多补 366 天，当前 %d 天，请拆分范围", len(runDates)))
	}

	b := &model.BackfillBatch{
		ID: ids.New("bf"), Pipeline: pipeline, From: from, To: to,
		Total: len(runDates), Reason: reason, CreatedBy: actor, CreatedAt: time.Now(),
		Skipped: skipped,
	}
	created, failed := 0, []string{}
	for _, d := range runDates {
		inst, isNew, err := s.Trigger(ctx, workspaceID, pipeline, d, "backfill", map[string]any{"backfillId": b.ID})
		_ = inst
		if err != nil {
			// 单个日期失败不中断整批：把失败日期收集起来一起报，避免「补 30 天卡在第 3 天」。
			failed = append(failed, d+": "+err.Error())
			continue
		}
		if isNew {
			created++
		}
	}
	if created == 0 && len(failed) > 0 {
		return nil, errs.New(errs.CodeInternal, "全部日期触发失败："+failed[0])
	}
	b.Created = created
	b.FailDetail = failed
	if err := s.Repo.SaveBackfillBatch(ctx, workspaceID, b); err != nil {
		return nil, err
	}
	return b, nil
}

// BackfillProgress 返回某批次的实时进度。
func (s *Service) BackfillProgress(ctx context.Context, workspaceID, id string) (*model.BackfillBatch, error) {
	b, err := s.Repo.GetBackfillBatch(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	st, fails, err := s.Repo.BackfillStatus(ctx, workspaceID, b.Pipeline, b.From, b.To)
	if err != nil {
		return nil, err
	}
	b.Success, b.Failed, b.Running = st["success"], st["failed"], st["running"]
	b.Done = b.Success + b.Failed
	if b.Total > 0 {
		b.Percent = float64(b.Done) * 100 / float64(b.Total)
	}
	b.FailDetail = append(fails, b.FailDetail...)
	return b, nil
}

// ListBackfills 列出最近的补数批次。
func (s *Service) ListBackfills(ctx context.Context, workspaceID string, limit int) ([]model.BackfillBatch, error) {
	return s.Repo.ListBackfillBatches(ctx, workspaceID, limit)
}

// enumerateDates 生成 [from, to] 的日期列表，含端点。
func enumerateDates(from, to string) ([]string, error) {
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, errs.New(errs.CodeInvalidArgument, "from 必须是 YYYY-MM-DD")
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, errs.New(errs.CodeInvalidArgument, "to 必须是 YYYY-MM-DD")
	}
	if t.Before(f) {
		return nil, errs.New(errs.CodeInvalidArgument, "to 不能早于 from")
	}
	var out []string
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out, nil
}
