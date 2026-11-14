package repo

import (
	"context"
	"errors"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/jackc/pgx/v5"
)

// 合规仓储：保留策略、删除权任务、导出审批。

func (r *Repo) SaveRetentionPolicy(ctx context.Context, workspaceID string, p model.RetentionPolicy) error {
	if p.RetainDays <= 0 {
		p.RetainDays = 365
	}
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO retention_policy(id,workspace_id,model_name,retain_days,archive_to,enabled)
		 VALUES($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (workspace_id,model_name) DO UPDATE SET
		   retain_days=EXCLUDED.retain_days, archive_to=EXCLUDED.archive_to, enabled=EXCLUDED.enabled`,
		ids.New("rtp"), workspaceID, p.ModelName, p.RetainDays, p.ArchiveTo, p.Enabled)
	return err
}

func (r *Repo) GetRetentionPolicy(ctx context.Context, workspaceID, modelName string) (*model.RetentionPolicy, error) {
	var p model.RetentionPolicy
	p.WorkspaceID, p.ModelName = workspaceID, modelName
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT retain_days,archive_to,enabled FROM retention_policy
		 WHERE workspace_id=$1 AND model_name=$2`, workspaceID, modelName).
		Scan(&p.RetainDays, &p.ArchiveTo, &p.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "未配置保留策略: "+modelName)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repo) ListRetentionPolicies(ctx context.Context, workspaceID string) ([]model.RetentionPolicy, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT model_name,retain_days,archive_to,enabled FROM retention_policy
		 WHERE workspace_id=$1 ORDER BY model_name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RetentionPolicy{}
	for rows.Next() {
		var p model.RetentionPolicy
		p.WorkspaceID = workspaceID
		if err := rows.Scan(&p.ModelName, &p.RetainDays, &p.ArchiveTo, &p.Enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) CreateErasureJob(ctx context.Context, job model.ErasureJob) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO erasure_job(id,workspace_id,model_name,subject_key,status,reason,requested_by)
		 VALUES($1,$2,$3,$4,$5,$6,$7)`,
		job.ID, job.WorkspaceID, job.ModelName, job.SubjectKey, job.Status, job.Reason, job.RequestedBy)
	return err
}

func (r *Repo) SetErasureStatus(ctx context.Context, id, status string, rows int64, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE erasure_job SET status=$2, rows_deleted=$3, error=$4,
		        finished_at=CASE WHEN $2 IN ('done','failed') THEN now() ELSE finished_at END
		 WHERE id=$1`, id, status, rows, errMsg)
	return err
}

func (r *Repo) ListErasureJobs(ctx context.Context, workspaceID string, limit int) ([]model.ErasureJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,model_name,subject_key,status,reason,rows_deleted,error,requested_by,created_at,finished_at
		 FROM erasure_job WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT `+itoa(limit), workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ErasureJob{}
	for rows.Next() {
		var j model.ErasureJob
		j.WorkspaceID = workspaceID
		if err := rows.Scan(&j.ID, &j.ModelName, &j.SubjectKey, &j.Status, &j.Reason,
			&j.RowsDeleted, &j.Error, &j.RequestedBy, &j.CreatedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// TableClassification 返回一组表里最高的敏感级别。
// 只按表名匹配，不再用「工作区存在某数据集」这种近似判断：
// 一张表的 secret 列不应该让同工作区的其他表也变成 high。
func (r *Repo) TableClassification(ctx context.Context, workspaceID string, tables []string) (string, error) {
	if len(tables) == 0 {
		return "none", nil
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT DISTINCT classification FROM column_policy
		 WHERE workspace_id=$1 AND table_name = ANY($2)`, workspaceID, tables)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	rank := 0
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		if r := model.ClassificationRank(c); r > rank {
			rank = r
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return model.ClassificationName(rank), nil
}
