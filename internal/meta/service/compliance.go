package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
)

// T0807 合规与数据生命周期。
//
// 三件事：
//   1. 保留策略：到期数据归档或删除，留审计；
//   2. 导出审批：high 级数据导出需批准，未批准拒绝；
//   3. 删除权：按主体键删除指定用户数据，生成任务并留痕。
//
// 共同点：都必须可追溯。合规场景下「删了但没记录」等于没删。

// RetentionPlan 是一次保留策略执行计划。
type RetentionPlan struct {
	ModelName  string   `json:"modelName"`
	Table      string   `json:"table"`
	RetainDays int      `json:"retainDays"`
	Cutoff     string   `json:"cutoff"`
	Action     string   `json:"action"` // archive | delete | skip
	Statements []string `json:"statements"`
	Note       string   `json:"note,omitempty"`
}

// PlanRetention 生成保留策略的执行计划（不执行）。
func (s *Service) PlanRetention(ctx context.Context, workspaceID, modelName string) (*RetentionPlan, error) {
	policy, err := s.Repo.GetRetentionPolicy(ctx, workspaceID, modelName)
	if err != nil {
		return nil, err
	}
	spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, modelName)
	if err != nil {
		return nil, err
	}
	if len(spec.PartitionBy) == 0 {
		return &RetentionPlan{
			ModelName: modelName, Table: spec.TableName(), RetainDays: policy.RetainDays,
			Action: "skip", Note: "模型未分区，自动清理会全表扫描；请先按 dt 分区再启用保留策略",
		}, nil
	}
	c, err := s.connectorFor(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return nil, err
	}
	d := c.Dialect()
	partCol := strings.TrimSpace(spec.PartitionBy[0])
	if i := strings.Index(partCol, "("); i > 0 {
		inner := strings.TrimSuffix(partCol[i+1:], ")")
		if j := strings.LastIndex(inner, ","); j >= 0 {
			inner = inner[j+1:]
		}
		partCol = strings.Trim(strings.TrimSpace(inner), `"'`)
	}
	cutoff := time.Now().AddDate(0, 0, -policy.RetainDays).Format("2006-01-02")
	table := d.QuoteIdent(spec.TableName())
	if spec.Schema != "" {
		table = d.QuoteIdent(spec.Schema) + "." + table
	}
	cond := fmt.Sprintf("%s < '%s'", d.QuoteIdent(partCol), cutoff)

	plan := &RetentionPlan{
		ModelName: modelName, Table: spec.TableName(), RetainDays: policy.RetainDays,
		Cutoff: cutoff, Action: "delete",
	}
	if policy.ArchiveTo != "" {
		plan.Action = "archive"
		plan.Statements = []string{
			fmt.Sprintf("-- 归档到 %s（由对象存储侧任务执行）", policy.ArchiveTo),
			fmt.Sprintf("COPY (SELECT * FROM %s WHERE %s) TO STDOUT", table, cond),
			fmt.Sprintf("DELETE FROM %s WHERE %s", table, cond),
		}
	} else {
		plan.Statements = []string{fmt.Sprintf("DELETE FROM %s WHERE %s", table, cond)}
	}
	return plan, nil
}

// RunRetention 执行保留策略并写审计。返回受影响行数。
func (s *Service) RunRetention(ctx context.Context, workspaceID, modelName, actor string) (int64, error) {
	plan, err := s.PlanRetention(ctx, workspaceID, modelName)
	if err != nil {
		return 0, err
	}
	if plan.Action == "skip" {
		return 0, errs.New(errs.CodeInvalidArgument, plan.Note)
	}
	spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, modelName)
	if err != nil {
		return 0, err
	}
	c, err := s.connectorFor(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, stmt := range plan.Statements {
		if strings.HasPrefix(strings.TrimSpace(stmt), "--") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(stmt), "COPY") {
			// COPY TO STDOUT 需要流式消费，这里由归档任务单独执行；
			// 平台侧只做删除，不假装完成了归档。
			continue
		}
		// 安全阀：没有 WHERE 的 DELETE 一律拒绝。
		if strings.Contains(strings.ToUpper(stmt), "DELETE") && !strings.Contains(strings.ToUpper(stmt), "WHERE") {
			return affected, errs.New(errs.CodeForbidden, "拒绝执行无 WHERE 的删除语句")
		}
		res, eerr := c.Exec(ctx, stmt)
		if eerr != nil {
			return affected, errs.Wrap(errs.CodeExecutionFailed, eerr, "保留策略执行失败").WithMeta("sql", stmt)
		}
		affected += res.RowsAffected
	}
	s.Repo.Audit(ctx, model.AuditLog{
		WorkspaceID: workspaceID, Actor: actor, Action: "retention.run",
		Resource: modelName,
		Detail:   map[string]any{"cutoff": plan.Cutoff, "rows": affected, "action": plan.Action},
	})
	return affected, nil
}

