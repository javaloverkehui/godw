package semantic_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

func chartDataset() *semantic.Dataset {
	return &semantic.Dataset{
		Name:       "ds_sales",
		Dimensions: []semantic.Dimension{{Name: "region"}, {Name: "created_at"}},
		Measures:   []semantic.Measure{{Name: "amount"}, {Name: "cnt"}},
	}
}

func validChart() semantic.Chart {
	return semantic.Chart{
		Name: "c_sales", Type: semantic.ChartLine, Dataset: "ds_sales",
		Dimensions: []string{"created_at"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "created_at", Y: "amount"},
	}
}

// 图表编码是视觉通道到字段的映射，通道数量错配必须在保存时拦住，
// 而不是在渲染时才报错。
func TestValidateChart(t *testing.T) {
	if err := semantic.ValidateChart(validChart(), chartDataset()); err != nil {
		t.Fatalf("合法图表不应报错: %v", err)
	}

	mutate := func(f func(*semantic.Chart)) semantic.Chart {
		c := validChart()
		f(&c)
		return c
	}
	cases := []struct {
		name string
		c    semantic.Chart
		want string
	}{
		{"name 非法", mutate(func(c *semantic.Chart) { c.Name = "1c" }), "name 必须是标识符"},
		{"类型未知", mutate(func(c *semantic.Chart) { c.Type = "radar" }), "不支持的图表类型"},
		{"dataset 为空", mutate(func(c *semantic.Chart) { c.Dataset = "" }), "dataset 不能为空"},
		{"维度数量不符", mutate(func(c *semantic.Chart) { c.Type = semantic.ChartKPI }), "需要 0–0 个维度"},
		{"度量数量不符", mutate(func(c *semantic.Chart) {
			c.Type = semantic.ChartKPI
			c.Dimensions = nil
			c.Measures = nil
		}), "需要 1–3 个度量"},
		{"编码引用未选字段", mutate(func(c *semantic.Chart) { c.Encoding.X = "ghost" }), "引用了未选中的字段"},
		{"字段占用两个通道", mutate(func(c *semantic.Chart) { c.Encoding.Y = "created_at" }), "同时用于"},
		{"折线缺 x", mutate(func(c *semantic.Chart) { c.Encoding.X = "" }), "必须指定 encoding.x"},
		{"折线缺 y", mutate(func(c *semantic.Chart) { c.Encoding.Y = "" }), "必须指定 encoding.y"},
		{"饼图不支持多系列", mutate(func(c *semantic.Chart) {
			c.Type = semantic.ChartPie
			c.Dimensions = []string{"region"}
			c.Encoding = semantic.Encoding{X: "region", Y: "amount", Series: "region"}
		}), "不支持多系列"},
		{"饼图维度过多", mutate(func(c *semantic.Chart) {
			c.Type = semantic.ChartPie
			c.Dimensions = []string{"region", "created_at"}
			c.Encoding = semantic.Encoding{}
		}), "只能有 1 个维度"},
		{"堆叠柱缺系列", mutate(func(c *semantic.Chart) {
			c.Type = semantic.ChartStackBar
			c.Dimensions = []string{"region", "created_at"}
			c.Encoding = semantic.Encoding{X: "region", Y: "amount"}
		}), "必须指定 encoding.series"},
		{"维度不在数据集", mutate(func(c *semantic.Chart) { c.Dimensions = []string{"ghost"} }), "不在数据集"},
		{"度量不在数据集", mutate(func(c *semantic.Chart) { c.Measures = []string{"ghost"} }), "不在数据集"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := semantic.ValidateChart(tc.c, chartDataset())
			if !errs.Is(err, errs.CodeInvalidArgument) {
				t.Fatalf("错误码错误: %v", err)
			}
			ae := err.(*errs.AppError)
			problems, _ := ae.Meta["problems"].([]string)
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("问题缺少 %q: %v", tc.want, problems)
			}
		})
	}

	// 不传数据集时跳过字段存在性校验（设计器草稿阶段）。
	if err := semantic.ValidateChart(validChart(), nil); err != nil {
		t.Fatalf("无数据集时不应报字段错误: %v", err)
	}
}

