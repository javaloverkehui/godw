package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/quality"
)

// Profile 对一张表做一次轻量画像：行数、每列的空值率、去重数、极值，
// 并据此推荐质量规则。目的是让用户不用凭空想规则，而是「先看数据再定规则」。
//
// 只做全表聚合，不抽样：数据量在千万级以内可接受；更大的表建议先按分区筛。
func (s *Service) Profile(ctx context.Context, workspaceID, datasource, schema, table string, sampleRows int) (map[string]any, error) {
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

	// 1. 列清单：优先用已采集的元数据，避免再查 information_schema。
	datasets, err := s.Repo.ListDatasets(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	var cols []model.ColumnSpec
	for _, dt := range datasets {
		if strings.EqualFold(dt.Name, table) && (schema == "" || strings.EqualFold(dt.Schema, schema)) {
			cols = dt.Columns
			break
		}
	}
	if len(cols) == 0 {
		return nil, errs.New(errs.CodeNotFound, "未采集到该表的列信息，请先执行元数据采集")
	}

	// 2. 一次查询拿到行数与各列统计，减少往返。
	var selects []string
	selects = append(selects, "COUNT(*) AS total_rows")
	for i, col := range cols {
		q := d.QuoteIdent(col.Name)
		selects = append(selects,
			fmt.Sprintf("COUNT(*) FILTER (WHERE %s IS NULL) AS n%d", q, i),
			fmt.Sprintf("COUNT(DISTINCT %s) AS d%d", q, i))
		// 只对数值与日期列取极值，字符串极值意义不大且代价高
		if isOrdered(col.Type) {
			selects = append(selects,
				fmt.Sprintf("MIN(%s)::text AS mn%d", q, i),
				fmt.Sprintf("MAX(%s)::text AS mx%d", q, i))
		}
	}
	sqlText := "SELECT " + strings.Join(selects, ", ") + " FROM " + full
	if sampleRows > 0 {
		sqlText = "SELECT " + strings.Join(selects, ", ") + " FROM (SELECT * FROM " + full + " " + d.LimitOffset(int64(sampleRows), 0) + ") s"
	}
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "画像查询失败").WithMeta("sql", sqlText)
	}
	if !rows.Next() {
		rows.Close()
		return nil, errs.New(errs.CodeExecutionFailed, "画像查询无返回")
	}
	// 结果按列名取值，避免位置错乱
	meta := rows.Columns()
	vals := make([]any, len(meta))
	ptrs := make([]any, len(meta))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	byName := map[string]any{}
	for i, m := range meta {
		byName[m.Name] = vals[i]
	}

	total := toInt64(byName["total_rows"])
	profiles := make([]map[string]any, 0, len(cols))
	var rules []model.QualityRule

	for i, col := range cols {
		nulls := toInt64(byName[fmt.Sprintf("n%d", i)])
		distinct := toInt64(byName[fmt.Sprintf("d%d", i)])
		p := map[string]any{
			"column": col.Name, "type": col.Type,
			"nulls": nulls, "distinct": distinct,
		}
		if total > 0 {
			p["nullRate"] = float64(nulls) / float64(total)
		}
		if v, ok := byName[fmt.Sprintf("mn%d", i)]; ok && v != nil {
			p["min"] = fmt.Sprint(v)
		}
		if v, ok := byName[fmt.Sprintf("mx%d", i)]; ok && v != nil {
			p["max"] = fmt.Sprint(v)
		}
		profiles = append(profiles, p)

		// 3. 推荐规则：空值率为 0 → not_null；完全唯一 → unique；枚举型 → accepted_values 候选
		if total > 0 && nulls == 0 {
			rules = append(rules, model.QualityRule{
				Name: col.Name + "_not_null", Kind: "not_null", Column: col.Name, Severity: "error",
			})
		}
		if total > 0 && distinct == total && distinct > 1 {
			rules = append(rules, model.QualityRule{
				Name: col.Name + "_unique", Kind: "unique", Column: col.Name, Severity: "warn",
			})
		}
		if total > 0 && distinct > 0 && distinct <= 12 && !isOrdered(col.Type) {
			rules = append(rules, model.QualityRule{
				Name: col.Name + "_accepted", Kind: "accepted_values", Column: col.Name, Severity: "warn",
				Config: map[string]any{"values": []any{}},
			})
		}
	}

	// 4. 推荐规则必须能编译通过，否则用户在 UI 里点了会报错。
	valid := make([]model.QualityRule, 0, len(rules))
	for _, r := range rules {
		if _, err := quality.BuildWith(r, schema, table, d); err == nil {
			valid = append(valid, r)
		}
	}

	return map[string]any{
		"table": table, "schema": schema, "rows": total,
		"columns": profiles, "recommended": valid, "sql": sqlText,
	}, nil
}

func isOrdered(t string) bool {
	switch strings.ToLower(t) {
	case "int", "bigint", "smallint", "decimal", "numeric", "float", "double", "real",
		"date", "timestamp", "datetime", "timestamptz", "time":
		return true
	}
	return false
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case []byte:
		var n int64
		fmt.Sscan(string(x), &n)
		return n
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	}
	return 0
}