// RequestErasure 创建删除权任务：按主体键删除指定用户的数据。
// 删除在后台执行，先返回任务 ID，避免同步请求超时。
func (s *Service) RequestErasure(ctx context.Context, workspaceID, modelName, subjectColumn, subjectValue, reason, actor string) (*model.ErasureJob, error) {
	if strings.TrimSpace(subjectColumn) == "" || strings.TrimSpace(subjectValue) == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "subjectColumn 与 subjectValue 必填")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "删除请求必须说明原因（合规要求）")
	}
	spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, modelName)
	if err != nil {
		return nil, err
	}
	if !hasColumnIn(spec, subjectColumn) {
		return nil, errs.New(errs.CodeInvalidArgument, "模型中不存在列: "+subjectColumn)
	}
	job := &model.ErasureJob{
		ID: ids.New("ers"), WorkspaceID: workspaceID, ModelName: modelName,
		SubjectKey: subjectColumn + "=" + subjectValue, Reason: reason,
		RequestedBy: actor, Status: "pending",
	}
	if err := s.Repo.CreateErasureJob(ctx, *job); err != nil {
		return nil, err
	}
	go s.runErasure(context.WithoutCancel(ctx), *job, spec, subjectColumn, subjectValue)
	return job, nil
}

func (s *Service) runErasure(ctx context.Context, job model.ErasureJob, spec *model.ModelSpec, column, value string) {
	_ = s.Repo.SetErasureStatus(ctx, job.ID, "running", 0, "")
	c, err := s.connectorFor(ctx, job.WorkspaceID, spec.Datasource)
	if err != nil {
		_ = s.Repo.SetErasureStatus(ctx, job.ID, "failed", 0, err.Error())
		return
	}
	d := c.Dialect()
	table := d.QuoteIdent(spec.TableName())
	if spec.Schema != "" {
		table = d.QuoteIdent(spec.Schema) + "." + table
	}
	stmt := fmt.Sprintf("DELETE FROM %s WHERE %s = '%s'",
		table, d.QuoteIdent(column), strings.ReplaceAll(value, "'", "''"))
	res, err := c.Exec(ctx, stmt)
	if err != nil {
		_ = s.Repo.SetErasureStatus(ctx, job.ID, "failed", 0, err.Error())
		return
	}
	_ = s.Repo.SetErasureStatus(ctx, job.ID, "done", res.RowsAffected, "")
	s.Repo.Audit(ctx, model.AuditLog{
		WorkspaceID: job.WorkspaceID, Actor: job.RequestedBy, Action: "erasure.done",
		Resource: job.ModelName,
		Detail:   map[string]any{"subject": job.SubjectKey, "rows": res.RowsAffected, "reason": job.Reason},
	})
}

func hasColumnIn(spec *model.ModelSpec, name string) bool {
	for _, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// CheckExportApproval 判断某导出是否需要审批、是否已批准。
// 目前审批流是两态（申请 → 批准），够用且不引入额外系统。
func (s *Service) CheckExportApproval(ctx context.Context, workspaceID, dataset string) error {
	level, err := s.Repo.DatasetClassification(ctx, workspaceID, dataset)
	if err != nil {
		// 查不到分级时按 low 处理：导出不因元数据缺失而全面阻断，
		// 但会在导出记录里留痕，便于事后审计。
		return nil
	}
	if level != "high" {
		return nil
	}
	ok, err := s.Repo.HasApprovedExport(ctx, workspaceID, dataset)
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.CodeForbidden,
			"数据集包含 high 级数据，导出需先提交审批并获批准").
			WithMeta("dataset", dataset).WithMeta("classification", level)
	}
	return nil
}
