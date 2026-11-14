package semantic_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

type fakeConn struct {
	rows connector.Rows
	err  error
}

func (c *fakeConn) Type() string                       { return "fake" }
func (c *fakeConn) Capabilities() connector.Capability { return connector.Capability{} }
func (c *fakeConn) Dialect() dialect.Dialect           { return dialect.Postgres{} }
func (c *fakeConn) Test(context.Context) error         { return nil }
func (c *fakeConn) ListSchemas(context.Context) ([]string, error) {
	return nil, nil
}
func (c *fakeConn) ListTables(context.Context, string) ([]connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) DescribeTable(context.Context, string, string) (*connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) Query(context.Context, string, ...any) (connector.Rows, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.rows, nil
}
func (c *fakeConn) Exec(context.Context, string, ...any) (connector.Result, error) {
	return connector.Result{}, nil
}
func (c *fakeConn) Stream(context.Context, string, int, func(connector.Rows) error) error {
	return nil
}
func (c *fakeConn) Close() error { return nil }

func metric() semantic.Metric {
	return semantic.Metric{
		Name: "sales", ModelName: "dws_sales", Agg: "sum", Expr: "amount",
		TimeColumn: "dt", Dimensions: []string{"region"}, Unit: "元",
	}
}

func salesSpec() model.ModelSpec {
	return model.ModelSpec{Name: "dws_sales", Schema: "dws", Layer: "dws"}
}

// 指标定义不是写 SQL 的地方：任何非标识符表达式都必须拒绝，
// 否则语义层的「统一口径」会被一行自定义表达式绕开。
func TestValidateMetric(t *testing.T) {
	if err := semantic.Validate(metric()); err != nil {
		t.Fatalf("合法指标不应报错: %v", err)
	}
	cases := []struct {
		name string
		m    semantic.Metric
		want string
	}{
		{"name 非法", semantic.Metric{Name: "1bad", ModelName: "m", Agg: "sum", Expr: "a"}, "name 必须是标识符"},
		{"model 为空", semantic.Metric{Name: "ok", Agg: "sum", Expr: "a"}, "model 不能为空"},
		{"agg 非法", semantic.Metric{Name: "ok", ModelName: "m", Agg: "median", Expr: "a"}, "agg 只支持"},
		{"expr 非法", semantic.Metric{Name: "ok", ModelName: "m", Agg: "sum", Expr: "a + b"}, "expr 只能是被聚合的列名"},
		{"维度非法", semantic.Metric{Name: "ok", ModelName: "m", Agg: "sum", Expr: "a",
			Dimensions: []string{"a b"}}, "维度 \"a b\" 非法"},
		{"时间列非法", semantic.Metric{Name: "ok", ModelName: "m", Agg: "sum", Expr: "a",
			TimeColumn: "dt asc"}, "time_column"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := semantic.Validate(tc.m)
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
	// count 不要求 expr：COUNT(*) 是合法指标。
	if err := semantic.Validate(semantic.Metric{Name: "cnt", ModelName: "m", Agg: "count"}); err != nil {
		t.Fatalf("count 允许空 expr: %v", err)
	}
}

// 指标 SQL 拼装：时间桶、聚合、维度、过滤与上限，任何一处错位都会让报表口径漂移。
func TestBuildMetric(t *testing.T) {
	spec := salesSpec()

	t.Run("缺省", func(t *testing.T) {
		got, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`SELECT (DATE_TRUNC('day', "dt"))::date AS ts, SUM("amount") AS value`,
			`FROM "dws"."dws_sales"`,
			"GROUP BY 1",
			"ORDER BY 1",
			"LIMIT 1000",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("SQL 缺少 %q:\n%s", want, got)
			}
		}
	})
	t.Run("粒度与维度", func(t *testing.T) {
		got, err := semantic.Build(metric(), spec, dialect.Postgres{},
			semantic.Query{Granularity: "month", Dimension: "region"})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`DATE_TRUNC('month', "dt")`, `"region" AS dim`, "GROUP BY 1, 2"} {
			if !strings.Contains(got, want) {
				t.Fatalf("SQL 缺少 %q:\n%s", want, got)
			}
		}
	})
	t.Run("未声明维度", func(t *testing.T) {
		_, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{Dimension: "city"})
		if !errs.Is(err, errs.CodeInvalidArgument) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("非法粒度", func(t *testing.T) {
		if _, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{Granularity: "hour"}); err == nil {
			t.Fatal("hour 粒度应报错")
		}
	})
	t.Run("时间范围与权限条件", func(t *testing.T) {
		got, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{
			From: "2024-01-01", To: "2024-02-01",
			ExtraWhere: []string{"  ", "region = '华东'"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"dt" >= '2024-01-01' AND "dt" < '2024-02-01' AND (region = '华东')`) {
			t.Fatalf("过滤条件错误:\n%s", got)
		}
	})
	t.Run("非日期值要转义", func(t *testing.T) {
		got, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{From: "2024-01-01' OR 1=1"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `'2024-01-01'' OR 1=1'`) {
			t.Fatalf("字面量未转义:\n%s", got)
		}
	})
	t.Run("limit 边界", func(t *testing.T) {
		for in, want := range map[int]string{0: "LIMIT 1000", -1: "LIMIT 1000", 6000: "LIMIT 1000", 50: "LIMIT 50"} {
			got, err := semantic.Build(metric(), spec, dialect.Postgres{}, semantic.Query{Limit: in})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, want) {
				t.Fatalf("limit=%d 时缺少 %q:\n%s", in, want, got)
			}
		}
	})
	t.Run("缺省时间列 dt", func(t *testing.T) {
		m := metric()
		m.TimeColumn = ""
		got, err := semantic.Build(m, spec, dialect.Postgres{}, semantic.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"dt"`) {
			t.Fatalf("缺省时间列错误:\n%s", got)
		}
	})
	t.Run("无 schema", func(t *testing.T) {
		got, err := semantic.Build(metric(), model.ModelSpec{Name: "t"}, dialect.Postgres{}, semantic.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `FROM "t"`) {
			t.Fatalf("表引用错误:\n%s", got)
		}
	})
	t.Run("非法指标不产出 SQL", func(t *testing.T) {
		bad := metric()
		bad.Agg = "median"
		if got, err := semantic.Build(bad, spec, dialect.Postgres{}, semantic.Query{}); err == nil || got != "" {
			t.Fatalf("非法指标应报错: %q %v", got, err)
		}
	})
}