// 图表到查询请求的默认排序：折线按 x 升序，柱/饼按度量降序取 Top N。
func TestChartToQuery(t *testing.T) {
	line := validChart()
	got := line.ToQuery()
	if got.OrderBy != "created_at" || got.Desc {
		t.Fatalf("折线排序错误: %+v", got)
	}

	bar := validChart()
	bar.Type = semantic.ChartBar
	got = bar.ToQuery()
	if got.OrderBy != "amount" || !got.Desc {
		t.Fatalf("柱状排序错误: %+v", got)
	}

	pie := validChart()
	pie.Type = semantic.ChartPie
	got = pie.ToQuery()
	if got.OrderBy != "amount" || !got.Desc || got.Limit != 20 {
		t.Fatalf("饼图配置错误: %+v", got)
	}

	// 无度量时（KPI 草稿）不能设置排序。
	kpi := validChart()
	kpi.Type = semantic.ChartKPI
	kpi.Measures = nil
	if got := kpi.ToQuery(); got.OrderBy != "" {
		t.Fatalf("无度量不应排序: %+v", got)
	}
}

// 影响面：改图表前要知道哪些看板在用，按名称或 ID 命中都算。
func TestChartImpact(t *testing.T) {
	c := semantic.Chart{ID: "c1", Name: "c_sales"}
	dashboards := []semantic.Dashboard{
		{Name: "首页", Cards: []semantic.Card{{Metric: "c_sales"}}},
		{Name: "销售", Cards: []semantic.Card{{Metric: "c1"}}},
		{Name: "无关", Cards: []semantic.Card{{Metric: "other"}}},
		{Name: "多卡", Cards: []semantic.Card{{Metric: "c1"}, {Metric: "c1"}}},
	}
	got := c.Impact(dashboards)
	if strings.Join(got, ",") != "首页,销售,多卡" {
		t.Fatalf("影响面错误: %v", got)
	}
}

func TestChartMeta(t *testing.T) {
	types := semantic.ChartTypes()
	if len(types) != 9 {
		t.Fatalf("图表类型数量错误: %v", types)
	}
	schema := semantic.ChartSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok || props["name"] == nil || props["encoding"] == nil {
		t.Fatalf("Schema 结构错误: %+v", schema)
	}
	if reqs, ok := schema["required"].([]string); !ok || len(reqs) != 3 {
		t.Fatalf("Schema required 错误: %+v", schema)
	}
}

func dashboards() semantic.Dashboard {
	return semantic.Dashboard{
		Name: "首页",
		Cards: []semantic.Card{
			{Metric: "sales", Days: 7, Span: 1},
			{Chart: "c_sales", Span: 2},
		},
	}
}

// 看板校验：卡片必须二选一（指标/图表），窗口与栅格宽度都有硬约束。
func TestValidateDashboard(t *testing.T) {
	if err := semantic.ValidateDashboard(dashboards()); err != nil {
		t.Fatalf("合法看板不应报错: %v", err)
	}
	cases := []struct {
		name string
		d    semantic.Dashboard
		want string
	}{
		{"name 为空", semantic.Dashboard{Cards: []semantic.Card{{Metric: "m", Days: 1}}}, "name 不能为空"},
		{"cards 为空", semantic.Dashboard{Name: "d"}, "cards 不能为空"},
		{"卡片过多", func() semantic.Dashboard {
			d := semantic.Dashboard{Name: "d"}
			for i := 0; i < 13; i++ {
				d.Cards = append(d.Cards, semantic.Card{Metric: "m", Days: 1})
			}
			return d
		}(), "最多 12 个卡片"},
		{"同时指定指标与图表", semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "m", Chart: "c", Days: 1}}}, "只能二选一"},
		{"两者都缺", semantic.Dashboard{Name: "d", Cards: []semantic.Card{{}}}, "必须指定 metric 或 chart"},
		{"指标窗口非正", semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "m"}}}, "days 必须为正"},
		{"span 非法", semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "m", Days: 1, Span: 3}}}, "只能是 1 或 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := semantic.ValidateDashboard(tc.d)
			if !errs.Is(err, errs.CodeInvalidArgument) {
				t.Fatalf("错误码错误: %v", err)
			}
			ae := err.(*errs.AppError)
			problems, _ := ae.Meta["problems"].([]string)
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("问题缺少 %q: %v", tc.want, problems)
			}
		})
	}
	// 图表卡的窗口由看板全局筛选决定，days=0 合法。
	if err := semantic.ValidateDashboard(semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}); err != nil {
		t.Fatalf("图表卡不应要求 days: %v", err)
	}
}

