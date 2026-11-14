package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/lineage"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// RecordColumnLineage 在管道发布时分析每个 SQL 节点的列级血缘。
// 解析失败不阻断发布：退化为表级血缘，避免因为函数语法超出解析器能力而阻塞上线。
func (s *Service) RecordColumnLineage(ctx context.Context, workspaceID string, spec model.PipelineSpec) {
	// 物理表名（schema.table）到模型名的映射：SQL 里写的是 dwd.dwd_order，
	// 下游表用的是模型名 dwd_order，不归一化血缘就接不起来。
	alias := s.tableToModel(ctx, workspaceID)

	for _, n := range spec.Nodes {
		if n.Model == "" || strings.TrimSpace(n.SQL) == "" {
			continue
		}
		q, err := lineage.Analyze(n.SQL)
		if err != nil {
			s.Log.Warn("列级血缘解析失败，退化为表级", "node", n.ID, "err", err)
			continue
		}
		if q.HasStar {
			s.Log.Warn("SQL 使用 SELECT *，列级血缘不完整", "node", n.ID)
		}
		edges := make([]model.ColumnEdge, 0, len(q.Outputs)*2)
		for _, o := range q.Outputs {
			for _, in := range o.Inputs {
				if in.Table == "" || in.Column == "" {
					continue
				}
				up := normalizeTable(alias, in.Table)
				// 自引用不算血缘：dt 取自同名表的 dt，写在图上就是自环。
				if up == n.Model && strings.EqualFold(in.Column, o.Name) {
					continue
				}
				edges = append(edges, model.ColumnEdge{
					DownstreamTable:  n.Model,
					DownstreamColumn: o.Name,
					UpstreamTable:    up,
					UpstreamColumn:   in.Column,
					NodeID:           n.ID,
				})
			}
		}
		if err := s.Repo.ReplaceColumnLineage(ctx, workspaceID, n.Model, edges); err != nil {
			s.Log.Warn("写入列级血缘失败", "model", n.Model, "err", err)
		}
	}
}

// tableToModel 建立物理表名到模型名的映射，含 schema 限定与裸表名两种写法。
func (s *Service) tableToModel(ctx context.Context, workspaceID string) map[string]string {
	out := map[string]string{}
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return out
	}
	for _, m := range models {
		if m.Spec == nil {
			continue
		}
		tbl := strings.ToLower(m.Spec.TableName())
		schema := strings.ToLower(m.Spec.Schema)
		if schema != "" {
			out[schema+"."+tbl] = m.Name
		}
		// 裸表名只在没有歧义时登记
		if _, exists := out[tbl]; exists {
			out[tbl] = ""
		} else {
			out[tbl] = m.Name
		}
	}
	return out
}

func normalizeTable(alias map[string]string, table string) string {
	t := strings.ToLower(strings.ReplaceAll(table, `"`, ""))
	if m, ok := alias[t]; ok && m != "" {
		return m
	}
	// ods.order 这种 schema.table 可能带引号残留
	if i := strings.LastIndex(t, "."); i >= 0 {
		if m, ok := alias[t[i+1:]]; ok && m != "" {
			return m
		}
	}
	return t
}

// Impact 返回某列变更影响的下游列。
func (s *Service) Impact(ctx context.Context, workspaceID, table, column string, depth int) (map[string]any, error) {
	edges, err := s.Repo.ImpactAnalysis(ctx, workspaceID, table, column, depth)
	if err != nil {
		return nil, err
	}
	tables := map[string]bool{}
	cols := map[string]bool{}
	for _, e := range edges {
		tables[e.DownstreamTable] = true
		cols[e.DownstreamTable+"."+e.DownstreamColumn] = true
	}
	tl := make([]string, 0, len(tables))
	for t := range tables {
		tl = append(tl, t)
	}
	cl := make([]string, 0, len(cols))
	for c := range cols {
		cl = append(cl, c)
	}
	return map[string]any{
		"origin":  table + "." + column,
		"edges":   edges,
		"tables":  tl,
		"columns": cl,
		"count":   len(cl),
	}, nil
}

// ---------- 脱敏 ----------

// Mask 按策略脱敏单个值。策略为 none 时原样返回。
// 做的是「展示层脱敏」：值已经被读出，只是不返回给前端明文。
func Mask(maskType string, v any) any {
	if v == nil {
		return nil
	}
	str := fmt.Sprint(v)
	switch maskType {
	case "", "none":
		return v
	case "full":
		return "***"
	case "partial":
		r := []rune(str)
		if len(r) <= 2 {
			return "***"
		}
		keep := len(r) / 3
		if keep < 1 {
			keep = 1
		}
		return string(r[:keep]) + strings.Repeat("*", len(r)-keep*2) + string(r[len(r)-keep:])
	case "hash":
		// 只暴露前 8 位，保证可关联但不可逆
		sum := sha1Hex(str)
		return "sha1:" + sum[:8]
	case "email":
		at := strings.Index(str, "@")
		if at <= 0 {
			return "***"
		}
		name := []rune(str[:at])
		if len(name) <= 2 {
			return "*@" + str[at+1:]
		}
		return string(name[:2]) + "***" + str[at:]
	case "phone":
		if len(str) < 7 {
			return "***"
		}
		return str[:3] + "****" + str[len(str)-4:]
	}
	return v
}

