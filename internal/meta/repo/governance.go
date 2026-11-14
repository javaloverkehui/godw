package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/jackc/pgx/v5"
)

// ---------- 列级血缘 ----------

// ReplaceColumnLineage 用一次事务替换某张下游表的全部列级血缘，避免半更新。
func (r *Repo) ReplaceColumnLineage(ctx context.Context, workspaceID, downstream string, edges []model.ColumnEdge) error {
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM lineage_column WHERE workspace_id=$1 AND downstream_table=$2`, workspaceID, downstream); err != nil {
		return err
	}
	for _, e := range edges {
		if e.UpstreamTable == "" || e.UpstreamColumn == "" || e.DownstreamColumn == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO lineage_column(workspace_id,downstream_table,downstream_column,upstream_table,upstream_column,node_id)
			 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			workspaceID, downstream, e.DownstreamColumn, e.UpstreamTable, e.UpstreamColumn, e.NodeID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ColumnLineage 返回某张表（可选某列）的上游列。
func (r *Repo) ColumnLineage(ctx context.Context, workspaceID, table, column string) ([]model.ColumnEdge, error) {
	q := `SELECT downstream_table,downstream_column,upstream_table,upstream_column,node_id
	      FROM lineage_column WHERE workspace_id=$1 AND downstream_table=$2
	        AND NOT (upstream_table=downstream_table AND upstream_column=downstream_column)`
	args := []any{workspaceID, table}
	if column != "" {
		q += ` AND downstream_column=$3`
		args = append(args, column)
	}
	q += ` ORDER BY downstream_column,upstream_table,upstream_column`
	rows, err := r.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ColumnEdge{}
	for rows.Next() {
		var e model.ColumnEdge
		if err := rows.Scan(&e.DownstreamTable, &e.DownstreamColumn, &e.UpstreamTable, &e.UpstreamColumn, &e.NodeID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ImpactAnalysis 从给定表列出发，向下游递归找受影响的表列，最多 depth 层。
// 用递归 CTE 一次算完，避免应用层 N+1 查询。
func (r *Repo) ImpactAnalysis(ctx context.Context, workspaceID, table, column string, depth int) ([]model.ColumnEdge, error) {
	if depth <= 0 || depth > 10 {
		depth = 10
	}
	rows, err := r.DB.Pool.Query(ctx, `
WITH RECURSIVE walk AS (
    SELECT downstream_table, downstream_column, upstream_table, upstream_column, node_id, 1 AS lvl
    FROM lineage_column
    WHERE workspace_id=$1 AND upstream_table=$2 AND ($3='' OR upstream_column=$3)
      AND NOT (upstream_table=downstream_table AND upstream_column=downstream_column)
  UNION
    SELECT e.downstream_table, e.downstream_column, e.upstream_table, e.upstream_column, e.node_id, w.lvl+1
    FROM lineage_column e
    JOIN walk w ON e.workspace_id=$1 AND e.upstream_table=w.downstream_table AND e.upstream_column=w.downstream_column
    WHERE w.lvl < $4
)
SELECT DISTINCT downstream_table, downstream_column, upstream_table, upstream_column, node_id FROM walk
ORDER BY downstream_table, downstream_column`, workspaceID, table, column, depth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ColumnEdge{}
	for rows.Next() {
		var e model.ColumnEdge
		if err := rows.Scan(&e.DownstreamTable, &e.DownstreamColumn, &e.UpstreamTable, &e.UpstreamColumn, &e.NodeID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- 列策略 ----------

func (r *Repo) SaveColumnPolicy(ctx context.Context, workspaceID string, p model.ColumnPolicy) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO column_policy(workspace_id,table_name,column_name,classification,mask_type)
		 VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT (workspace_id,table_name,column_name) DO UPDATE SET
		   classification=EXCLUDED.classification, mask_type=EXCLUDED.mask_type, updated_at=now()`,
		workspaceID, p.TableName, p.ColumnName, p.Classification, p.MaskType)
	return err
}

