package semantic

import (
	"context"
	"fmt"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// Card 是看板上的一个格子。
//
// 两种形态二选一：
//   - Metric：引用一个指标，由 RunCards 按 day 粒度聚合，渲染成折线；
//   - Chart：引用一张已保存的 BI 图表（charts/{name}），按图表自身的 type 渲染。
//
// 之所以保留两条路径而不是把指标都改写成图表：指标带单位、口径说明和维度切片语义，
// 图表带编码槽；合并会丢掉其中一方的表达力，而看板恰恰是两者的共同容器。
type Card struct {
	Title     string `json:"title" yaml:"title"`
	Metric    string `json:"metric,omitempty" yaml:"metric,omitempty"`
	Chart     string `json:"chart,omitempty" yaml:"chart,omitempty"`
	Days      int    `json:"days" yaml:"days"`
	Dimension string `json:"dimension,omitempty" yaml:"dimension,omitempty"`
	Span      int    `json:"span,omitempty" yaml:"span,omitempty"` // 1 或 2 列
}

// Dashboard 是一组卡片的集合。
type Dashboard struct {
	ID          string `json:"id,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
	Cards       []Card `json:"cards" yaml:"cards"`
}

// ValidateDashboard 校验看板结构。指标是否存在由调用方补充检查。
func ValidateDashboard(d Dashboard) error {
	var problems []string
	if d.Name == "" {
		problems = append(problems, "name 不能为空")
	}
	if len(d.Cards) == 0 {
		problems = append(problems, "cards 不能为空")
	}
	if len(d.Cards) > 12 {
		problems = append(problems, "单页最多 12 个卡片")
	}
	for i, c := range d.Cards {
		hasMetric, hasChart := c.Metric != "", c.Chart != ""
		switch {
		case hasMetric && hasChart:
			problems = append(problems, fmt.Sprintf("cards[%d] 同时指定了 metric 与 chart，只能二选一", i))
		case !hasMetric && !hasChart:
			problems = append(problems, fmt.Sprintf("cards[%d] 必须指定 metric 或 chart", i))
		case hasMetric && c.Days <= 0:
			// 指标的窗口是卡片级参数；图表卡的窗口由看板全局筛选决定，不在此约束。
			problems = append(problems, fmt.Sprintf("cards[%d].days 必须为正", i))
		}
		if c.Span != 0 && c.Span != 1 && c.Span != 2 {
			problems = append(problems, fmt.Sprintf("cards[%d].span 只能是 1 或 2", i))
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeInvalidArgument, "看板定义不合法").WithMeta("problems", problems)
	}
	return nil
}

// CardSeries 是渲染一个卡片所需的全部数据。
// Metric 卡用 Points（时序点，前端走交互折线）；Chart 卡用 Series + ChartPoints
// （带系列名，前端按 ChartType 分派到柱/饼/指标卡/表格）。两组字段不混用。
type CardSeries struct {
	Card
	Kind        string        `json:"kind"`
	Unit        string        `json:"unit"`
	ChartType   string        `json:"chartType,omitempty"`
	Series      []ChartSeries `json:"series,omitempty"`
	ChartPoints []ChartPoint  `json:"chartPoints,omitempty"`
	Points      []Point       `json:"points"`
	Error       string        `json:"error,omitempty"`
}

// CardChart 是图表卡的可渲染数据。看板只取这三样，
// 不把整个 Chart 透出去——否则前端容易把看板做成第二个 BI 编辑器。
type CardChart struct {
	Type   string
	Series []ChartSeries
	Points []ChartPoint
}

// ChartCardResolver 解析并执行一张图表卡。opt 是看板级全局筛选，
// 由调用方决定怎么落成查询条件（是否可筛选只有服务层知道）。
type ChartCardResolver func(name string, opt RunOptions) (*CardChart, error)

// DashboardData 是看板的一次性渲染结果。
type DashboardData struct {
	Dashboard
	Cards []CardSeries `json:"cards"`
}

// RunOptions 是看板级的全局筛选，作用到每一张卡片上。
// Days>0 覆盖卡片自带的窗口（用户在页头切了 7/14/30/90 天）。
// Dim 非空时只保留该维度值的序列（等价于「只看 paid」这类切片）。
type RunOptions struct {
	Days int
	Dim  string
}

// RunCards 逐个渲染卡片。单个卡片失败不影响整页，错误挂在卡片上。
// 串行执行：卡片数量少（≤12），并发带来的连接争抢不划算。
func RunCards(ctx context.Context, resolve func(string) (Metric, model.ModelSpec, connector.Connector, error), resolveChart ChartCardResolver, d Dashboard, opt RunOptions) DashboardData {
	out := DashboardData{Dashboard: d}
	for _, c := range d.Cards {
		cs := CardSeries{Card: c}
		// 图表卡与指标卡走两条链路，但共用同一个「错误挂在卡片上」的约定。
		if c.Chart != "" {
			fillChartCard(&cs, c, resolveChart, opt)
			out.Cards = append(out.Cards, cs)
			continue
		}
		cs.Kind = "metric"
		m, spec, conn, err := resolve(c.Metric)
		if err != nil {
			cs.Error = err.Error()
			out.Cards = append(out.Cards, cs)
			continue
		}
		days := opt.Days
		if days <= 0 {
			days = c.Days
		}
		if days <= 0 {
			days = 30
		}
		q := Query{
			From:        daysAgo(days),
			To:          today(),
			Granularity: "day",
			Dimension:   c.Dimension,
			Limit:       2000,
		}
		series, err := Run(ctx, conn, m, spec, q)
		if err != nil {
			cs.Error = err.Error()
			out.Cards = append(out.Cards, cs)
			continue
		}
		cs.Unit = m.Unit
		// 回写实际生效的窗口：页面切了「7 天」而卡片仍标 30 天，会让人以为筛选没生效
		cs.Days = days
		cs.Points = filterByDim(series.Points, opt.Dim)
		out.Cards = append(out.Cards, cs)
	}
	return out
}

// fillChartCard 把图表查询结果装进卡片。
// 维度切片作用在系列上：看板选了「paid」时，图表卡只保留 paid 这条线，
// 而不是把整张图藏起来——后者会让人以为图表坏了。
func fillChartCard(cs *CardSeries, c Card, resolveChart ChartCardResolver, opt RunOptions) {
	cs.Kind = "chart"
	if resolveChart == nil {
		cs.Error = "看板未接入图表解析"
		return
	}
	cc, err := resolveChart(c.Chart, opt)
	if err != nil {
		cs.Error = err.Error()
		return
	}
	cs.ChartType = cc.Type
	cs.Series, cs.ChartPoints = cc.Series, cc.Points
	if opt.Dim != "" {
		cs.Series, cs.ChartPoints = filterChartByDim(cc.Series, cc.Points, opt.Dim)
	}
}

// filterChartByDim 只保留指定名称的系列。单系列图表（无 series 编码）的系列名是图表标题，
// 与维度值不会相等，此时返回空，由前端显示「该维度值下没有数据」——与指标卡行为一致。
func filterChartByDim(series []ChartSeries, points []ChartPoint, dim string) ([]ChartSeries, []ChartPoint) {
	if dim == "" || len(series) == 0 {
		return series, points
	}
	kept := make([]ChartSeries, 0, 1)
	for _, s := range series {
		if s.Name == dim {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	pts := make([]ChartPoint, 0, len(points))
	for _, p := range points {
		if p.Series == dim {
			pts = append(pts, p)
		}
	}
	return kept, pts
}

// filterByDim 只保留指定维度值的点；维度不匹配时返回空，由前端显示「该筛选下无数据」。
func filterByDim(points []Point, dim string) []Point {
	if dim == "" {
		return points
	}
	out := make([]Point, 0, len(points))
	for _, p := range points {
		if p.Dim == dim {
			out = append(out, p)
		}
	}
	return out
}

func today() string { return time.Now().Format("2006-01-02") }

func daysAgo(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }
