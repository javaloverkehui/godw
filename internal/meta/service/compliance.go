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

// ExportTables 解析一次导出实际会读到的物理表。
// metric/dashboard 走指标绑定的模型；dataset 走数据集的基础模型。
// 导出的审批必须基于这些表，而不是「用户传进来的 target 字符串」。
func (s *Service) ExportTables(ctx context.Context, workspaceID, kind, target string) ([]string, error) {
	models := []string{}
	switch kind {
	case "metric":
		m, err := s.Repo.MetricByName(ctx, workspaceID, target)
		if err != nil {
			return nil, err
		}
		models = append(models, m.ModelName)
	case "dashboard":
		d, err := s.Repo.DashboardByName(ctx, workspaceID, target)
		if err != nil {
			return nil, err
		}
		for _, c := range d.Cards {
			if c.Metric != "" {
				m, merr := s.Repo.MetricByName(ctx, workspaceID, c.Metric)
				if merr != nil {
					return nil, merr
				}
				models = append(models, m.ModelName)
			}
		}
	case "dataset":
		d, err := s.Repo.GetDataset(ctx, workspaceID, target)
		if err != nil {
			return nil, err
		}
		models = append(models, d.BaseModel)
	}
	seen := map[string]bool{}
	tables := []string{}
	for _, name := range models {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, spec.TableName())
	}
	return tables, nil
}

// ExportSensitivity 返回一次导出的最高敏感级别及涉及的表。
// 分级解析失败时不阻断导出，但调用方应把错误写进审计：
// 合规系统宁可让人看到「分级未知」，也不要假装已经检查过。
func (s *Service) ExportSensitivity(ctx context.Context, workspaceID, kind, target string) (string, []string, error) {
	tables, err := s.ExportTables(ctx, workspaceID, kind, target)
	if err != nil {
		return "none", nil, err
	}
	level, err := s.Repo.TableClassification(ctx, workspaceID, tables)
	if err != nil {
		return "none", tables, err
	}
	return level, tables, nil
}
