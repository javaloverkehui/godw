package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/sqlx"
)

// T0410 试跑与 Dry-run。
//
// 目标不是「跑一下看看」，而是「在不产生任何副作用的前提下，
// 把将要执行的东西完整摊开给人看」。因此：
//   - dry-run 只渲染，不连库，绝不写目标表；
//   - sample-run 只跑 SELECT 且强制包 LIMIT，绝不跑写入语句。

// DryRunNode 是单个节点的试跑结果。
type DryRunNode struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Model      string   `json:"model,omitempty"`
	Datasource string   `json:"datasource,omitempty"`
	WriteMode  string   `json:"writeMode,omitempty"`
	SQL        string   `json:"sql,omitempty"`
	Refs       []string `json:"refs,omitempty"`
	// Inference 说明增量条件是怎么推导的。
	Inference *compile.Inference `json:"inference,omitempty"`
	// Managed 表示目标表由平台托管（会真的建表）。
	Managed bool `json:"managed"`
	// Error 是单节点渲染失败的原因，不影响其它节点继续渲染。
	Error string `json:"error,omitempty"`
}

// DryRunResult 是整条管道的试跑结果。
type DryRunResult struct {
	Pipeline string         `json:"pipeline"`
	BizDate  string         `json:"bizDate"`
	Params   map[string]any `json:"params"`
	Nodes    []DryRunNode   `json:"nodes"`
	Stages   [][]string     `json:"stages"`
	Warnings []string       `json:"warnings,omitempty"`
	// WouldWrite 列出会被写入的表，让「试跑」的影响面一目了然。
	WouldWrite []string `json:"wouldWrite,omitempty"`
}

// DryRun 渲染整条管道，不连库、不写库。
func (s *Service) DryRun(ctx context.Context, workspaceID string, spec model.PipelineSpec, bizDate string, extra map[string]any) (*DryRunResult, error) {
	if err := compile.Validate(&spec); err != nil {
		return nil, err
	}
	vars := map[string]any{"biz_date": bizDate, "biz_ts": bizDate + " 00:00:00"}
	for _, p := range spec.Params {
		if p.Default != nil {
			vars[p.Name] = p.Default
		}
	}
	for k, v := range extra {
		vars[k] = v
	}

	res := &DryRunResult{Pipeline: spec.Name, BizDate: bizDate, Params: vars, Nodes: []DryRunNode{}}

	for _, n := range compile.Topo(spec.Nodes) {
		item := DryRunNode{ID: n.ID, Type: n.Type, Model: n.Model, Datasource: n.Datasource, WriteMode: n.WriteMode}
		if strings.TrimSpace(n.SQL) != "" {
			rendered, err := sqlx.Render(n.SQL, vars)
			if err != nil {
				item.Error = err.Error()
			} else {
				item.SQL = rendered
				item.Refs = sqlx.Vars(n.SQL)
			}
		}

		// 目标模型已知时做增量推导，让用户看到「本次扫哪些分区」。
		if n.Model != "" && n.Type != model.NodeQuality {
			mspec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, n.Model)
			if err != nil {
				item.Error = firstNonEmpty(item.Error, "模型未发布: "+n.Model)
			} else {
				item.Managed = true
				res.WouldWrite = append(res.WouldWrite, n.Model)
				explicit := ""
				if n.Config != nil {
					explicit, _ = n.Config["where"].(string)
				}
				if inf, ierr := compile.InferIncremental(*mspec, bizDate, explicit); ierr == nil {
					item.Inference = &inf
					res.Warnings = append(res.Warnings, compile.ValidateIncremental(*mspec, inf)...)
					if item.SQL != "" && inf.Where != "" {
						item.SQL = compile.ApplyWhere(item.SQL, inf)
					}
				} else if n.WriteMode != model.WriteOverwrite && n.Type == model.NodeSQL {
					// 没有 where 的 SQL 节点必须有办法限定范围，否则报错提醒。
					item.Error = firstNonEmpty(item.Error, ierr.Error())
				}
			}
		}
		res.Nodes = append(res.Nodes, item)
	}

	// 执行计划按层分组，前端画甘特/泳道直接用。
	for i, stage := range compile.Stages(spec.Nodes) {
		ids := make([]string, len(stage))
		for j, n := range stage {
			ids[j] = n.ID
		}
		res.Stages = append(res.Stages, ids)
		_ = i
	}
	return res, nil
}

