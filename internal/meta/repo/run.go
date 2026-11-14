package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/jackc/pgx/v5"
)

// ---------- instance ----------

// CreateInstance 幂等地创建实例。返回 created=false 表示同一幂等键已存在，
// 调用方应直接复用已有实例，不要重复派发。
func (r *Repo) CreateInstance(ctx context.Context, inst model.Instance) (*model.Instance, bool, error) {
	inst.ID = ids.New("ins")
	inst.CreatedAt = time.Now()
	if inst.Status == "" {
		inst.Status = "queued"
	}
	params, err := json.Marshal(inst.Params)
	if err != nil {
		return nil, false, err
	}
	tag, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO instance(id,workspace_id,pipeline_id,pipeline_ver,biz_date,trigger_type,status,idempotency_key,params)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (workspace_id,idempotency_key) DO NOTHING`,
		inst.ID, inst.WorkspaceID, inst.PipelineID, inst.PipelineVer, inst.BizDate,
		inst.TriggerType, inst.Status, inst.IdempotencyKey, params)
	if err != nil {
		return nil, false, err
	}
	if tag.RowsAffected() == 0 {
		existing, err := r.instanceByIdemKey(ctx, inst.WorkspaceID, inst.IdempotencyKey)
		return existing, false, err
	}
	return &inst, true, nil
}

func (r *Repo) instanceByIdemKey(ctx context.Context, workspaceID, key string) (*model.Instance, error) {
	row := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,pipeline_id,pipeline_ver,biz_date::text,trigger_type,status,
		        idempotency_key,params,error,worker_owner,lease_until,started_at,finished_at,created_at
		 FROM instance WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, key)
	return scanInstance(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(row rowScanner) (*model.Instance, error) {
	var i model.Instance
	var params []byte
	err := row.Scan(&i.ID, &i.WorkspaceID, &i.PipelineID, &i.PipelineVer, &i.BizDate,
		&i.TriggerType, &i.Status, &i.IdempotencyKey, &params, &i.Error,
		&i.WorkerOwner, &i.LeaseUntil, &i.StartedAt, &i.FinishedAt, &i.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "实例不存在")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(params, &i.Params)
	return &i, nil
}

// AddTasks 为实例写入全部节点，初始状态 pending。
func (r *Repo) AddTasks(ctx context.Context, instanceID string, nodes []model.NodeSpec) error {
	if len(nodes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, n := range nodes {
		batch.Queue(
			`INSERT INTO task_instance(id,instance_id,node_id,node_type,status)
			 VALUES($1,$2,$3,$4,'pending') ON CONFLICT (instance_id,node_id) DO NOTHING`,
			ids.New("tsk"), instanceID, n.ID, n.Type)
	}
	return r.DB.Pool.SendBatch(ctx, batch).Close()
}

// ClaimInstance 领取一个排队实例。SKIP LOCKED 保证多个 worker 不会拿到同一个，
// 租约超时的 running 实例也会被回收重试。
func (r *Repo) ClaimInstance(ctx context.Context, worker string, lease time.Duration) (*model.Instance, error) {
	row := r.DB.Pool.QueryRow(ctx,
		`UPDATE instance SET status='running',
		        worker_owner=$1, lease_until=now()+$2::interval, started_at=COALESCE(started_at,now())
		 WHERE id = (
		   SELECT id FROM instance
		   WHERE status='queued'
		      OR (status='running' AND lease_until IS NOT NULL AND lease_until < now())
		   ORDER BY created_at
		   FOR UPDATE SKIP LOCKED
		   LIMIT 1
		 )
		 RETURNING id,workspace_id,pipeline_id,pipeline_ver,biz_date::text,trigger_type,status,
		           idempotency_key,params,error,worker_owner,lease_until,started_at,finished_at,created_at`,
		worker, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	inst, err := scanInstance(row)
	if err != nil {
		if errs.Is(err, errs.CodeNotFound) {
			return nil, nil // 无任务可取
		}
		return nil, err
	}
	return inst, nil
}

// Heartbeat 续期。返回 CodeLeaseLost 表示已被别人接管，当前 worker 必须停止写入。
func (r *Repo) Heartbeat(ctx context.Context, instanceID, worker string, lease time.Duration) error {
	tag, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET lease_until=now()+$3::interval
		 WHERE id=$1 AND worker_owner=$2 AND status='running'`,
		instanceID, worker, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeLeaseLost, "实例租约已失效: "+instanceID)
	}
	return nil
}

