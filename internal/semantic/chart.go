package semantic

import (
	"fmt"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
)

// T0707 图表配置模型。
//
// 图表配置必须可存、可迁移、可复用 —— 这意味着它是纯数据，不含任何代码或函数。
// 编码（encoding）是图表的核心：它把「字段」映射到「视觉通道」，
// 不同图表类型对通道数量有硬约束，校验必须在这里做，而不是等到渲染时报错。

// Chart 是一张图表的完整配置。
type Chart struct {
	ID          string   `json:"id,omitempty"`
	WorkspaceID string   `json:"workspaceId,omitempty"`
	Name        string   `json:"name" yaml:"name"`
	Title       string   `json:"title,omitempty" yaml:"title,omitempty"`
	Type        string   `json:"type" yaml:"type"`
	Dataset     string   `json:"dataset" yaml:"dataset"`
	Dimensions  []string `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Measures    []string `json:"measures,omitempty" yaml:"measures,omitempty"`
	Encoding    Encoding `json:"encoding" yaml:"encoding"`
	Style       Style    `json:"style,omitempty" yaml:"style,omitempty"`
	Filters     []Filter `json:"filters,omitempty" yaml:"filters,omitempty"`
	Version     int      `json:"version,omitempty"`
}

// Encoding 把字段映射到视觉通道。
type Encoding struct {
	X      string `json:"x,omitempty" yaml:"x,omitempty"`
	Y      string `json:"y,omitempty" yaml:"y,omitempty"`
	Series string `json:"series,omitempty" yaml:"series,omitempty"`
	Size   string `json:"size,omitempty" yaml:"size,omitempty"`
	Color  string `json:"color,omitempty" yaml:"color,omitempty"`
	Label  string `json:"label,omitempty" yaml:"label,omitempty"`
}

// Style 是样式，全部为可序列化的基础类型。
type Style struct {
	Palette      string `json:"palette,omitempty" yaml:"palette,omitempty"`
	ShowLegend   *bool  `json:"showLegend,omitempty" yaml:"showLegend,omitempty"`
	ShowGrid     *bool  `json:"showGrid,omitempty" yaml:"showGrid,omitempty"`
	Stack        bool   `json:"stack,omitempty" yaml:"stack,omitempty"`
	Smooth       bool   `json:"smooth,omitempty" yaml:"smooth,omitempty"`
	NumberFormat string `json:"numberFormat,omitempty" yaml:"numberFormat,omitempty"`
}

// ChartType 是图表类型。
const (
	ChartLine     = "line"
	ChartBar      = "bar"
	ChartStackBar = "stack_bar"
	ChartPie      = "pie"
	ChartDonut    = "donut"
	ChartKPI      = "kpi"
	ChartTable    = "table"
	ChartScatter  = "scatter"
	ChartMap      = "map"
)

// ChartTypes 返回支持的图表类型。
func ChartTypes() []string {
	return []string{ChartLine, ChartBar, ChartStackBar, ChartPie, ChartDonut,
		ChartKPI, ChartTable, ChartScatter, ChartMap}
}

// typeSpec 描述每种图表对通道的约束。
type typeSpec struct {
	minDims     int
	maxDims     int
	minMeasures int
	maxMeasures int
	// needX 表示必须有 x 通道（折线/柱状）。
	needX bool
	// needY 表示必须有 y 通道。
	needY bool
	// allowSeries 是否支持多系列。
	allowSeries bool
	desc        string
}

var chartSpecs = map[string]typeSpec{
	ChartLine:     {minDims: 1, maxDims: 1, minMeasures: 1, maxMeasures: 1, needX: true, needY: true, allowSeries: true, desc: "折线：1 个时间维度 + 1 个度量"},
	ChartBar:      {minDims: 1, maxDims: 2, minMeasures: 1, maxMeasures: 1, needX: true, needY: true, allowSeries: true, desc: "柱状：1–2 维度 + 1 度量"},
	ChartStackBar: {minDims: 2, maxDims: 2, minMeasures: 1, maxMeasures: 1, needX: true, needY: true, allowSeries: true, desc: "堆叠柱：2 维度 + 1 度量"},
	ChartPie:      {minDims: 1, maxDims: 1, minMeasures: 1, maxMeasures: 1, needX: false, needY: false, desc: "饼图：1 维度 + 1 度量"},
	ChartDonut:    {minDims: 1, maxDims: 1, minMeasures: 1, maxMeasures: 1, desc: "环形图：1 维度 + 1 度量"},
	ChartKPI:      {minDims: 0, maxDims: 0, minMeasures: 1, maxMeasures: 3, desc: "指标卡：1–3 个度量，无维度"},
	ChartTable:    {minDims: 0, maxDims: 6, minMeasures: 1, maxMeasures: 8, desc: "表格：最多 6 维度 + 8 度量"},
	ChartScatter:  {minDims: 0, maxDims: 2, minMeasures: 2, maxMeasures: 2, needX: true, needY: true, desc: "散点：2 度量（x/y），可选 1 维度着色"},
	ChartMap:      {minDims: 1, maxDims: 1, minMeasures: 1, maxMeasures: 1, desc: "地图：1 个地理维度 + 1 度量"},
}

// ValidateChart 校验图表配置。dataset 用于确认字段存在。
func ValidateChart(c Chart, d *Dataset) error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if !identRe.MatchString(c.Name) {
		add("name 必须是标识符，实际 %q", c.Name)
	}
	spec, ok := chartSpecs[c.Type]
	if !ok {
		add("不支持的图表类型 %q，可选：%s", c.Type, strings.Join(ChartTypes(), ", "))
		return errs.New(errs.CodeInvalidArgument, "图表配置不合法").WithMeta("problems", problems)
	}
	if c.Dataset == "" {
		add("dataset 不能为空")
	}

	if len(c.Dimensions) < spec.minDims || len(c.Dimensions) > spec.maxDims {
		add("%s 需要 %d–%d 个维度（当前 %d）。%s",
			c.Type, spec.minDims, spec.maxDims, len(c.Dimensions), spec.desc)
	}
	if len(c.Measures) < spec.minMeasures || len(c.Measures) > spec.maxMeasures {
		add("%s 需要 %d–%d 个度量（当前 %d）",
			c.Type, spec.minMeasures, spec.maxMeasures, len(c.Measures))
	}

	// 编码槽引用必须落在已声明的字段内，且不能一个字段占两个槽。
	all := append(append([]string{}, c.Dimensions...), c.Measures...)
	known := map[string]bool{}
	for _, f := range all {
		known[f] = true
	}
	slots := []struct{ name, val string }{
		{"x", c.Encoding.X}, {"y", c.Encoding.Y}, {"series", c.Encoding.Series},
		{"size", c.Encoding.Size}, {"color", c.Encoding.Color}, {"label", c.Encoding.Label},
	}
	used := map[string]string{}
	for _, s := range slots {
		if s.val == "" {
			continue
		}
		if !known[s.val] {
			add("encoding.%s 引用了未选中的字段 %q", s.name, s.val)
		}
		if prev, dup := used[s.val]; dup {
			add("字段 %q 同时用于 encoding.%s 与 encoding.%s", s.val, prev, s.name)
		}
		used[s.val] = s.name
	}
	if spec.needX && c.Encoding.X == "" {
		add("%s 必须指定 encoding.x", c.Type)
	}
	if spec.needY && c.Encoding.Y == "" {
		add("%s 必须指定 encoding.y", c.Type)
	}
	if !spec.allowSeries && c.Encoding.Series != "" {
		add("%s 不支持多系列（encoding.series）", c.Type)
	}
	// 饼图/环形图的扇区必须有唯一维度，否则会叠成一堆。
	if (c.Type == ChartPie || c.Type == ChartDonut) && len(c.Dimensions) != 1 {
		add("饼图/环形图只能有 1 个维度，当前 %d 个", len(c.Dimensions))
	}
	// 堆叠图必须指定系列，否则「堆叠」没有意义。
	if c.Type == ChartStackBar && c.Encoding.Series == "" {
		add("堆叠柱必须指定 encoding.series")
	}

	// 字段必须存在于数据集。
	if d != nil {
		dims := map[string]bool{}
		for _, dim := range d.Dimensions {
			dims[dim.Name] = true
		}
		meas := map[string]bool{}
		for _, m := range d.Measures {
			meas[m.Name] = true
		}
		for _, f := range c.Dimensions {
			if !dims[f] {
				add("维度 %q 不在数据集 %s 中", f, d.Name)
			}
		}
		for _, f := range c.Measures {
			if !meas[f] {
				add("度量 %q 不在数据集 %s 中", f, d.Name)
			}
		}
	}

	if len(problems) > 0 {
		return errs.New(errs.CodeInvalidArgument, "图表配置不合法").WithMeta("problems", problems)
	}
	return nil
}

// ToQuery 把图表配置翻译成查询请求。
func (c Chart) ToQuery() QueryRequest {
	req := QueryRequest{
		Dataset:    c.Dataset,
		Dimensions: c.Dimensions,
		Measures:   c.Measures,
		Filters:    c.Filters,
	}
	// 折线图默认按 x 升序，柱状图按 y 降序（业务更关心 Top N）。
	switch c.Type {
	case ChartLine:
		req.OrderBy = c.Encoding.X
		req.Desc = false
	case ChartBar, ChartStackBar, ChartPie, ChartDonut, ChartMap:
		if len(c.Measures) > 0 {
			req.OrderBy = c.Measures[0]
			req.Desc = true
		}
	}
	if c.Type == ChartPie || c.Type == ChartDonut {
		req.Limit = 20 // 饼图扇区过多不可读
	}
	return req
}

// Impact 返回引用了该图表的看板名，用于「改图表前提示影响面」。
func (c Chart) Impact(dashboards []Dashboard) []string {
	out := []string{}
	for _, d := range dashboards {
		for _, card := range d.Cards {
			// 看板卡片既可能引用指标，也可能引用图表；这里按名字匹配。
			if card.Metric == c.Name || card.Metric == c.ID {
				out = append(out, d.Name)
				break
			}
		}
	}
	return out
}

// ChartSchema 返回图表配置的 JSON Schema 片段，供前端动态表单渲染。
func ChartSchema() map[string]any {
	types := ChartTypes()
	return map[string]any{
		"type":     "object",
		"required": []string{"name", "type", "dataset"},
		"properties": map[string]any{
			"name":       map[string]any{"type": "string", "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"},
			"title":      map[string]any{"type": "string"},
			"type":       map[string]any{"type": "string", "enum": types},
			"dataset":    map[string]any{"type": "string"},
			"dimensions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"measures":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"encoding": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x": map[string]any{"type": "string"}, "y": map[string]any{"type": "string"},
					"series": map[string]any{"type": "string"}, "size": map[string]any{"type": "string"},
					"color": map[string]any{"type": "string"}, "label": map[string]any{"type": "string"},
				},
			},
		},
	}
}
