package semantic

import (
	"context"
	"errors"
	"testing"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
)

// 卡片的两种形态必须互斥：同时给 metric 和 chart 时不能「随便挑一个渲染」，
// 否则看板定义里会出现一份永远不生效的字段。
func TestValidateDashboardCardKind(t *testing.T) {
	cases := []struct {
		name    string
		card    Card
		wantErr bool
	}{
		{"仅指标", Card{Title: "GMV", Metric: "gmv", Days: 30}, false},
		{"仅图表", Card{Title: "店铺分布", Chart: "shop_pie"}, false},
		{"同时指定", Card{Metric: "gmv", Chart: "shop_pie"}, true},
		{"都没指定", Card{Title: "空卡"}, true},
		{"指标缺窗口", Card{Metric: "gmv"}, true},
		{"span 越界", Card{Metric: "gmv", Days: 7, Span: 3}, true},
	}
	for _, tc := range cases {
		err := ValidateDashboard(Dashboard{Name: "d", Cards: []Card{tc.card}})
		if tc.wantErr && err == nil {
			t.Errorf("%s: 期望报错，实际通过", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: 期望通过，实际 %v", tc.name, err)
		}
	}
}

// 图表卡必须走 resolveChart，指标卡必须走 resolveMetric，两条链路不能串。
func TestRunCardsRoutesChartCards(t *testing.T) {
	d := Dashboard{Name: "d", Cards: []Card{
		{Title: "指标", Metric: "gmv", Days: 30},
		{Title: "图表", Chart: "shop_pie"},
	}}
	metricCalls, chartCalls := 0, 0

	resolveMetric := func(string) (Metric, model.ModelSpec, connector.Connector, error) {
		metricCalls++
		// 返回错误即可跳过真实查询，RunCards 的约定是「失败挂在卡片上」。
		return Metric{}, model.ModelSpec{}, nil, errors.New("测试不连库")
	}
	resolveChart := func(name string, _ RunOptions) (*CardChart, error) {
		chartCalls++
		if name != "shop_pie" {
			t.Fatalf("图表名被改写: %s", name)
		}
		return &CardChart{
			Type:   ChartPie,
			Series: []ChartSeries{{Name: "华东", Data: []float64{3}}},
			Points: []ChartPoint{{X: "华东", Y: 3, Series: "华东"}},
		}, nil
	}

	data := RunCards(context.Background(), resolveMetric, resolveChart, d, RunOptions{})
	if metricCalls != 1 || chartCalls != 1 {
		t.Fatalf("调用次数不对: metric=%d chart=%d", metricCalls, chartCalls)
	}
	if len(data.Cards) != 2 {
		t.Fatalf("卡片数不对: %d", len(data.Cards))
	}
	if data.Cards[0].Kind != "metric" {
		t.Errorf("第一张应为 metric，实际 %q", data.Cards[0].Kind)
	}
	got := data.Cards[1]
	if got.Kind != "chart" || got.ChartType != ChartPie {
		t.Errorf("图表卡字段不对: kind=%q type=%q", got.Kind, got.ChartType)
	}
	if len(got.Series) != 1 || got.Series[0].Name != "华东" {
		t.Errorf("系列没有透传: %+v", got.Series)
	}
}

// 维度切片作用在系列上：选「paid」时保留 paid，而不是把整张卡清空。
func TestRunCardsChartDimFilter(t *testing.T) {
	d := Dashboard{Name: "d", Cards: []Card{{Title: "订单", Chart: "order_trend"}}}
	resolveChart := func(string, RunOptions) (*CardChart, error) {
		return &CardChart{
			Type: ChartLine,
			Series: []ChartSeries{
				{Name: "paid", Data: []float64{2, 3}},
				{Name: "created", Data: []float64{5, 1}},
			},
			Points: []ChartPoint{
				{X: "11-01", Y: 2, Series: "paid"},
				{X: "11-01", Y: 5, Series: "created"},
				{X: "11-02", Y: 3, Series: "paid"},
			},
		}, nil
	}
	resolveMetric := func(string) (Metric, model.ModelSpec, connector.Connector, error) {
		return Metric{}, model.ModelSpec{}, nil, errors.New("不应被调用")
	}

	data := RunCards(context.Background(), resolveMetric, resolveChart, d, RunOptions{Dim: "paid"})
	card := data.Cards[0]
	if len(card.Series) != 1 || card.Series[0].Name != "paid" {
		t.Fatalf("维度切片没有收敛到单系列: %+v", card.Series)
	}
	if len(card.ChartPoints) != 2 {
		t.Errorf("数据点应为 2 个，实际 %d", len(card.ChartPoints))
	}
	for _, p := range card.ChartPoints {
		if p.Series != "paid" {
			t.Errorf("残留了其它系列的点: %+v", p)
		}
	}
}

// 某张图表卡查询失败不能拖垮整页，也不能把错误吞掉。
func TestRunCardsChartErrorIsolated(t *testing.T) {
	d := Dashboard{Name: "d", Cards: []Card{
		{Title: "坏图", Chart: "broken"},
		{Title: "好图", Chart: "ok"},
	}}
	resolveChart := func(name string, _ RunOptions) (*CardChart, error) {
		if name == "broken" {
			return nil, errors.New("数据集不存在")
		}
		return &CardChart{Type: ChartBar, Series: []ChartSeries{{Name: "a", Data: []float64{1}}}}, nil
	}
	resolveMetric := func(string) (Metric, model.ModelSpec, connector.Connector, error) {
		return Metric{}, model.ModelSpec{}, nil, errors.New("不应被调用")
	}

	data := RunCards(context.Background(), resolveMetric, resolveChart, d, RunOptions{})
	if data.Cards[0].Error != "数据集不存在" {
		t.Errorf("失败卡片没有带上原因: %q", data.Cards[0].Error)
	}
	if data.Cards[1].Error != "" || len(data.Cards[1].Series) != 1 {
		t.Errorf("健康卡片被连带影响: %+v", data.Cards[1])
	}
}

// 没接图表解析器时给一句可执行的报错，而不是空卡片。
func TestRunCardsChartWithoutResolver(t *testing.T) {
	d := Dashboard{Name: "d", Cards: []Card{{Chart: "c"}}}
	resolveMetric := func(string) (Metric, model.ModelSpec, connector.Connector, error) {
		return Metric{}, model.ModelSpec{}, nil, errors.New("不应被调用")
	}
	data := RunCards(context.Background(), resolveMetric, nil, d, RunOptions{})
	if data.Cards[0].Error == "" {
		t.Error("缺少解析器时应报错")
	}
}
