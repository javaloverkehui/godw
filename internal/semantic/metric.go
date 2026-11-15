package semantic

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// Metric 是语义层指标：一次定义，所有看板复用同一口径。
type Metric struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspaceId,omitempty"`
	Name        string   `json:"name" yaml:"name"`
	DisplayName string   `json:"displayName" yaml:"displayName"`
	ModelName   string   `json:"model" yaml:"model"`
	Agg         string   `json:"agg" yaml:"agg"`
	Expr        string   `json:"expr" yaml:"expr"`
	TimeColumn  string   `json:"timeColumn" yaml:"timeColumn"`
	Dimensions  []string `json:"dimensions" yaml:"dimensions"`
	Unit        string   `json:"unit" yaml:"unit"`
	Description string   `json:"description" yaml:"description"`
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var exprRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate 拒绝一切非标识符的表达式：指标不是让用户写 SQL 的地方。
func Validate(m Metric) error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if !identRe.MatchString(m.Name) {
		add("name 必须是标识符，实际 %q", m.Name)
	}
	if m.ModelName == "" {
		add("model 不能为空")
	}
	switch m.Agg {
	case "sum", "avg", "count", "count_distinct", "min", "max":
	default:
		add("agg 只支持 sum/avg/count/count_distinct/min/max，实际 %q", m.Agg)
	}
	if m.Agg != "count" && !exprRe.MatchString(m.Expr) {
		add("expr 只能是被聚合的列名，实际 %q", m.Expr)
	}
	for _, d := range m.Dimensions {
		if !identRe.MatchString(d) {
			add("维度 %q 非法", d)
		}
	}
	if m.TimeColumn != "" && !identRe.MatchString(m.TimeColumn) {
		add("time_column %q 非法", m.TimeColumn)
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeInvalidArgument, "指标定义不合法").WithMeta("problems", problems)
	}
	return nil
}

// Query 是查询参数。时间范围闭开区间 [From, To)。
type Query struct {
	From        string `json:"from" yaml:"from"`
	To          string `json:"to" yaml:"to"`
	Granularity string `json:"granularity" yaml:"granularity"` // day | week | month
	Dimension   string `json:"dimension" yaml:"dimension"`
	Limit       int    `json:"limit" yaml:"limit"`
	// ExtraWhere 是服务端注入的行级权限条件，来自 row_policy。
	// 不接受外部直接传入：只能由 Service 在调用前填充，避免绕过。
	ExtraWhere []string `json:"-" yaml:"-"`
}

// Point 是序列上的一个点。
type Point struct {
	Ts    string  `json:"ts"`
	Dim   string  `json:"dim,omitempty"`
	Value float64 `json:"value"`
}

type Series struct {
	Metric string  `json:"metric"`
	Unit   string  `json:"unit"`
	Query  Query   `json:"query"`
	Points []Point `json:"points"`
	SQL    string  `json:"sql"`
	// Cached 为真表示命中缓存，前端可据此提示。
	Cached bool `json:"cached,omitempty"`
}