// MaskRows 对结果集按列策略脱敏。policies 以 "table.column" 或 "column" 为键。
func MaskRows(columns []string, rows [][]any, policies map[string]string) {
	if len(policies) == 0 {
		return
	}
	idx := make([]string, len(columns))
	for i, c := range columns {
		idx[i] = strings.ToLower(c)
	}
	for i := range rows {
		for j, col := range idx {
			m, ok := policies[col]
			if !ok || m == "" || m == "none" {
				continue
			}
			if j < len(rows[i]) {
				rows[i][j] = Mask(m, rows[i][j])
			}
		}
	}
}

// ColumnMasks 把策略列表转成「列名 → 脱敏方式」。表名限定优先。
func ColumnMasks(policies []model.ColumnPolicy, table string) map[string]string {
	out := map[string]string{}
	tl := strings.ToLower(table)
	for _, p := range policies {
		if table != "" && strings.ToLower(p.TableName) != tl {
			continue
		}
		out[strings.ToLower(p.ColumnName)] = p.MaskType
		out[strings.ToLower(p.TableName)+"."+strings.ToLower(p.ColumnName)] = p.MaskType
	}
	return out
}

// ApplyRowFilters 把命中的行级策略拼成 AND 条件，替换 ${user}/${role} 占位符。
func ApplyRowFilters(filters []string, user, role string) string {
	var parts []string
	for _, f := range filters {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		f = strings.ReplaceAll(f, "${user}", quoteLiteral(user))
		f = strings.ReplaceAll(f, "${role}", quoteLiteral(role))
		parts = append(parts, "("+f+")")
	}
	return strings.Join(parts, " AND ")
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sha1Hex(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// PreviewTable 读取某张表的若干行，并应用行级过滤与列级脱敏。
// 这是「治理真正生效」的关键路径：管理台看到的和导出看到的都必须经过它。
func (s *Service) PreviewTable(ctx context.Context, workspaceID, datasource, schema, table, user, role string, limit int) (map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ds, err := s.DatasourceSpec(ctx, workspaceID, datasource)
	if err != nil {
		return nil, err
	}
	c, err := s.Pool.Acquire(ctx, ds)
	if err != nil {
		return nil, err
	}
	d := c.Dialect()
	full := d.QuoteIdent(table)
	if schema != "" {
		full = d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
	}

	where := ""
	if role != "" {
		filters, ferr := s.Repo.RowFiltersFor(ctx, workspaceID, table, role)
		if ferr == nil && len(filters) > 0 {
			if cond := ApplyRowFilters(filters, user, role); cond != "" {
				where = " WHERE " + cond
			}
		}
	}
	sqlText := "SELECT * FROM " + full + where + " " + d.LimitOffset(int64(limit), 0)
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "预览查询失败").WithMeta("sql", sqlText)
	}
	defer rows.Close()

	meta := rows.Columns()
	cols := make([]string, len(meta))
	for i, m := range meta {
		cols[i] = m.Name
	}
	data := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		data = append(data, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	policies, _ := s.Repo.ListColumnPolicies(ctx, workspaceID, table)
	masks := ColumnMasks(policies, table)
	MaskRows(cols, data, masks)

	// 把实际生效的策略回给前端，避免「以为脱敏了其实没有」。
	applied := map[string]string{}
	for _, c := range cols {
		if m, ok := masks[strings.ToLower(c)]; ok && m != "" && m != "none" {
			applied[c] = m
		}
	}
	return map[string]any{
		"columns": cols,
		"rows":    data,
		"masks":   applied,
		"filter":  where,
		"sql":     sqlText,
	}, nil
}

// ApplyRowFilters 读取某表的行级策略并渲染成条件列表，供指标/数据集查询注入。
func (s *Service) ApplyRowFilters(ctx context.Context, workspaceID, table, user, role string) ([]string, error) {
	if role == "" {
		return nil, nil
	}
	filters, err := s.Repo.RowFiltersFor(ctx, workspaceID, table, role)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, "("+ApplyRowFilters([]string{f}, user, role)+")")
	}
	return out, nil
}

// connectorFor 按数据源名取连接器，供语义层查询复用连接池。
func (s *Service) connectorFor(ctx context.Context, workspaceID, name string) (connector.Connector, error) {
	spec, err := s.DatasourceSpec(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	return s.Pool.Acquire(ctx, spec)
}

// MaskRows 按列策略脱敏并返回实际生效的列映射。
func (s *Service) MaskRows(ctx context.Context, workspaceID, table string, columns []string, rows [][]any) ([][]any, map[string]string) {
	policies, err := s.Repo.ListColumnPolicies(ctx, workspaceID, table)
	if err != nil || len(policies) == 0 {
		return rows, map[string]string{}
	}
	masks := ColumnMasks(policies, table)
	MaskRows(columns, rows, masks)
	applied := map[string]string{}
	for _, c := range columns {
		if m, ok := masks[strings.ToLower(c)]; ok && m != "" && m != "none" {
			applied[c] = m
		}
	}
	return rows, applied
}