// 聚合映射：count 必须 COUNT(*)，count_distinct 必须去重，其余保留列名。
func TestBuildMetricAggs(t *testing.T) {
	spec := salesSpec()
	cases := []struct {
		agg  string
		expr string
		want string
	}{
		{"count", "", "COUNT(*)"},
		{"count", "id", "COUNT(*)"},
		{"count_distinct", "user_id", `COUNT(DISTINCT "user_id")`},
		{"avg", "amount", `AVG("amount")`},
		{"min", "amount", `MIN("amount")`},
		{"max", "amount", `MAX("amount")`},
	}
	for _, tc := range cases {
		m := metric()
		m.Agg, m.Expr = tc.agg, tc.expr
		got, err := semantic.Build(m, spec, dialect.Postgres{}, semantic.Query{})
		if err != nil {
			t.Fatalf("%s: %v", tc.agg, err)
		}
		if !strings.Contains(got, tc.want+" AS value") {
			t.Fatalf("%s(%s) 渲染错误:\n%s", tc.agg, tc.expr, got)
		}
	}
}

// 时间桶必须按方言翻译，写错就是「查不出数据」而不是报错。
func TestBuildMetricBuckets(t *testing.T) {
	spec := salesSpec()
	cases := []struct {
		name string
		d    dialect.Dialect
		gran string
		want string
	}{
		{"ch 日", dialect.ClickHouse{}, "", "toDate(`dt`)"},
		{"ch 周", dialect.ClickHouse{}, "week", "toStartOfWeek(`dt`)"},
		{"ch 月", dialect.ClickHouse{}, "month", "toStartOfMonth(`dt`)"},
		{"mysql 日", dialect.MySQL{}, "", "DATE(`dt`)"},
		{"mysql 周", dialect.MySQL{}, "week", "DATE_SUB(DATE(`dt`)"},
		{"mysql 月", dialect.MySQL{}, "month", "DATE_FORMAT(`dt`, '%Y-%m-01')"},
		{"ansi 日", dialect.ANSI{}, "", `(DATE_TRUNC('day', "dt"))::date`},
		{"ansi 周", dialect.ANSI{}, "week", `DATE_TRUNC('week', "dt")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := semantic.Build(metric(), spec, tc.d, semantic.Query{Granularity: tc.gran})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("缺少 %q:\n%s", tc.want, got)
			}
		})
	}
}

// 执行结果归一化：时间格式与维度切片错位会让折线图画到别的日期上。
func TestRunMetric(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("无维度", func(t *testing.T) {
		c := &fakeConn{rows: connector.NewMemRows(
			[]connector.ColumnMeta{{Name: "ts"}, {Name: "value"}},
			[][]any{{day, 1.5}, {day.AddDate(0, 0, 1), 2.5}})}
		got, err := semantic.Run(ctx, c, metric(), salesSpec(), semantic.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Metric != "sales" || got.Unit != "元" || len(got.Points) != 2 {
			t.Fatalf("结果错误: %+v", got)
		}
		if got.Points[0].Ts != "2024-01-02" || got.Points[0].Value != 1.5 || got.Points[0].Dim != "" {
			t.Fatalf("点错误: %+v", got.Points[0])
		}
		if !strings.Contains(got.SQL, "SELECT") {
			t.Fatalf("SQL 未回填: %+v", got)
		}
	})
	t.Run("带维度", func(t *testing.T) {
		c := &fakeConn{rows: connector.NewMemRows(
			[]connector.ColumnMeta{{Name: "ts"}, {Name: "dim"}, {Name: "value"}},
			[][]any{{day, "east", 3}})}
		got, err := semantic.Run(ctx, c, metric(), salesSpec(), semantic.Query{Dimension: "region"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Points) != 1 || got.Points[0].Dim != "east" || got.Points[0].Value != 3 {
			t.Fatalf("点错误: %+v", got.Points)
		}
	})
	t.Run("构建失败", func(t *testing.T) {
		bad := metric()
		bad.Agg = "nope"
		if _, err := semantic.Run(ctx, &fakeConn{}, bad, salesSpec(), semantic.Query{}); err == nil {
			t.Fatal("非法指标应报错")
		}
	})
	t.Run("查询失败", func(t *testing.T) {
		_, err := semantic.Run(ctx, &fakeConn{err: errors.New("down")}, metric(), salesSpec(), semantic.Query{})
		if !errs.Is(err, errs.CodeExecutionFailed) {
			t.Fatalf("错误码错误: %v", err)
		}
		if err.(*errs.AppError).Meta["sql"] == nil {
			t.Fatalf("错误应带 SQL: %v", err)
		}
	})
	t.Run("扫描失败", func(t *testing.T) {
		rows := connector.NewMemRows([]connector.ColumnMeta{{Name: "ts"}}, [][]any{{day}})
		if _, err := semantic.Run(ctx, &fakeConn{rows: rows}, metric(), salesSpec(), semantic.Query{}); err == nil {
			t.Fatal("扫描失败应报错")
		}
	})
	t.Run("维度扫描失败", func(t *testing.T) {
		rows := connector.NewMemRows([]connector.ColumnMeta{{Name: "ts"}, {Name: "dim"}}, [][]any{{day, "east"}})
		if _, err := semantic.Run(ctx, &fakeConn{rows: rows}, metric(), salesSpec(), semantic.Query{Dimension: "region"}); err == nil {
			t.Fatal("维度扫描失败应报错")
		}
	})
}
