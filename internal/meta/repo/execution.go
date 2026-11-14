package repo

import (
	"context"
	"time"

	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/obs/logstore"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/jackc/pgx/v5"
)

// 执行面仓储：告警规则/事件、资源组、运行日志、队列视图。

// ---------- 告警 ----------

func (r *Repo) SaveAlertRule(ctx context.Context, workspaceID string, rule alert.Rule) (*alert.Rule, error) {
	if rule.ID == "" {
		rule.ID = ids.New("alr")
	}
	rule.WorkspaceID = workspaceID
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO alert_rule(id,workspace_id,name,kind,target,threshold,deadline,severity,channels,enabled,silence_from,silence_to)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (workspace_id,name) DO UPDATE SET
		   kind=EXCLUDED.kind, target=EXCLUDED.target, threshold=EXCLUDED.threshold,
		   deadline=EXCLUDED.deadline, severity=EXCLUDED.severity, channels=EXCLUDED.channels,
		   enabled=EXCLUDED.enabled, silence_from=EXCLUDED.silence_from, silence_to=EXCLUDED.silence_to`,
		rule.ID, workspaceID, rule.Name, string(rule.Kind), rule.Target, rule.Threshold,
		rule.Deadline, rule.Severity, rule.Channels, rule.Enabled, rule.SilenceFrom, rule.SilenceTo)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

func (r *Repo) ListAlertRules(ctx context.Context, workspaceID string) ([]alert.Rule, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,name,kind,target,threshold,deadline,severity,channels,enabled,silence_from,silence_to
		 FROM alert_rule WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []alert.Rule{}
	for rows.Next() {
		var a alert.Rule
		a.WorkspaceID = workspaceID
		var kind string
		if err := rows.Scan(&a.ID, &a.Name, &kind, &a.Target, &a.Threshold, &a.Deadline,
			&a.Severity, &a.Channels, &a.Enabled, &a.SilenceFrom, &a.SilenceTo); err != nil {
			return nil, err
		}
		a.Kind = alert.Kind(kind)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteAlertRule(ctx context.Context, workspaceID, name string) error {
	_, err := r.DB.Pool.Exec(ctx, `DELETE FROM alert_rule WHERE workspace_id=$1 AND name=$2`, workspaceID, name)
	return err
}

// AlertEvent 是告警事件的持久化形态。
type AlertEvent struct {
	ID         string    `json:"id"`
	RuleID     string    `json:"ruleId"`
	RuleName   string    `json:"ruleName"`
	Pipeline   string    `json:"pipeline"`
	InstanceID string    `json:"instanceId"`
	Severity   string    `json:"severity"`
	Title      string    `json:"title"`
	Detail     string    `json:"detail"`
	Status     string    `json:"status"`
	DedupeKey  string    `json:"dedupeKey"`
	Notified   bool      `json:"notified"`
	CreatedAt  time.Time `json:"createdAt"`
	HandledBy  string    `json:"handledBy,omitempty"`
}

// EmitAlert 写入告警事件。dedupe_key 冲突时返回已存在的事件（不重复打扰）。
func (r *Repo) EmitAlert(ctx context.Context, workspaceID string, ev alert.Event) (*AlertEvent, bool, error) {
	id := ids.New("ale")
	var out AlertEvent
	err := r.DB.Pool.QueryRow(ctx,
		`INSERT INTO alert_event(id,workspace_id,rule_id,rule_name,pipeline,instance_id,severity,title,detail,dedupe_key)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (workspace_id,dedupe_key) WHERE dedupe_key <> '' DO NOTHING
		 RETURNING id,rule_id,rule_name,pipeline,instance_id,severity,title,detail,status,dedupe_key,notified,created_at`,
		id, workspaceID, ev.RuleID, ev.RuleName, ev.Pipeline, ev.InstanceID, ev.Severity,
		ev.Title, ev.Detail, ev.DedupeKey).
		Scan(&out.ID, &out.RuleID, &out.RuleName, &out.Pipeline, &out.InstanceID, &out.Severity,
			&out.Title, &out.Detail, &out.Status, &out.DedupeKey, &out.Notified, &out.CreatedAt)
	if err == nil {
		return &out, true, nil
	}
	if err != pgx.ErrNoRows {
		return nil, false, err
	}
	// 已存在：查出原事件，标记「不重复发送」。
	err = r.DB.Pool.QueryRow(ctx,
		`SELECT id,rule_id,rule_name,pipeline,instance_id,severity,title,detail,status,dedupe_key,notified,created_at
		 FROM alert_event WHERE workspace_id=$1 AND dedupe_key=$2`, workspaceID, ev.DedupeKey).
		Scan(&out.ID, &out.RuleID, &out.RuleName, &out.Pipeline, &out.InstanceID, &out.Severity,
			&out.Title, &out.Detail, &out.Status, &out.DedupeKey, &out.Notified, &out.CreatedAt)
	if err != nil {
		return nil, false, err
	}
	return &out, false, nil
}

func (r *Repo) ListAlerts(ctx context.Context, workspaceID, status string, limit int) ([]AlertEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id,rule_id,rule_name,pipeline,instance_id,severity,title,detail,status,dedupe_key,notified,created_at,handled_by
	      FROM alert_event WHERE workspace_id=$1`
	args := []any{workspaceID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ` + itoa(limit)
	rows, err := r.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertEvent{}
	for rows.Next() {
		var a AlertEvent
		if err := rows.Scan(&a.ID, &a.RuleID, &a.RuleName, &a.Pipeline, &a.InstanceID, &a.Severity,
			&a.Title, &a.Detail, &a.Status, &a.DedupeKey, &a.Notified, &a.CreatedAt, &a.HandledBy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HandleAlert 更新告警处理状态（acknowledged / resolved）。
func (r *Repo) HandleAlert(ctx context.Context, workspaceID, id, status, actor string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE alert_event SET status=$3, handled_by=$4, handled_at=now()
		 WHERE workspace_id=$1 AND id=$2`, workspaceID, id, status, actor)
	return err
}