func (r *Repo) FinishInstance(ctx context.Context, instanceID, status, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET status=$2, error=$3, finished_at=now(), lease_until=NULL
		 WHERE id=$1`, instanceID, status, errMsg)
	return err
}

func (r *Repo) StartTask(ctx context.Context, instanceID, nodeID string, attempt int) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE task_instance SET status='running', attempt=$3, started_at=now()
		 WHERE instance_id=$1 AND node_id=$2`, instanceID, nodeID, attempt)
	return err
}

func (r *Repo) FinishTask(ctx context.Context, instanceID, nodeID, status string, rows, durationMS int64, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE task_instance SET status=$3, rows_affected=$4, duration_ms=$5, error=$6, finished_at=now()
		 WHERE instance_id=$1 AND node_id=$2`, instanceID, nodeID, status, rows, durationMS, errMsg)
	return err
}

func (r *Repo) SkipDownstream(ctx context.Context, instanceID string, nodeIDs []string, reason string) error {
	for _, id := range nodeIDs {
		if _, err := r.DB.Pool.Exec(ctx,
			`UPDATE task_instance SET status='blocked', error=$3, finished_at=now()
			 WHERE instance_id=$1 AND node_id=$2 AND status='pending'`, instanceID, id, reason); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) GetInstance(ctx context.Context, id string) (*model.Instance, error) {
	// 详情页要展示管道名与资源组，一次 join 取全，免得前端再发一次请求。
	inst := &model.Instance{}
	var params []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT i.id,i.workspace_id,i.pipeline_id,i.pipeline_ver,i.biz_date::text,i.trigger_type,i.status,
		        i.idempotency_key,i.params,i.error,i.worker_owner,i.lease_until,i.started_at,i.finished_at,i.created_at,
		        COALESCE(p.name,''), COALESCE(i.resource_group,''), COALESCE(i.priority,0)
		   FROM instance i LEFT JOIN pipeline p ON p.id = i.pipeline_id
		  WHERE i.id=$1`, id).
		Scan(&inst.ID, &inst.WorkspaceID, &inst.PipelineID, &inst.PipelineVer, &inst.BizDate,
			&inst.TriggerType, &inst.Status, &inst.IdempotencyKey, &params, &inst.Error,
			&inst.WorkerOwner, &inst.LeaseUntil, &inst.StartedAt, &inst.FinishedAt, &inst.CreatedAt,
			&inst.PipelineName, &inst.ResourceGroup, &inst.Priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "实例不存在")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(params, &inst.Params)
	tasks, err := r.Tasks(ctx, id)
	if err != nil {
		return nil, err
	}
	inst.Tasks = tasks
	return inst, nil
}

