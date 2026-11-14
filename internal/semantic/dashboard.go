package semantic

import (
	"context"
	"fmt"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// Card 是看板上的一个格子：引用一个指标 + 展示参数。
type Card struct {
	Title     string `json:"title" yaml:"title"`
	Metric    string `json:"metric" yaml:"metric"`
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
		if c.Metric == "" {
			problems = append(problems, fmt.Sprintf("cards[%d].metric 不能为空", i))
		}
		if c.Days <= 0 {
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
type CardSeries struct {
	Card
	Unit   string  `json:"unit"`
	Points []Point `json:"points"`
	Error  string  `json:"error,omitempty"`
}

// DashboardData 是看板的一次性渲染结果。
type DashboardData struct {
	Dashboard
	Cards []CardSeries `json:"cards"`
}

// RunCards 逐个渲染卡片。单个卡片失败不影响整页，错误挂在卡片上。
// 串行执行：卡片数量少（≤12），并发带来的连接争抢不划算。
func RunCards(ctx context.Context, resolve func(string) (Metric, model.ModelSpec, connector.Connector, error), d Dashboard) DashboardData {
	out := DashboardData{Dashboard: d}
	for _, c := range d.Cards {
		cs := CardSeries{Card: c}
		m, spec, conn, err := resolve(c.Metric)
		if err != nil {
			cs.Error = err.Error()
			out.Cards = append(out.Cards, cs)
			continue
		}
		days := c.Days
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
		cs.Points = series.Points
		out.Cards = append(out.Cards, cs)
	}
	return out
}

func today() string { return time.Now().Format("2006-01-02") }

func daysAgo(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }
