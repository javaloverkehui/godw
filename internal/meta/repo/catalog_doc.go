package repo

import (
	"context"
	"time"
)

// NodeRun 是「某个节点最近一次执行」的摘要，用于文档站显示「最近一次成功/失败」。
type NodeRun struct {
	PipelineID string
	NodeID     string
	InstanceID string
	Status     string
	BizDate    string
	FinishedAt time.Time
}

// LatestNodeRuns 一次取回全部管道里每个节点的最近一次终态（success/failed）。
//
// 为什么不在 Go 里遍历实例再拼：文档站一屏要显示几十个模型，
// 每个模型一次查询就是几十次往返；这里一条 DISTINCT ON 就能结束。
// 排序键用 finished_at，缺省回落到 created_at，避免「跑完但没写 finished_at」的旧数据排到最前。
func (r *Repo) LatestNodeRuns(ctx context.Context, workspaceID string) ([]NodeRun, error) {
	rows, err := r.DB.Pool.Query(ctx, `
SELECT DISTINCT ON (i.pipeline_id, t.node_id)
       i.pipeline_id, t.node_id, i.id, t.status,
       to_char(i.biz_date, 'YYYY-MM-DD'), COALESCE(t.finished_at, i.finished_at, i.created_at)
FROM task_instance t
JOIN instance i ON i.id = t.instance_id
WHERE i.workspace_id = $1 AND t.status IN ('success','failed')
ORDER BY i.pipeline_id, t.node_id, COALESCE(t.finished_at, i.finished_at, i.created_at) DESC`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeRun{}
	for rows.Next() {
		var n NodeRun
		if err := rows.Scan(&n.PipelineID, &n.NodeID, &n.InstanceID, &n.Status, &n.BizDate, &n.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QualitySnapshot 是某条质量规则最近一次的判定结果。
type QualitySnapshot struct {
	PipelineID string
	NodeID     string
	RuleName   string
	Passed     bool
	Observed   string
	At         time.Time
}

// LatestQualityResults 取每条规则最近一次的判定结果。
// 文档站要显示「这条规则上次过没过」，一条 DISTINCT ON 比逐规则查询便宜得多。
//
// 按 (管道, 节点, 规则名) 归组而不是只按规则名：规则名在不同模型里极易重名
// （id_not_null 到处都是），只按名字匹配会把 A 模型的结果显示到 B 模型上。
func (r *Repo) LatestQualityResults(ctx context.Context, workspaceID string) ([]QualitySnapshot, error) {
	rows, err := r.DB.Pool.Query(ctx, `
SELECT DISTINCT ON (i.pipeline_id, q.node_id, q.rule_name)
       i.pipeline_id, q.node_id, q.rule_name, q.passed, q.observed, q.created_at
FROM quality_result q
JOIN instance i ON i.id = q.instance_id
WHERE i.workspace_id = $1
ORDER BY i.pipeline_id, q.node_id, q.rule_name, q.created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QualitySnapshot{}
	for rows.Next() {
		var s QualitySnapshot
		if err := rows.Scan(&s.PipelineID, &s.NodeID, &s.RuleName, &s.Passed, &s.Observed, &s.At); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ModelVersionCounts 返回每个模型的版本数。文档站用它显示「改过几版」。
func (r *Repo) ModelVersionCounts(ctx context.Context, workspaceID string) (map[string]int, error) {
	rows, err := r.DB.Pool.Query(ctx, `
SELECT m.name, COUNT(v.id)
FROM model m LEFT JOIN model_version v ON v.model_id = m.id
WHERE m.workspace_id = $1
GROUP BY m.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}
