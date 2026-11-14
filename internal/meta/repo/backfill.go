package repo

import (
	"context"
	"time"

	"github.com/godw/godw/internal/meta/model"
)

// ExistingBizDates 返回日期范围内已有实例的业务日期集合。
// 判「已有」不看状态：失败实例也算已有，补数默认跳过，
// 想重跑失败分区请用 force，这样「补数」和「重跑」是两个明确动作，不混淆。
func (r *Repo) ExistingBizDates(ctx context.Context, workspaceID, pipeline, from, to string) (map[string]bool, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT DISTINCT i.biz_date::text
		   FROM instance i JOIN pipeline p ON p.id = i.pipeline_id
		  WHERE i.workspace_id = $1 AND p.name = $2 AND i.biz_date BETWEEN $3 AND $4`,
		workspaceID, pipeline, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// SaveBackfillBatch 记录一次补数提交，便于事后审计「谁在什么时候补了哪段」。
func (r *Repo) SaveBackfillBatch(ctx context.Context, workspaceID string, b *model.BackfillBatch) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO backfill_batch(id,workspace_id,pipeline,date_from,date_to,total,created,skipped,reason,created_by,created_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		b.ID, workspaceID, b.Pipeline, b.From, b.To, b.Total, b.Created, b.Skipped, b.Reason, b.CreatedBy, b.CreatedAt)
	return err
}

func (r *Repo) GetBackfillBatch(ctx context.Context, workspaceID, id string) (*model.BackfillBatch, error) {
	var b model.BackfillBatch
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,pipeline,date_from,date_to,total,created,skipped,reason,created_by,created_at
		   FROM backfill_batch WHERE workspace_id=$1 AND id=$2`, workspaceID, id).
		Scan(&b.ID, &b.Pipeline, &b.From, &b.To, &b.Total, &b.Created, &b.Skipped, &b.Reason, &b.CreatedBy, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *Repo) ListBackfillBatches(ctx context.Context, workspaceID string, limit int) ([]model.BackfillBatch, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,pipeline,date_from,date_to,total,created,skipped,reason,created_by,created_at
		   FROM backfill_batch WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.BackfillBatch{}
	for rows.Next() {
		var b model.BackfillBatch
		if err := rows.Scan(&b.ID, &b.Pipeline, &b.From, &b.To, &b.Total, &b.Created, &b.Skipped,
			&b.Reason, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackfillStatus 统计范围内的实例状态。
func (r *Repo) BackfillStatus(ctx context.Context, workspaceID, pipeline, from, to string) (map[string]int, []string, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT i.status, count(*), COALESCE(min(i.error),'')
		   FROM instance i JOIN pipeline p ON p.id = i.pipeline_id
		  WHERE i.workspace_id=$1 AND p.name=$2 AND i.biz_date BETWEEN $3 AND $4
		  GROUP BY i.status`, workspaceID, pipeline, from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	res := map[string]int{"success": 0, "failed": 0, "running": 0}
	fails := []string{}
	for rows.Next() {
		var status string
		var n int
		var errMsg string
		if err := rows.Scan(&status, &n, &errMsg); err != nil {
			return nil, nil, err
		}
		switch status {
		case "success":
			res["success"] += n
		case "failed":
			res["failed"] += n
			if errMsg != "" {
				fails = append(fails, errMsg)
			}
		case "running", "queued":
			res["running"] += n
		}
	}
	return res, fails, rows.Err()
}

// BackfillGantt 为运维甘特图提供数据：每个实例的起止时间。
func (r *Repo) BackfillGantt(ctx context.Context, workspaceID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 120
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT i.id, COALESCE(p.name,''), i.status, i.biz_date::text,
		        COALESCE(i.started_at, i.created_at), COALESCE(i.finished_at, now())
		   FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		  WHERE i.workspace_id=$1 ORDER BY i.created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, status, biz string
		var start, end time.Time
		if err := rows.Scan(&id, &name, &status, &biz, &start, &end); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "pipeline": name, "status": status, "bizDate": biz,
			"start": start, "end": end,
			"durationMs": end.Sub(start).Milliseconds(),
		})
	}
	return out, rows.Err()
}