func (r *Repo) Tasks(ctx context.Context, instanceID string) ([]model.TaskInstance, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,instance_id,node_id,node_type,status,attempt,lease_owner,lease_until,
		        rows_affected,duration_ms,error,started_at,finished_at
		 FROM task_instance WHERE instance_id=$1 ORDER BY started_at NULLS FIRST, node_id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TaskInstance{}
	for rows.Next() {
		var t model.TaskInstance
		if err := rows.Scan(&t.ID, &t.InstanceID, &t.NodeID, &t.NodeType, &t.Status, &t.Attempt,
			&t.LeaseOwner, &t.LeaseUntil, &t.RowsAffected, &t.DurationMS, &t.Error,
			&t.StartedAt, &t.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const instanceCols = `i.id,i.workspace_id,i.pipeline_id,COALESCE(p.name,''),i.pipeline_ver,i.biz_date::text,
	i.trigger_type,i.status,i.idempotency_key,i.params,i.error,i.worker_owner,
	i.lease_until,i.started_at,i.finished_at,i.created_at`

// ListInstancesPage 返回筛选后的实例与总数（总数用于前端分页，不受 LIMIT 影响）。
func (r *Repo) ListInstancesPage(ctx context.Context, workspaceID string, q model.InstanceQuery) ([]model.Instance, int, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	args := []any{workspaceID}
	where := "i.workspace_id=$1"
	if q.Pipeline != "" {
		args = append(args, q.Pipeline)
		where += fmt.Sprintf(" AND p.name=$%d", len(args))
	}
	if q.Status != "" {
		args = append(args, q.Status)
		where += fmt.Sprintf(" AND i.status=$%d", len(args))
	}
	if q.From != "" {
		args = append(args, q.From)
		where += fmt.Sprintf(" AND i.biz_date >= $%d::date", len(args))
	}
	if q.To != "" {
		args = append(args, q.To)
		where += fmt.Sprintf(" AND i.biz_date <= $%d::date", len(args))
	}

	var total int
	if err := r.DB.Pool.QueryRow(ctx,
		`SELECT count(*) FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id WHERE `+where,
		args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []model.Instance{}, 0, nil
	}

	args = append(args, q.Limit, q.Offset)
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT `+instanceCols+`
		 FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		 WHERE `+where+`
		 ORDER BY i.created_at DESC LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []model.Instance{}
	for rows.Next() {
		var i model.Instance
		var params []byte
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.PipelineID, &i.PipelineName, &i.PipelineVer, &i.BizDate,
			&i.TriggerType, &i.Status, &i.IdempotencyKey, &params, &i.Error,
			&i.WorkerOwner, &i.LeaseUntil, &i.StartedAt, &i.FinishedAt, &i.CreatedAt); err != nil {
			return nil, 0, err
		}
		_ = json.Unmarshal(params, &i.Params)
		out = append(out, i)
	}
	return out, total, rows.Err()
}

// ResetTasks 把指定节点打回 pending，供「从某个节点重跑」使用。
// 不清工作簿之外的任何东西：上游的成功状态保留，执行器会跳过它们。
func (r *Repo) ResetTasks(ctx context.Context, instanceID string, nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	if _, err := r.DB.Pool.Exec(ctx,
		`UPDATE task_instance SET status='pending', error='', rows_affected=0, duration_ms=0,
		        started_at=NULL, finished_at=NULL, lease_owner='', lease_until=NULL
		 WHERE instance_id=$1 AND node_id = ANY($2)`, instanceID, nodeIDs); err != nil {
		return err
	}
	// 质量结果按 (实例, 节点) 追加写入，重跑前必须清掉，否则同一规则会出现两条记录。
	_, err := r.DB.Pool.Exec(ctx,
		`DELETE FROM quality_result WHERE instance_id=$1 AND node_id = ANY($2)`, instanceID, nodeIDs)
	return err
}

// RequeueInstance 把实例退回队列，由 worker 重新领取（原地重跑，不新建实例）。
func (r *Repo) RequeueInstance(ctx context.Context, instanceID, reason string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET status='queued', error=$2, finished_at=NULL, started_at=NULL,
		        worker_owner='', lease_until=NULL, queued_at=now()
		 WHERE id=$1`, instanceID, reason)
	return err
}

func (r *Repo) ListInstances(ctx context.Context, workspaceID string, limit int) ([]model.Instance, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT i.id,i.workspace_id,i.pipeline_id,COALESCE(p.name,''),i.pipeline_ver,i.biz_date::text,
		        i.trigger_type,i.status,i.idempotency_key,i.params,i.error,i.worker_owner,
		        i.lease_until,i.started_at,i.finished_at,i.created_at
		 FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		 WHERE i.workspace_id=$1 ORDER BY i.created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Instance{}
	for rows.Next() {
		var i model.Instance
		var params []byte
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.PipelineID, &i.PipelineName, &i.PipelineVer, &i.BizDate,
			&i.TriggerType, &i.Status, &i.IdempotencyKey, &params, &i.Error,
			&i.WorkerOwner, &i.LeaseUntil, &i.StartedAt, &i.FinishedAt, &i.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(params, &i.Params)
		out = append(out, i)
	}
	return out, rows.Err()
}

// PreviousInstance 取同管道里「上一轮」实例，用于渲染 SQL 与本次对比。
// 只按创建时间往前找一条：同一业务日期手动重跑多次时，最近一次才是有效的对比基准。
func (r *Repo) PreviousInstance(ctx context.Context, workspaceID, pipelineID, instanceID string, before time.Time) (*model.Instance, error) {
	inst := &model.Instance{}
	var params []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT `+instanceCols+`
		 FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		 WHERE i.workspace_id=$1 AND i.pipeline_id=$2 AND i.id<>$3 AND i.created_at < $4
		 ORDER BY i.created_at DESC LIMIT 1`, workspaceID, pipelineID, instanceID, before).
		Scan(&inst.ID, &inst.WorkspaceID, &inst.PipelineID, &inst.PipelineName, &inst.PipelineVer, &inst.BizDate,
			&inst.TriggerType, &inst.Status, &inst.IdempotencyKey, &params, &inst.Error,
			&inst.WorkerOwner, &inst.LeaseUntil, &inst.StartedAt, &inst.FinishedAt, &inst.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(params, &inst.Params)
	return inst, nil
}

// RequeueStale 把租约超时的实例退回 queued，由下一轮重新领取。
// 已成功的节点会保留状态，重跑时按「跳过成功节点」续跑。
func (r *Repo) RequeueStale(ctx context.Context, grace time.Duration) (int, error) {
	tag, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET status='queued', worker_owner='', lease_until=NULL,
		        error=COALESCE(NULLIF(error,''),'租约超时，自动重排队')
		 WHERE status='running' AND lease_until IS NOT NULL
		   AND lease_until < now() - $1::interval`, fmt.Sprintf("%d seconds", int(grace.Seconds())))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ---------- quality ----------

func (r *Repo) SaveQualityResults(ctx context.Context, instanceID, nodeID string, results []struct {
	RuleName, Observed, Expected, Severity string
	Passed                                 bool
}) error {
	for _, q := range results {
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO quality_result(id,instance_id,node_id,rule_name,passed,observed,expected,severity)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			ids.New("qr"), instanceID, nodeID, q.RuleName, q.Passed, q.Observed, q.Expected, q.Severity); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) QualityResults(ctx context.Context, instanceID string) ([]model.QualityResult, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,instance_id,node_id,rule_name,passed,observed,expected,severity,created_at
		 FROM quality_result WHERE instance_id=$1 ORDER BY created_at`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.QualityResult{}
	for rows.Next() {
		var q model.QualityResult
		if err := rows.Scan(&q.ID, &q.InstanceID, &q.NodeID, &q.RuleName, &q.Passed,
			&q.Observed, &q.Expected, &q.Severity, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ---------- lineage ----------

func (r *Repo) ReplaceLineage(ctx context.Context, workspaceID, downstream string, upstream []string) error {
	if _, err := r.DB.Pool.Exec(ctx, `DELETE FROM lineage_edge WHERE workspace_id=$1 AND downstream=$2`, workspaceID, downstream); err != nil {
		return err
	}
	for _, up := range upstream {
		if up == "" || up == downstream {
			continue
		}
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO lineage_edge(workspace_id,upstream,downstream) VALUES($1,$2,$3)
			 ON CONFLICT DO NOTHING`, workspaceID, up, downstream); err != nil {
			return err
		}
	}
	return nil
}

// LineageGraph 返回全量边，前端用它画图；规模在万级以内足够。
func (r *Repo) LineageGraph(ctx context.Context, workspaceID string) ([][2]string, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT upstream,downstream FROM lineage_edge WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][2]string{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// ---------- audit ----------

func (r *Repo) Audit(ctx context.Context, a model.AuditLog) error {
	detail, err := json.Marshal(a.Detail)
	if err != nil {
		return err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO audit_log(workspace_id,actor,action,resource,detail,request_id)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		a.WorkspaceID, a.Actor, a.Action, a.Resource, detail, a.RequestID)
	return err
}

func (r *Repo) ListAudit(ctx context.Context, workspaceID string, limit int) ([]model.AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,actor,action,resource,detail,request_id,created_at
		 FROM audit_log WHERE workspace_id=$1 ORDER BY id DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditLog{}
	for rows.Next() {
		var a model.AuditLog
		var detail []byte
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Actor, &a.Action, &a.Resource, &detail, &a.RequestID, &a.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &a.Detail)
		out = append(out, a)
	}
	return out, rows.Err()
}

// QualityOverview 按规则聚合最近 N 次运行的结果，用于质量看板。
func (r *Repo) QualityOverview(ctx context.Context, workspaceID string, limit int) ([]map[string]any, error) {
	rows, err := r.DB.Pool.Query(ctx, `
SELECT rule_name,
       COUNT(*) AS total,
       COUNT(*) FILTER (WHERE passed) AS passed,
       COUNT(*) FILTER (WHERE NOT passed) AS failed,
       MAX(q.created_at) AS last_at
FROM quality_result q
JOIN instance i ON i.id = q.instance_id
WHERE i.workspace_id = $1
GROUP BY rule_name
ORDER BY failed DESC, rule_name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var total, passed, failed int64
		var last time.Time
		if err := rows.Scan(&name, &total, &passed, &failed, &last); err != nil {
			return nil, err
		}
		rate := 1.0
		if total > 0 {
			rate = float64(passed) / float64(total)
		}
		out = append(out, map[string]any{
			"rule": name, "total": total, "passed": passed, "failed": failed,
			"passRate": rate, "lastAt": last,
		})
	}
	return out, rows.Err()
}

// QualityTrend 返回最近 N 天的通过率趋势。
func (r *Repo) QualityTrend(ctx context.Context, workspaceID string, days int) ([]map[string]any, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	rows, err := r.DB.Pool.Query(ctx, `
SELECT to_char(date_trunc('day', q.created_at), 'YYYY-MM-DD') AS d,
       COUNT(*) AS total,
       COUNT(*) FILTER (WHERE passed) AS passed
FROM quality_result q
JOIN instance i ON i.id = q.instance_id
WHERE i.workspace_id = $1 AND q.created_at >= now() - ($2::int * interval '1 day')
GROUP BY 1 ORDER BY 1`, workspaceID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var d string
		var total, passed int64
		if err := rows.Scan(&d, &total, &passed); err != nil {
			return nil, err
		}
		rate := 1.0
		if total > 0 {
			rate = float64(passed) / float64(total)
		}
		out = append(out, map[string]any{"date": d, "total": total, "passed": passed, "passRate": rate})
	}
	return out, rows.Err()
}

// AddLineage 只新增边，不删除已有边。
// 模型发布时用它可以避免把管道提交阶段记录的模型间依赖一起清掉。
func (r *Repo) AddLineage(ctx context.Context, workspaceID, downstream string, upstream []string) error {
	for _, up := range upstream {
		if up == "" || up == downstream {
			continue
		}
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO lineage_edge(workspace_id,upstream,downstream) VALUES($1,$2,$3)
			 ON CONFLICT DO NOTHING`, workspaceID, up, downstream); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 队列（T0504 / T0510） ----------

// RunningCount 返回某资源组当前 running 实例数，用于分发前判断空位。
func (r *Repo) RunningCount(ctx context.Context, workspaceID, group string) (int, error) {
	var n int
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT count(*) FROM instance
		 WHERE workspace_id=$1 AND status='running' AND resource_group=$2`,
		workspaceID, group).Scan(&n)
	return n, err
}

// Promote 把 waiting/queued 的实例提升为 queued（或保持 queued 供 worker 领取）。
// 这里用 waiting 作为「已建但资源组满」的暂存态，queue 是「可被领取」。
func (r *Repo) Promote(ctx context.Context, workspaceID, group string, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET status='queued', queued_at=COALESCE(queued_at, now())
		 WHERE id IN (
		   SELECT id FROM instance
		   WHERE workspace_id=$1 AND resource_group=$2 AND status='waiting'
		   ORDER BY priority, created_at
		   LIMIT $3
		 )`, workspaceID, group, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// QueueItem 是排队视图的一行。
type QueueItem struct {
	InstanceID    string `json:"instanceId"`
	PipelineID    string `json:"pipelineId"`
	PipelineName  string `json:"pipelineName"`
	ResourceGroup string `json:"resourceGroup"`
	Priority      int    `json:"priority"`
	Status        string `json:"status"`
	QueuedAt      string `json:"queuedAt,omitempty"`
}

// PendingInstances 返回排队中的实例，按优先级与入队时间排序。
func (r *Repo) PendingInstances(ctx context.Context, workspaceID string, limit int) ([]QueueItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT i.id, i.pipeline_id, COALESCE(p.name,''), i.resource_group, i.priority, i.status,
		        COALESCE(to_char(i.queued_at,'YYYY-MM-DD"T"HH24:MI:SSOF'),'')
		 FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		 WHERE i.workspace_id=$1 AND i.status IN ('queued','waiting')
		 ORDER BY i.priority, i.created_at LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueItem{}
	for rows.Next() {
		var q QueueItem
		if err := rows.Scan(&q.InstanceID, &q.PipelineID, &q.PipelineName, &q.ResourceGroup,
			&q.Priority, &q.Status, &q.QueuedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SetInstanceResources 记录资源组与优先级，创建实例时调用。
func (r *Repo) SetInstanceResources(ctx context.Context, instanceID, group string, priority int) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET resource_group=$2, priority=$3 WHERE id=$1`, instanceID, group, priority)
	return err
}

// RecordInstanceUsage 记录实例的资源用量，供成本核算与配额分析使用。
func (r *Repo) RecordInstanceUsage(ctx context.Context, instanceID string, cpuMS, scannedBytes int64) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE instance SET cpu_ms=$2, scanned_bytes=$3 WHERE id=$1`,
		instanceID, cpuMS, scannedBytes)
	return err
}

// SetInstanceStatus 直接改实例状态，用于队列提升与门禁阻断等状态机迁移。
func (r *Repo) SetInstanceStatus(ctx context.Context, instanceID, status string) error {
	_, err := r.DB.Pool.Exec(ctx, `UPDATE instance SET status=$2 WHERE id=$1`, instanceID, status)
	return err
}

// PreviousInstanceRows 返回同管道上一业务日期的总写入行数，用于数据量环比告警。
func (r *Repo) PreviousInstanceRows(ctx context.Context, workspaceID, pipelineID, bizDate string) (int64, error) {
	var rows int64
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(t.rows_affected),0)
		 FROM instance i JOIN task_instance t ON t.instance_id=i.id
		 WHERE i.workspace_id=$1 AND i.pipeline_id=$2 AND i.biz_date < $3::date
		   AND i.status='success'
		   AND i.biz_date = (SELECT MAX(biz_date) FROM instance
		                     WHERE workspace_id=$1 AND pipeline_id=$2 AND biz_date < $3::date AND status='success')`,
		workspaceID, pipelineID, bizDate).Scan(&rows)
	return rows, err
}

// CancelQueuedInstance 只取消「还没被 worker 领取」的实例。
//
// running 的实例不做强杀：worker 正在跑某条 SQL，中途杀进程会让目标表处于
// 半写状态（尤其 overwrite 类写入）。与其给一个假的「已终止」，不如明确告知
// 需要等当前节点结束。这是能力边界，不是缺陷。
func (r *Repo) CancelQueuedInstance(ctx context.Context, id string) (string, error) {
	var status string
	if err := r.DB.Pool.QueryRow(ctx, `SELECT status FROM instance WHERE id=$1`, id).Scan(&status); err != nil {
		return "", err
	}
	switch status {
	case "pending", "queued", "waiting":
		if _, err := r.DB.Pool.Exec(ctx,
			`UPDATE instance SET status='canceled', finished_at=now(), error='用户取消'
			 WHERE id=$1 AND status IN ('pending','queued','waiting')`, id); err != nil {
			return status, err
		}
		if _, err := r.DB.Pool.Exec(ctx,
			`UPDATE task_instance SET status='skipped', error='用户取消', finished_at=now()
			 WHERE instance_id=$1 AND status='pending'`, id); err != nil {
			return status, err
		}
		return status, nil
	case "running":
		return status, errs.New(errs.CodeConflict, "实例正在运行，需等当前节点结束后才能取消")
	default:
		return status, errs.New(errs.CodeConflict, "实例已结束（"+status+"），无需取消")
	}
}

// InstanceMatrix 返回最近 N 次运行的「节点 × 运行」状态矩阵。
//
// 两次查询而不是一次 JOIN：实例与任务是一对多，JOIN 之后还得按实例去重，
// 反而更容易写错；先取实例 id，再用 ANY($1) 一次取完任务更直白。
func (r *Repo) InstanceMatrix(ctx context.Context, workspaceID, pipeline string, limit int) (*model.InstanceMatrix, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	args := []any{workspaceID}
	where := "i.workspace_id=$1"
	if pipeline != "" {
		args = append(args, pipeline)
		where += fmt.Sprintf(" AND p.name=$%d", len(args))
	}
	args = append(args, limit)

	rows, err := r.DB.Pool.Query(ctx,
		`SELECT i.id, i.biz_date::text, i.status, i.started_at
		 FROM instance i LEFT JOIN pipeline p ON p.id=i.pipeline_id
		 WHERE `+where+`
		 ORDER BY i.created_at DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &model.InstanceMatrix{
		Pipeline: pipeline,
		Runs:     []model.MatrixRun{},
		Nodes:    []string{},
		Cells:    map[string][]*model.MatrixCell{},
	}
	for rows.Next() {
		var run model.MatrixRun
		if err := rows.Scan(&run.ID, &run.BizDate, &run.Status, &run.StartedAt); err != nil {
			return nil, err
		}
		out.Runs = append(out.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Runs) == 0 {
		return out, nil
	}

	ids := make([]string, len(out.Runs))
	at := make(map[string]int, len(out.Runs))
	for i, run := range out.Runs {
		ids[i] = run.ID
		at[run.ID] = i
	}

	trows, err := r.DB.Pool.Query(ctx,
		`SELECT instance_id, node_id, status, duration_ms, attempt, COALESCE(error,'')
		 FROM task_instance WHERE instance_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer trows.Close()

	for trows.Next() {
		var instID, nodeID string
		var c model.MatrixCell
		if err := trows.Scan(&instID, &nodeID, &c.Status, &c.DurationMS, &c.Attempt, &c.Error); err != nil {
			return nil, err
		}
		i, ok := at[instID]
		if !ok {
			continue
		}
		if out.Cells[nodeID] == nil {
			out.Cells[nodeID] = make([]*model.MatrixCell, len(out.Runs))
		}
		cell := c
		out.Cells[nodeID][i] = &cell
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}

	// 行序必须稳定：两次刷新顺序不同，会让人以为看错了表。
	for n := range out.Cells {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Strings(out.Nodes)
	return out, nil
}
