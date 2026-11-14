package service

import (
	"context"
	"fmt"
	"time"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
	"github.com/jackc/pgx/v5/pgtype"
)

// 语义层服务：数据集与图表的校验、查询与影响面。

// PublishedModels 返回工作空间内已发布模型的 spec 映射，供数据集校验引用。
func (s *Service) PublishedModels(ctx context.Context, workspaceID string) (map[string]model.ModelSpec, error) {
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]model.ModelSpec{}
	for _, m := range models {
		if m.Spec != nil && m.ActiveVersion > 0 {
			out[m.Name] = *m.Spec
		}
	}
	return out, nil
}

// ApplyDataset 校验并保存数据集。
func (s *Service) ApplyDataset(ctx context.Context, workspaceID string, d semantic.Dataset) (*semantic.Dataset, error) {
	models, err := s.PublishedModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := semantic.ValidateDataset(d, models); err != nil {
		return nil, err
	}
	return s.Repo.SaveDataset(ctx, workspaceID, d)
}

// DatasetQuery 编译并执行数据集查询，行级权限在编译前注入。
func (s *Service) DatasetQuery(ctx context.Context, workspaceID string, req semantic.QueryRequest, user, role string) (map[string]any, error) {
	d, err := s.Repo.GetDataset(ctx, workspaceID, req.Dataset)
	if err != nil {
		return nil, err
	}
	models, err := s.PublishedModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	base, ok := models[d.BaseModel]
	if !ok {
		return nil, errs.New(errs.CodeNotFound, "数据集的基础模型未发布: "+d.BaseModel)
	}

	// 行级权限：与指标查询共用同一套策略，避免两个入口口径不一致。
	if filters, ferr := s.ApplyRowFilters(ctx, workspaceID, base.TableName(), user, role); ferr == nil {
		req.RLS = filters
	}

	c, err := s.connectorFor(ctx, workspaceID, base.Datasource)
	if err != nil {
		return nil, err
	}
	built, err := semantic.BuildQuery(*d, req, models, c.Dialect())
	if err != nil {
		return nil, err
	}

	start := time.Now()
	rows, err := c.Query(ctx, built.SQL, built.Args...)
	if err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "数据集查询失败").
			WithMeta("sql", built.SQL).WithMeta("args", built.Args)
	}
	defer rows.Close()

	cols := rows.Columns()
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
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
		data = append(data, vals)
		if len(data) > 20000 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 脱敏：列策略按表名匹配。
	masked, masks := s.MaskRows(ctx, workspaceID, base.TableName(), names, data)
	return map[string]any{
		"dataset":   d.Name,
		"columns":   names,
		"rows":      masked,
		"rowCount":  len(masked),
		"sql":       built.SQL,
		"args":      built.Args,
		"masks":     masks,
		"elapsedMs": time.Since(start).Milliseconds(),
	}, nil
}

// ApplyChart 校验并保存图表。返回受影响看板，供前端提示。
func (s *Service) ApplyChart(ctx context.Context, workspaceID string, c semantic.Chart) (*semantic.Chart, []string, error) {
	var ds *semantic.Dataset
	if c.Dataset != "" {
		if d, err := s.Repo.GetDataset(ctx, workspaceID, c.Dataset); err == nil {
			ds = d
		} else {
			return nil, nil, errs.New(errs.CodeNotFound, "图表引用的数据集不存在: "+c.Dataset)
		}
	}
	if err := semantic.ValidateChart(c, ds); err != nil {
		return nil, nil, err
	}
	// 影响面：改图表前先告诉用户哪些看板会变。
	var impact []string
	if existing, err := s.Repo.GetChart(ctx, workspaceID, c.Name); err == nil {
		dashboards, derr := s.Repo.ListDashboards(ctx, workspaceID)
		if derr == nil {
			impact = existing.Impact(dashboards)
		}
	}
	saved, err := s.Repo.SaveChart(ctx, workspaceID, c)
	if err != nil {
		return nil, nil, err
	}
	return saved, impact, nil
}

// ChartQuery 按图表配置执行查询。BI 页与看板共用同一条链路。
func (s *Service) ChartQuery(ctx context.Context, workspaceID, name, user, role string) (*semantic.ChartResult, error) {
	return s.ChartQueryWith(ctx, workspaceID, name, user, role, nil)
}