// Build 编译指标查询。所有标识符都经过白名单校验与方言引用，
// 时间范围走参数化字面量，不做字符串拼接。
func Build(m Metric, spec model.ModelSpec, d dialect.Dialect, q Query) (string, error) {
	if err := Validate(m); err != nil {
		return "", err
	}
	timeCol := m.TimeColumn
	if timeCol == "" {
		timeCol = "dt"
	}
	gran := q.Granularity
	if gran == "" {
		gran = "day"
	}
	if gran != "day" && gran != "week" && gran != "month" {
		return "", errs.New(errs.CodeInvalidArgument, "granularity 只支持 day/week/month")
	}

	table := d.QuoteIdent(spec.TableName())
	if spec.Schema != "" {
		table = d.QuoteIdent(spec.Schema) + "." + table
	}

	tsExpr := bucket(timeCol, gran, d)
	aggExpr := aggregate(m, d)

	selects := []string{tsExpr + " AS ts"}
	groups := []string{"1"}
	dim := ""
	if q.Dimension != "" {
		found := false
		for _, dd := range m.Dimensions {
			if dd == q.Dimension {
				found = true
				break
			}
		}
		if !found {
			return "", errs.New(errs.CodeInvalidArgument, "维度未在指标中声明: "+q.Dimension).
				WithMeta("allowed", m.Dimensions)
		}
		dim = q.Dimension
		selects = append(selects, d.QuoteIdent(dim)+" AS dim")
		groups = append(groups, "2")
	}
	selects = append(selects, aggExpr+" AS value")

	where := []string{}
	if q.From != "" {
		where = append(where, fmt.Sprintf("%s >= %s", d.QuoteIdent(timeCol), lit(q.From)))
	}
	if q.To != "" {
		where = append(where, fmt.Sprintf("%s < %s", d.QuoteIdent(timeCol), lit(q.To)))
	}
	for _, extra := range q.ExtraWhere {
		if strings.TrimSpace(extra) != "" {
			where = append(where, "("+extra+")")
		}
	}

	limit := q.Limit
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}

	var b strings.Builder
	b.WriteString("SELECT " + strings.Join(selects, ", ") + "\nFROM " + table)
	if len(where) > 0 {
		b.WriteString("\nWHERE " + strings.Join(where, " AND "))
	}
	b.WriteString("\nGROUP BY " + strings.Join(groups, ", "))
	b.WriteString("\nORDER BY 1")
	b.WriteString("\n" + d.LimitOffset(int64(limit), 0))
	return b.String(), nil
}

func aggregate(m Metric, d dialect.Dialect) string {
	switch m.Agg {
	case "count":
		return "COUNT(*)"
	case "count_distinct":
		return "COUNT(DISTINCT " + d.QuoteIdent(m.Expr) + ")"
	default:
		return strings.ToUpper(m.Agg) + "(" + d.QuoteIdent(m.Expr) + ")"
	}
}

func bucket(col, gran string, d dialect.Dialect) string {
	switch d.Name() {
	case "clickhouse":
		switch gran {
		case "week":
			return "toStartOfWeek(" + d.QuoteIdent(col) + ")"
		case "month":
			return "toStartOfMonth(" + d.QuoteIdent(col) + ")"
		default:
			return "toDate(" + d.QuoteIdent(col) + ")"
		}
	case "mysql":
		switch gran {
		case "month":
			return "DATE_FORMAT(" + d.QuoteIdent(col) + ", '%Y-%m-01')"
		case "week":
			return "DATE_SUB(DATE(" + d.QuoteIdent(col) + "), INTERVAL WEEKDAY(" + d.QuoteIdent(col) + ") DAY)"
		default:
			return "DATE(" + d.QuoteIdent(col) + ")"
		}
	default:
		return "(" + d.DateTrunc(gran, d.QuoteIdent(col)) + ")::date"
	}
}

// Run 执行查询并把结果归一化为序列。
func Run(ctx context.Context, c connector.Connector, m Metric, spec model.ModelSpec, q Query) (*Series, error) {
	sqlText, err := Build(m, spec, c.Dialect(), q)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "指标查询失败").WithMeta("sql", sqlText)
	}
	defer rows.Close()

	// Points 必须初始化成空切片而不是 nil：Go 的 nil slice 序列化成 JSON null，
	// 前端 series.points.length 会直接抛「Cannot read properties of null」。
	// 「区间内没有数据」是正常结果，不该表现成页面崩掉。
	out := &Series{Metric: m.Name, Unit: m.Unit, Query: q, SQL: sqlText, Points: []Point{}}
	hasDim := q.Dimension != ""
	for rows.Next() {
		var ts time.Time
		var val float64
		var dim string
		if hasDim {
			if err := rows.Scan(&ts, &dim, &val); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&ts, &val); err != nil {
				return nil, err
			}
		}
		out.Points = append(out.Points, Point{Ts: ts.Format("2006-01-02"), Dim: dim, Value: val})
	}
	return out, rows.Err()
}

func lit(s string) string {
	if _, err := time.Parse("2006-01-02", s); err == nil {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