// MarkAlertNotified 标记已推送，避免重复发。
func (r *Repo) MarkAlertNotified(ctx context.Context, id string) error {
	_, err := r.DB.Pool.Exec(ctx, `UPDATE alert_event SET notified=true WHERE id=$1`, id)
	return err
}

// ---------- 资源组 ----------

func (r *Repo) ListResourceGroups(ctx context.Context, workspaceID string) ([]map[string]any, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT name,max_concurrency,max_timeout_sec,max_pool,engines FROM resource_group
		 WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var conc, timeout, pool int
		var engines []string
		if err := rows.Scan(&name, &conc, &timeout, &pool, &engines); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"name": name, "maxConcurrency": conc, "maxTimeoutSec": timeout,
			"maxPool": pool, "engines": engines,
		})
	}
	return out, rows.Err()
}

func (r *Repo) UpsertResourceGroup(ctx context.Context, workspaceID, name string, conc, timeout, pool int, engines []string) error {
	// nil 切片会被编码成 SQL NULL，而 engines 列是 NOT NULL + 默认 '{}'。
	// 这里显式转成空切片，让「不限制引擎」表达为「空数组」而不是「未知」。
	if engines == nil {
		engines = []string{}
	}
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO resource_group(id,workspace_id,name,max_concurrency,max_timeout_sec,max_pool,engines)
		 VALUES($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (workspace_id,name) DO UPDATE SET
		   max_concurrency=EXCLUDED.max_concurrency, max_timeout_sec=EXCLUDED.max_timeout_sec,
		   max_pool=EXCLUDED.max_pool, engines=EXCLUDED.engines`,
		ids.New("rg"), workspaceID, name, conc, timeout, pool, engines)
	return err
}

// ---------- 运行日志 ----------

// LogSink 实现 logstore.Sink：批量写 run_log 与按 seq 回读。
type LogSink struct{ DB *DB }

func (s LogSink) WriteBatch(ctx context.Context, instanceID string, lines []logstore.Line) error {
	if len(lines) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, l := range lines {
		batch.Queue(`INSERT INTO run_log(instance_id,node_id,seq,level,message,created_at)
		             VALUES($1,$2,$3,$4,$5,$6)`,
			instanceID, l.NodeID, int(l.Seq), l.Level, l.Message, l.At)
	}
	return s.DB.Pool.SendBatch(ctx, batch).Close()
}

func (s LogSink) ReadAfter(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]logstore.Line, error) {
	rows, err := s.DB.Pool.Query(ctx,
		`SELECT seq,node_id,level,message,created_at FROM run_log
		 WHERE instance_id=$1 AND seq>$2 ORDER BY seq LIMIT $3`, instanceID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []logstore.Line{}
	for rows.Next() {
		var l logstore.Line
		if err := rows.Scan(&l.Seq, &l.NodeID, &l.Level, &l.Message, &l.At); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------- 辅助 ----------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// RecordQualityWaiver 记录一次门禁放行。
func (r *Repo) RecordQualityWaiver(ctx context.Context, workspaceID, instanceID, nodeID, reason, actor string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO quality_waiver(id,workspace_id,instance_id,node_id,reason,actor)
		 VALUES($1,$2,$3,$4,$5,$6)`, ids.New("qwa"), workspaceID, instanceID, nodeID, reason, actor)
	return err
}

// ListQualityWaivers 返回某实例的放行记录。
func (r *Repo) ListQualityWaivers(ctx context.Context, workspaceID, instanceID string) ([]map[string]any, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,node_id,reason,actor,created_at FROM quality_waiver
		 WHERE workspace_id=$1 AND instance_id=$2 ORDER BY created_at DESC`, workspaceID, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, node, reason, actor string
		var at time.Time
		if err := rows.Scan(&id, &node, &reason, &actor, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "nodeId": node, "reason": reason, "actor": actor, "createdAt": at,
		})
	}
	return out, rows.Err()
}