func dayRows(dims ...string) connector.Rows {
	day := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	data := make([][]any, 0, len(dims))
	for i, d := range dims {
		data = append(data, []any{day.AddDate(0, 0, i), d, float64(i + 1)})
	}
	return connector.NewMemRows(
		[]connector.ColumnMeta{{Name: "ts"}, {Name: "dim"}, {Name: "value"}}, data)
}

// 看板渲染：单卡失败不能拖垮整页，错误必须挂在卡片上；
// 全局筛选要同时作用于指标卡与图表卡。
func TestRunCards(t *testing.T) {
	ctx := context.Background()
	resolve := func(string) (semantic.Metric, model.ModelSpec, connector.Connector, error) {
		return metric(), salesSpec(), &fakeConn{rows: dayRows("east", "west")}, nil
	}

	t.Run("指标卡使用卡片窗口", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "sales", Days: 7}}}
		// Run 需要按维度扫描，卡片必须声明 dimension。
		d.Cards[0].Dimension = "region"
		out := semantic.RunCards(ctx, resolve, nil, d, semantic.RunOptions{})
		cs := out.Cards[0]
		if cs.Kind != "metric" || cs.Days != 7 || cs.Unit != "元" || len(cs.Points) != 2 {
			t.Fatalf("指标卡错误: %+v", cs)
		}
	})
	t.Run("全局窗口覆盖卡片", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "sales", Days: 7, Dimension: "region"}}}
		out := semantic.RunCards(ctx, resolve, nil, d, semantic.RunOptions{Days: 14})
		if out.Cards[0].Days != 14 {
			t.Fatalf("窗口未覆盖: %+v", out.Cards[0])
		}
	})
	t.Run("缺省窗口 30 天", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "sales", Dimension: "region"}}}
		out := semantic.RunCards(ctx, resolve, nil, d, semantic.RunOptions{})
		if out.Cards[0].Days != 30 {
			t.Fatalf("缺省窗口错误: %+v", out.Cards[0])
		}
	})
	t.Run("维度切片", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "sales", Days: 7, Dimension: "region"}}}
		out := semantic.RunCards(ctx, resolve, nil, d, semantic.RunOptions{Dim: "east"})
		pts := out.Cards[0].Points
		if len(pts) != 1 || pts[0].Dim != "east" {
			t.Fatalf("切片错误: %+v", pts)
		}
	})
	t.Run("解析指标失败", func(t *testing.T) {
		badResolve := func(string) (semantic.Metric, model.ModelSpec, connector.Connector, error) {
			return semantic.Metric{}, model.ModelSpec{}, nil, errors.New("指标不存在")
		}
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "nope", Days: 7}}}
		out := semantic.RunCards(ctx, badResolve, nil, d, semantic.RunOptions{})
		if !strings.Contains(out.Cards[0].Error, "指标不存在") || out.Cards[0].Kind != "metric" {
			t.Fatalf("错误未挂到卡片: %+v", out.Cards[0])
		}
	})
	t.Run("查询失败", func(t *testing.T) {
		failResolve := func(string) (semantic.Metric, model.ModelSpec, connector.Connector, error) {
			return metric(), salesSpec(), &fakeConn{err: errors.New("db down")}, nil
		}
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Metric: "sales", Days: 7}}}
		out := semantic.RunCards(ctx, failResolve, nil, d, semantic.RunOptions{})
		if !strings.Contains(out.Cards[0].Error, "db down") {
			t.Fatalf("查询错误未挂到卡片: %+v", out.Cards[0])
		}
	})

	chartCard := func(name string, opt semantic.RunOptions) (*semantic.CardChart, error) {
		return &semantic.CardChart{
			Type: semantic.ChartLine,
			Series: []semantic.ChartSeries{
				{Name: "east", Data: []float64{1}}, {Name: "west", Data: []float64{2}},
			},
			Points: []semantic.ChartPoint{
				{X: "2024-01-01", Y: 1, Series: "east"},
				{X: "2024-01-01", Y: 2, Series: "west"},
			},
		}, nil
	}
	t.Run("图表卡未接入解析", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, nil, d, semantic.RunOptions{})
		if out.Cards[0].Kind != "chart" || !strings.Contains(out.Cards[0].Error, "未接入") {
			t.Fatalf("错误未挂到卡片: %+v", out.Cards[0])
		}
	})
	t.Run("图表解析失败", func(t *testing.T) {
		fail := func(string, semantic.RunOptions) (*semantic.CardChart, error) {
			return nil, errors.New("图表不存在")
		}
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, fail, d, semantic.RunOptions{})
		if !strings.Contains(out.Cards[0].Error, "图表不存在") {
			t.Fatalf("错误未挂到卡片: %+v", out.Cards[0])
		}
	})
	t.Run("图表卡成功", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, chartCard, d, semantic.RunOptions{})
		cs := out.Cards[0]
		if cs.Kind != "chart" || cs.ChartType != semantic.ChartLine || len(cs.Series) != 2 || len(cs.ChartPoints) != 2 {
			t.Fatalf("图表卡错误: %+v", cs)
		}
	})
	t.Run("图表卡按维度切片", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, chartCard, d, semantic.RunOptions{Dim: "east"})
		cs := out.Cards[0]
		if len(cs.Series) != 1 || cs.Series[0].Name != "east" || len(cs.ChartPoints) != 1 {
			t.Fatalf("切片错误: %+v", cs)
		}
	})
	t.Run("图表卡切片无匹配", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, chartCard, d, semantic.RunOptions{Dim: "north"})
		if len(out.Cards[0].Series) != 0 || len(out.Cards[0].ChartPoints) != 0 {
			t.Fatalf("无匹配应清空: %+v", out.Cards[0])
		}
	})
	t.Run("图表无系列时切片原样返回", func(t *testing.T) {
		empty := func(string, semantic.RunOptions) (*semantic.CardChart, error) {
			return &semantic.CardChart{Type: semantic.ChartKPI}, nil
		}
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Chart: "c"}}}
		out := semantic.RunCards(ctx, resolve, empty, d, semantic.RunOptions{Dim: "east"})
		if len(out.Cards[0].Series) != 0 || out.Cards[0].ChartType != semantic.ChartKPI {
			t.Fatalf("单系列图表不应被切片清空: %+v", out.Cards[0])
		}
	})
	t.Run("混合卡片", func(t *testing.T) {
		d := semantic.Dashboard{Name: "d", Cards: []semantic.Card{
			{Metric: "sales", Days: 7, Dimension: "region"},
			{Chart: "c"},
		}}
		out := semantic.RunCards(ctx, resolve, chartCard, d, semantic.RunOptions{})
		if len(out.Cards) != 2 || out.Cards[0].Kind != "metric" || out.Cards[1].Kind != "chart" {
			t.Fatalf("混合卡片错误: %+v", out.Cards)
		}
	})
}