// SampleRunResult 是采样执行结果。
type SampleRunResult struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Count   int        `json:"count"`
	SQL     string     `json:"sql"`
	Elapsed int64      `json:"elapsedMs"`
	// Truncated 表示结果被 LIMIT 截断。
	Truncated bool `json:"truncated"`
}

// SampleRun 对管道中指定节点（默认第一个 SQL 节点）的 SELECT 部分包一层 LIMIT 真跑。
// 不写任何目标表：这里只取 SQL 文本，绝不生成 INSERT。
func (s *Service) SampleRun(ctx context.Context, workspaceID string, spec model.PipelineSpec, nodeID, bizDate string, limit int) (*SampleRunResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	vars := map[string]any{"biz_date": bizDate, "biz_ts": bizDate + " 00:00:00"}
	for _, p := range spec.Params {
		if p.Default != nil {
			vars[p.Name] = p.Default
		}
	}

	var target *model.NodeSpec
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if nodeID != "" && n.ID == nodeID {
			target = n
			break
		}
		if nodeID == "" && n.Type == model.NodeSQL && strings.TrimSpace(n.SQL) != "" {
			target = n
			break
		}
	}
	if target == nil {
		return nil, errs.New(errs.CodeNotFound, "找不到可采样的 SQL 节点")
	}

	sqlText, err := sqlx.Render(target.SQL, vars)
	if err != nil {
		return nil, errs.Wrap(errs.CodeCompileFailed, err, "节点渲染失败")
	}
	sqlText = strings.TrimRight(strings.TrimSpace(sqlText), ";")

	// 增量条件同样注入，否则采样扫的是全表，与真实执行不符。
	if target.Model != "" {
		if mspec, merr := s.Repo.PublishedModelSpec(ctx, workspaceID, target.Model); merr == nil {
			explicit := ""
			if target.Config != nil {
				explicit, _ = target.Config["where"].(string)
			}
			if inf, ierr := compile.InferIncremental(*mspec, bizDate, explicit); ierr == nil {
				sqlText = compile.ApplyWhere(sqlText, inf)
			}
		}
	}

	wrapped := fmt.Sprintf("SELECT * FROM (\n%s\n) godw_sample LIMIT %d", sqlText, limit+1)
	dsName := target.Datasource
	if dsName == "" && target.Model != "" {
		if mspec, merr := s.Repo.PublishedModelSpec(ctx, workspaceID, target.Model); merr == nil {
			dsName = mspec.Datasource
		}
	}
	if dsName == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "节点未指定数据源，无法采样")
	}

	dsSpec, err := s.DatasourceSpec(ctx, workspaceID, dsName)
	if err != nil {
		return nil, err
	}
	c, err := s.Pool.Acquire(ctx, dsSpec)
	if err != nil {
		return nil, err
	}

	rows, err := c.Query(ctx, wrapped)
	if err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "采样执行失败").WithMeta("sql", truncate(sqlText, 400))
	}
	defer rows.Close()

	out := &SampleRunResult{SQL: wrapped}
	cols := rows.Columns()
	out.Columns = make([]string, len(cols))
	for i, c := range cols {
		out.Columns[i] = c.Name
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		line := make([]string, len(vals))
		for i, v := range vals {
			line[i] = fmt.Sprint(deref(v))
		}
		out.Rows = append(out.Rows, line)
		if len(out.Rows) > limit {
			out.Truncated = true
			out.Rows = out.Rows[:limit]
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Count = len(out.Rows)
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func deref(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