func (r *Repo) ListColumnPolicies(ctx context.Context, workspaceID, table string) ([]model.ColumnPolicy, error) {
	q := `SELECT table_name,column_name,classification,mask_type,updated_at
	      FROM column_policy WHERE workspace_id=$1`
	args := []any{workspaceID}
	if table != "" {
		q += ` AND table_name=$2`
		args = append(args, table)
	}
	q += ` ORDER BY table_name,column_name`
	rows, err := r.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ColumnPolicy{}
	for rows.Next() {
		var p model.ColumnPolicy
		p.WorkspaceID = workspaceID
		if err := rows.Scan(&p.TableName, &p.ColumnName, &p.Classification, &p.MaskType, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- 行策略 ----------

func (r *Repo) SaveRowPolicy(ctx context.Context, workspaceID string, p model.RowPolicy) (*model.RowPolicy, error) {
	if p.Role == "" {
		p.Role = "*"
	}
	p.ID = ids.New("rpl")
	p.WorkspaceID = workspaceID
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO row_policy(id,workspace_id,table_name,role,filter_expr,description)
		 VALUES($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (workspace_id,table_name,role) DO UPDATE SET
		   filter_expr=EXCLUDED.filter_expr, description=EXCLUDED.description`,
		p.ID, workspaceID, p.TableName, p.Role, p.FilterExpr, p.Description)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repo) ListRowPolicies(ctx context.Context, workspaceID string) ([]model.RowPolicy, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,table_name,role,filter_expr,description,created_at
		 FROM row_policy WHERE workspace_id=$1 ORDER BY table_name,role`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RowPolicy{}
	for rows.Next() {
		var p model.RowPolicy
		p.WorkspaceID = workspaceID
		if err := rows.Scan(&p.ID, &p.TableName, &p.Role, &p.FilterExpr, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RowFiltersFor 返回某张表对当前角色生效的过滤条件（含通配角色）。
func (r *Repo) RowFiltersFor(ctx context.Context, workspaceID, table, role string) ([]string, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT filter_expr FROM row_policy
		 WHERE workspace_id=$1 AND table_name=$2 AND role IN ($3,'*')`, workspaceID, table, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteRowPolicy(ctx context.Context, workspaceID, id string) error {
	tag, err := r.DB.Pool.Exec(ctx, `DELETE FROM row_policy WHERE workspace_id=$1 AND id=$2`, workspaceID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "策略不存在")
	}
	return nil
}

// ---------- 查询缓存 ----------

func (r *Repo) CacheGet(ctx context.Context, key string) ([]byte, bool, error) {
	var payload []byte
	err := r.DB.Pool.QueryRow(ctx,
		`UPDATE query_cache SET hit_count=hit_count+1
		 WHERE cache_key=$1 AND expires_at > now() RETURNING payload`, key).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (r *Repo) CachePut(ctx context.Context, key, workspaceID string, payload []byte, ttl time.Duration) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO query_cache(cache_key,workspace_id,payload,expires_at)
		 VALUES($1,$2,$3,now()+$4::interval)
		 ON CONFLICT (cache_key) DO UPDATE SET payload=EXCLUDED.payload, expires_at=EXCLUDED.expires_at, hit_count=0`,
		key, workspaceID, payload, ttl.String())
	return err
}

func (r *Repo) CachePurge(ctx context.Context) (int64, error) {
	tag, err := r.DB.Pool.Exec(ctx, `DELETE FROM query_cache WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repo) CacheStats(ctx context.Context, workspaceID string) (map[string]any, error) {
	var entries, hits int64
	var bytes int64
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(hit_count),0), COALESCE(SUM(pg_column_size(payload)),0)
		 FROM query_cache WHERE workspace_id=$1 AND expires_at > now()`, workspaceID).
		Scan(&entries, &hits, &bytes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entries": entries, "hits": hits, "bytes": bytes}, nil
}

// ---------- 导出 ----------

func (r *Repo) CreateExport(ctx context.Context, e model.ExportJob) (*model.ExportJob, error) {
	e.ID = ids.New("exp")
	params, err := json.Marshal(e.Params)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO export_job(id,workspace_id,kind,target,params,status,created_by)
		 VALUES($1,$2,$3,$4,$5,'queued',$6)`,
		e.ID, e.WorkspaceID, e.Kind, e.Target, params, e.CreatedBy)
	if err != nil {
		return nil, err
	}
	e.Status = "queued"
	return &e, nil
}

func (r *Repo) FinishExport(ctx context.Context, id, status, path, errMsg string, rows int64) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE export_job SET status=$2,file_path=$3,error=$4,row_count=$5,finished_at=now() WHERE id=$1`,
		id, status, path, errMsg, rows)
	return err
}

func (r *Repo) GetExport(ctx context.Context, workspaceID, id string) (*model.ExportJob, error) {
	var e model.ExportJob
	var params []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,kind,target,params,status,file_path,row_count,error,created_by,created_at,finished_at
		 FROM export_job WHERE workspace_id=$1 AND id=$2`, workspaceID, id).
		Scan(&e.ID, &e.WorkspaceID, &e.Kind, &e.Target, &params, &e.Status, &e.FilePath,
			&e.RowCount, &e.Error, &e.CreatedBy, &e.CreatedAt, &e.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "导出任务不存在")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(params, &e.Params)
	return &e, nil
}

func (r *Repo) ListExports(ctx context.Context, workspaceID string, limit int) ([]model.ExportJob, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,kind,target,status,file_path,row_count,error,created_by,created_at,finished_at
		 FROM export_job WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ExportJob{}
	for rows.Next() {
		var e model.ExportJob
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Kind, &e.Target, &e.Status, &e.FilePath,
			&e.RowCount, &e.Error, &e.CreatedBy, &e.CreatedAt, &e.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- 订阅 ----------

func (r *Repo) SaveSubscription(ctx context.Context, workspaceID string, s model.Subscription) (*model.Subscription, error) {
	if s.ID == "" {
		s.ID = ids.New("sub")
	}
	s.WorkspaceID = workspaceID
	conf, err := json.Marshal(s.Config)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO subscription(id,workspace_id,name,kind,target,channel,endpoint,schedule,config,enabled)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (workspace_id,name) DO UPDATE SET
		   kind=EXCLUDED.kind, target=EXCLUDED.target, channel=EXCLUDED.channel, endpoint=EXCLUDED.endpoint,
		   schedule=EXCLUDED.schedule, config=EXCLUDED.config, enabled=EXCLUDED.enabled`,
		s.ID, workspaceID, s.Name, s.Kind, s.Target, s.Channel, s.Endpoint, s.Schedule, conf, s.Enabled)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Repo) ListSubscriptions(ctx context.Context, workspaceID string) ([]model.Subscription, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,kind,target,channel,endpoint,schedule,config,enabled,last_run_at,last_status,created_at
		 FROM subscription WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Subscription{}
	for rows.Next() {
		var s model.Subscription
		var conf []byte
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Kind, &s.Target, &s.Channel,
			&s.Endpoint, &s.Schedule, &conf, &s.Enabled, &s.LastRunAt, &s.LastStatus, &s.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(conf, &s.Config)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) MarkSubscriptionRun(ctx context.Context, id, status string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE subscription SET last_run_at=now(), last_status=$2 WHERE id=$1`, id, status)
	return err
}

func (r *Repo) DeleteSubscription(ctx context.Context, workspaceID, id string) error {
	tag, err := r.DB.Pool.Exec(ctx, `DELETE FROM subscription WHERE workspace_id=$1 AND id=$2`, workspaceID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "订阅不存在")
	}
	return nil
}

// ---------- 分享 ----------

func (r *Repo) CreateShare(ctx context.Context, workspaceID, kind, target, createdBy string, ttl time.Duration) (*model.ShareLink, error) {
	s := &model.ShareLink{
		Token:       ids.New("shr"),
		WorkspaceID: workspaceID,
		Kind:        kind,
		Target:      target,
		CreatedBy:   createdBy,
	}
	var expires any
	if ttl > 0 {
		t := time.Now().Add(ttl)
		s.ExpiresAt = &t
		expires = t
	}
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO share_link(token,workspace_id,kind,target,expires_at,created_by)
		 VALUES($1,$2,$3,$4,$5,$6)`, s.Token, workspaceID, kind, target, expires, createdBy)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = time.Now()
	return s, nil
}

func (r *Repo) GetShare(ctx context.Context, token string) (*model.ShareLink, error) {
	var s model.ShareLink
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT token,workspace_id,kind,target,expires_at,created_by,created_at
		 FROM share_link WHERE token=$1 AND (expires_at IS NULL OR expires_at > now())`, token).
		Scan(&s.Token, &s.WorkspaceID, &s.Kind, &s.Target, &s.ExpiresAt, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "分享链接不存在或已过期")
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Repo) ListShares(ctx context.Context, workspaceID string) ([]model.ShareLink, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT token,workspace_id,kind,target,expires_at,created_by,created_at
		 FROM share_link WHERE workspace_id=$1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ShareLink{}
	for rows.Next() {
		var s model.ShareLink
		if err := rows.Scan(&s.Token, &s.WorkspaceID, &s.Kind, &s.Target, &s.ExpiresAt, &s.CreatedBy, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteShare(ctx context.Context, workspaceID, token string) error {
	_, err := r.DB.Pool.Exec(ctx, `DELETE FROM share_link WHERE workspace_id=$1 AND token=$2`, workspaceID, token)
	return err
}

// ---------- 成员 ----------

func (r *Repo) ListMembers(ctx context.Context, workspaceID string) ([]model.Member, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT workspace_id,user_id,role,created_at FROM workspace_member WHERE workspace_id=$1 ORDER BY user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Member{}
	for rows.Next() {
		var m model.Member
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) AddMember(ctx context.Context, workspaceID, userID, role string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO workspace_member(workspace_id,user_id,role) VALUES($1,$2,$3)
		 ON CONFLICT (workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role`, workspaceID, userID, role)
	return err
}

func (r *Repo) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`DELETE FROM workspace_member WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID)
	return err
}

// RoleOf 返回用户在指定工作区的角色；未登记时回退为 viewer。
func (r *Repo) RoleOf(ctx context.Context, workspaceID, userID string) string {
	var role string
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT role FROM workspace_member WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

// ListEnabledSubscriptions 返回全部启用订阅（跨工作区），供调度器扫描。
func (r *Repo) ListEnabledSubscriptions(ctx context.Context) ([]model.Subscription, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,kind,target,channel,endpoint,schedule,config,enabled,last_run_at,last_status
		 FROM subscription WHERE enabled ORDER BY workspace_id,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Subscription{}
	for rows.Next() {
		var s model.Subscription
		var conf []byte
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Kind, &s.Target, &s.Channel,
			&s.Endpoint, &s.Schedule, &conf, &s.Enabled, &s.LastRunAt, &s.LastStatus); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(conf, &s.Config)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------- 工作区（多租户） ----------

// ListWorkspacesFor 返回用户可见的工作区。admin 可看到全部；其他人只看自己是成员的。
func (r *Repo) ListWorkspacesFor(ctx context.Context, userID, role string) ([]model.Workspace, error) {
	q := `SELECT w.id,w.name,w.description,w.created_at FROM workspace w ORDER BY w.created_at`
	args := []any{}
	if role != "admin" {
		q = `SELECT w.id,w.name,w.description,w.created_at
		     FROM workspace w JOIN workspace_member m ON m.workspace_id=w.id
		     WHERE m.user_id=$1 ORDER BY w.created_at`
		args = append(args, userID)
	}
	rows, err := r.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Workspace{}
	for rows.Next() {
		var w model.Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Description, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *Repo) WorkspaceByID(ctx context.Context, id string) (*model.Workspace, error) {
	var w model.Workspace
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,name,description,created_at FROM workspace WHERE id=$1`, id).
		Scan(&w.ID, &w.Name, &w.Description, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "工作区不存在")
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *Repo) CreateWorkspace(ctx context.Context, name, description string) (*model.Workspace, error) {
	w := &model.Workspace{ID: ids.New("ws"), Name: name, Description: description, CreatedAt: time.Now()}
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO workspace(id,name,description) VALUES($1,$2,$3)`, w.ID, name, description)
	if isUnique(err) {
		return nil, errs.New(errs.CodeConflict, "工作区名称已存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// TenantUsage 汇总单个工作区的资源用量，用于成本核算与配额。
func (r *Repo) TenantUsage(ctx context.Context, workspaceID string) (map[string]any, error) {
	var dsCount, modelCount, pipeCount, instCount, runsLast30 int64
	var bytes int64
	err := r.DB.Pool.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM datasource WHERE workspace_id=$1),
  (SELECT COUNT(*) FROM model WHERE workspace_id=$1),
  (SELECT COUNT(*) FROM pipeline WHERE workspace_id=$1),
  (SELECT COUNT(*) FROM instance WHERE workspace_id=$1),
  (SELECT COUNT(*) FROM instance WHERE workspace_id=$1 AND created_at > now() - interval '30 days'),
  (SELECT COALESCE(SUM(pg_column_size(spec)),0) FROM model_version v
     JOIN model m ON m.id=v.model_id WHERE m.workspace_id=$1)`, workspaceID).
		Scan(&dsCount, &modelCount, &pipeCount, &instCount, &runsLast30, &bytes)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"datasources": dsCount, "models": modelCount, "pipelines": pipeCount,
		"instances": instCount, "runsLast30d": runsLast30, "modelSpecBytes": bytes,
	}, nil
}