// ChartQueryWith 在图表自带筛选之外追加 extra —— 看板的全局时间窗口落在这里，
// 但没有默认放开：调用方必须先确认对应维度 filterable。
func (s *Service) ChartQueryWith(ctx context.Context, workspaceID, name, user, role string, extra []semantic.Filter) (*semantic.ChartResult, error) {
	c, err := s.Repo.GetChart(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	req := c.ToQuery()
	req.Filters = append(req.Filters, extra...)
	res, err := s.DatasetQuery(ctx, workspaceID, req, user, role)
	if err != nil {
		return nil, err
	}
	rows, _ := res["rows"].([][]any)
	cols, _ := res["columns"].([]string)
	idx := map[string]int{}
	for i, n := range cols {
		idx[n] = i
	}

	// 系列顺序必须稳定：前端按索引分配颜色，用 map 遍历会让同一条线每次刷新换色。
	byName := map[string]*semantic.ChartSeries{}
	var order []string
	points := []semantic.ChartPoint{}
	for _, row := range rows {
		var x string
		if i, ok := idx[c.Encoding.X]; ok && i < len(row) {
			x = formatAxisValue(row[i])
		}
		y := 0.0
		if i, ok := idx[c.Encoding.Y]; ok && i < len(row) {
			y = toFloatSafe(row[i])
		}
		key := ""
		if c.Encoding.Series != "" {
			if i, ok := idx[c.Encoding.Series]; ok && i < len(row) {
				key = fmt.Sprint(row[i])
			}
		}
		if key == "" {
			key = c.Encoding.Y
		}
		if byName[key] == nil {
			byName[key] = &semantic.ChartSeries{Name: key}
			order = append(order, key)
		}
		byName[key].Data = append(byName[key].Data, y)
		points = append(points, semantic.ChartPoint{X: x, Y: y, Series: key})
	}
	series := make([]semantic.ChartSeries, 0, len(order))
	for _, k := range order {
		series = append(series, *byName[k])
	}
	// 单系列时用图表标题代替度量名，图例更干净。
	// points.series 必须同步改名，否则看板按维度值切片时对不上系列名。
	if len(series) == 1 {
		series[0].Name = firstNonEmptyStr(c.Title, series[0].Name)
		for i := range points {
			points[i].Series = series[0].Name
		}
	}

	sqlText, _ := res["sql"].(string)
	var elapsed int64
	switch v := res["elapsedMs"].(type) {
	case int64:
		elapsed = v
	case float64:
		elapsed = int64(v)
	case int:
		elapsed = int64(v)
	}
	return &semantic.ChartResult{
		Chart:     *c,
		Columns:   cols,
		Series:    series,
		Points:    points,
		SQL:       sqlText,
		ElapsedMs: elapsed,
	}, nil
}

// DatasetUsage 返回数据集的引用情况，删除前提示。
func (s *Service) DatasetUsage(ctx context.Context, workspaceID, name string) (map[string]any, error) {
	n, err := s.Repo.CountChartsUsingDataset(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"dataset": name, "charts": n}, nil
}

// toFloatSafe 把查询结果里的任意数值类型归一成 float64。
//
// 踩过的坑：pgx 默认把 NUMERIC 扫成 pgtype.Numeric 结构体，
// fmt.Sprint 得到的是 "{1 -3 false ...}"，Sscan 解不出来就静默变成 0，
// 图表于是全平在 0 上——看起来「查询成功」，实际数据全丢。
// 所以这里显式处理 Numeric/[]byte/string，并保留兜底解析。
func toFloatSafe(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int64:
		return float64(t)
	case int32:
		return float64(t)
	case int:
		return float64(t)
	case pgtype.Numeric:
		f, err := t.Float64Value()
		if err == nil && f.Valid {
			return f.Float64
		}
		return 0
	case []byte:
		var f float64
		_, _ = fmt.Sscan(string(t), &f)
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(t, &f)
		return f
	case nil:
		return 0
	}
	var f float64
	_, _ = fmt.Sscan(fmt.Sprint(v), &f)
	return f
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ConnectorFor 暴露连接器获取，供只编译不执行的接口复用。
func (s *Service) ConnectorFor(ctx context.Context, workspaceID, name string) (interface {
	Dialect() dialect.Dialect
}, error) {
	return s.connectorFor(ctx, workspaceID, name)
}

// formatAxisValue 把坐标轴上的值转成人类可读的短串。
//
// pgx 会把 date/timestamp 扫成 time.Time，直接 fmt.Sprint 会得到
// "2026-10-11 00:00:00 +0000 UTC" 这种又长又带时区的串，横轴根本放不下。
// 这里按「是否整点零点」决定要不要带时分。
func formatAxisValue(v any) string {
	switch t := v.(type) {
	case time.Time:
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format("2006-01-02 15:04")
	}
	return fmt.Sprint(v)
}