// CachePurgeByWorkspace 清掉某个工作区的全部缓存。
// 管道跑完后调用：宁可让下一次查询慢一点，也不要返回上一轮的旧数。
func (r *Repo) CachePurgeByWorkspace(ctx context.Context, workspaceID string) (int64, error) {
	tag, err := r.DB.Pool.Exec(ctx, `DELETE FROM query_cache WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AllColumnPolicies 返回工作空间全部列策略，供数据字典一次性取齐。
func (r *Repo) AllColumnPolicies(ctx context.Context, workspaceID string) ([]model.ColumnPolicy, error) {
	return r.ListColumnPolicies(ctx, workspaceID, "")
}

// AllColumnEdges 返回工作空间全部列级血缘，用于整图渲染时补列映射。
func (r *Repo) AllColumnEdges(ctx context.Context, workspaceID string) ([]model.ColumnEdge, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT upstream_table,upstream_column,downstream_table,downstream_column
		 FROM lineage_column WHERE workspace_id=$1
		 ORDER BY downstream_table,downstream_column LIMIT 20000`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ColumnEdge{}
	for rows.Next() {
		var e model.ColumnEdge
		if err := rows.Scan(&e.UpstreamTable, &e.UpstreamColumn, &e.DownstreamTable, &e.DownstreamColumn); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
