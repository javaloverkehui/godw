package quality_test

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
	"github.com/godw/godw/internal/quality"
)

// fakeConn 只实现质量规则用到的 Query/Dialect。
// 用内存结果集而不是起真库：规则的判定逻辑在 Go 里，SQL 只是取值手段。
type fakeConn struct {
	rows connector.Rows
	err  error
	d    dialect.Dialect
}

func (c *fakeConn) Type() string                                  { return "fake" }
func (c *fakeConn) Capabilities() connector.Capability            { return connector.Capability{} }
func (c *fakeConn) Dialect() dialect.Dialect                      { return c.d }
func (c *fakeConn) Test(context.Context) error                    { return nil }
func (c *fakeConn) ListSchemas(context.Context) ([]string, error) { return nil, nil }
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

// errRows 用来制造「没有结果行」与「迭代出错」两种异常路径。
type errRows struct {
	next bool
	err  error
}

func (r *errRows) Columns() []connector.ColumnMeta { return nil }
func (r *errRows) Next() bool {
	if r.next {
		return false
	}
	r.next = true
	return true
}
func (r *errRows) Scan(dest ...any) error { return errors.New("scan failed") }
func (r *errRows) Err() error             { return r.err }
func (r *errRows) Close() error           { return nil }

// emptyRows 永远没有结果行且没有错误。
type emptyRows struct{ err error }

func (r *emptyRows) Columns() []connector.ColumnMeta { return nil }
func (r *emptyRows) Next() bool                      { return false }
func (r *emptyRows) Scan(...any) error               { return nil }
func (r *emptyRows) Err() error                      { return r.err }
func (r *emptyRows) Close() error                    { return nil }

func rule(kind string, cfg map[string]any) model.QualityRule {
	return model.QualityRule{Name: "r_" + kind, Kind: kind, Column: "amount", Severity: "error", Config: cfg}
}

// 规则编译的缺陷面：SQL 拼错一个引号就会静默给错结果。
// 这里守住每类规则的关键片段与错误分支。
func TestBuild(t *testing.T) {
	cases := []struct {
		name string
		rule model.QualityRule
		want []string
	}{
		{"not_null", rule(quality.KindNotNull, nil), []string{`WHERE "amount" IS NULL`, "0 AS expected"}},
		{"unique", rule(quality.KindUnique, nil), []string{"GROUP BY", "HAVING COUNT(*) > 1"}},
		{"accepted_values", rule(quality.KindAcceptedValues, map[string]any{
			"values": []any{1, "a'b"}}), []string{"NOT IN ('1','a''b')"}},
		{"range 双端", rule(quality.KindRange, map[string]any{"min": 0, "max": 100}),
			[]string{`"amount" < 0 OR "amount" > 100`}},
		{"range 只有下限", rule(quality.KindRange, map[string]any{"min": int64(0)}),
			[]string{`"amount" < 0`, "observed"}},
		{"range 只有上限", rule(quality.KindRange, map[string]any{"max": "100"}),
			[]string{`"amount" > 100`}},
		{"range 浮点下限", rule(quality.KindRange, map[string]any{"min": 1.5}),
			[]string{`"amount" < 1.5`}},
		{"row_count", rule(quality.KindRowCount, map[string]any{"min": 10}),
			[]string{"10 AS expected", "行数 ≥ 10"}},
		{"custom_sql", rule(quality.KindCustomSQL, map[string]any{
			"sql": "SELECT COUNT(*) AS observed, 0 AS expected FROM t WHERE x > 0", "expect": "0"}),
			[]string{"SELECT COUNT(*)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := quality.Build(tc.rule, "dwd", "dwd_order", dialect.Postgres{})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.SQL+out.Expect, w) {
					t.Fatalf("缺少 %q:\nSQL=%s\nExpect=%s", w, out.SQL, out.Expect)
				}
			}
			if out.Name != tc.rule.Name || out.Severity != "error" {
				t.Fatalf("元信息错误: %+v", out)
			}
		})
	}

	bad := []struct {
		name string
		rule model.QualityRule
	}{
		{"accepted_values 缺 values", rule(quality.KindAcceptedValues, nil)},
		{"accepted_values 空数组", rule(quality.KindAcceptedValues, map[string]any{"values": []any{}})},
		{"accepted_values 类型不对", rule(quality.KindAcceptedValues, map[string]any{"values": "x"})},
		{"range 无边界", rule(quality.KindRange, nil)},
		{"custom_sql 为空", rule(quality.KindCustomSQL, map[string]any{"sql": "  "})},
		{"custom_sql 写操作", rule(quality.KindCustomSQL, map[string]any{"sql": "DELETE FROM t WHERE id = 1"})},
		{"未知类型", rule("unknown", nil)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := quality.Build(tc.rule, "", "t", dialect.Postgres{}); !errs.Is(err, errs.CodeInvalidArgument) {
				t.Fatalf("应返回参数错误: %v", err)
			}
		})
	}

	// 无 schema 时只引用表名；自定义 SQL 的 expect 原样透传。
	out, err := quality.Build(rule(quality.KindCustomSQL, map[string]any{
		"sql": "SELECT 1 AS observed, 1 AS expected FROM t WHERE 1=1", "expect": "ok"}), "", "t", dialect.MySQL{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Expect != "ok" {
		t.Fatalf("expect 透传错误: %+v", out)
	}
}

// BuildWith 的缺陷面：
//   - 默认 severity 缺失会让门禁形同虚设；
//   - 正则/引用检查的方言差异写错就是「永远通过」；
//   - 环比条件方向写反会让告警反着响。
func TestBuildWith(t *testing.T) {
	t.Run("旧规则委托", func(t *testing.T) {
		out, err := quality.BuildWith(rule(quality.KindNotNull, nil), "s", "t", dialect.Postgres{})
		if err != nil || !strings.Contains(out.SQL, `WHERE "amount" IS NULL`) {
			t.Fatalf("应复用旧实现: %+v %v", out, err)
		}
	})
	t.Run("默认 severity", func(t *testing.T) {
		out, err := quality.BuildWith(model.QualityRule{Name: "u", Kind: quality.KindUniqueCombination,
			Config: map[string]any{"columns": []string{"a", "b"}}}, "s", "t", dialect.Postgres{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Severity != "error" {
			t.Fatalf("severity 应默认 error: %+v", out)
		}
		if !strings.Contains(out.SQL, `"a", "b"`) {
			t.Fatalf("组合列渲染错误: %s", out.SQL)
		}
	})
	t.Run("组合列校验", func(t *testing.T) {
		for _, cols := range []any{[]string{"a"}, []any{"a", "b c"}} {
			_, err := quality.BuildWith(model.QualityRule{Kind: quality.KindUniqueCombination,
				Config: map[string]any{"columns": cols}}, "s", "t", dialect.Postgres{})
			if err == nil {
				t.Fatalf("%v 应报错", cols)
			}
		}
	})
	t.Run("正则各引擎", func(t *testing.T) {
		cases := []struct {
			d    dialect.Dialect
			want string
		}{
			{dialect.Postgres{}, `!~ 'a''b'`},
			{dialect.MySQL{}, `NOT REGEXP 'a''b'`},
			{dialect.ClickHouse{}, "NOT match(`amount`, 'a''b')"},
			{dialect.DuckDB{}, `NOT regexp_matches("amount", 'a''b')`},
		}
		for _, tc := range cases {
			out, err := quality.BuildWith(rule(quality.KindRegex,
				map[string]any{"pattern": "a'b"}), "s", "t", tc.d)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.SQL, tc.want) {
				t.Fatalf("%s 正则渲染错误: %s", tc.d.Name(), out.SQL)
			}
		}
		// 缺 pattern 与非法正则都必须报错。
		for _, cfg := range []map[string]any{nil, {"pattern": "["}} {
			if _, err := quality.BuildWith(rule(quality.KindRegex, cfg), "", "t", dialect.Postgres{}); err == nil {
				t.Fatalf("应报错: %v", cfg)
			}
		}
	})
	t.Run("引用检查", func(t *testing.T) {
		out, err := quality.BuildWith(rule(quality.KindReferential, map[string]any{
			"references": "dim_user", "references_column": "uid", "references_schema": "dim"}),
			"dwd", "dwd_order", dialect.Postgres{})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{`FROM "dim"."dim_user"`, `p."uid" = c."amount"`, "NOT EXISTS"} {
			if !strings.Contains(out.SQL, w) {
				t.Fatalf("引用 SQL 缺少 %q: %s", w, out.SQL)
			}
		}
		// 未指定被引用列时沿用本列名。
		out, err = quality.BuildWith(rule(quality.KindReferential, map[string]any{
			"references": "dim_user"}), "", "t", dialect.Postgres{})
		if err != nil || !strings.Contains(out.SQL, `p."amount" = c."amount"`) {
			t.Fatalf("默认引用列错误: %+v", out)
		}
		if _, err := quality.BuildWith(rule(quality.KindReferential, nil), "", "t", dialect.Postgres{}); err == nil {
			t.Fatal("缺少 references 应报错")
		}
	})
	t.Run("环比", func(t *testing.T) {
		// 默认阈值 30%，默认日期列 dt。
		out, err := quality.BuildWith(rule(quality.KindRowCountDelta, nil), "s", "t", dialect.Postgres{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Compare != "le" || out.Threshold != 30 || !strings.Contains(out.Expect, "30%") {
			t.Fatalf("默认环比配置错误: %+v", out)
		}
		if !strings.Contains(out.SQL, `"dt"`) || !strings.Contains(out.SQL, "ABS(") {
			t.Fatalf("环比 SQL 错误: %s", out.SQL)
		}
		// 自定义阈值与日期列。
		out, err = quality.BuildWith(rule(quality.KindRowCountDelta, map[string]any{
			"max_delta_pct": 10, "date_column": "stat_date"}), "", "t", dialect.Postgres{})
		if err != nil || out.Threshold != 10 || !strings.Contains(out.SQL, `"stat_date"`) {
			t.Fatalf("自定义环比错误: %+v %v", out, err)
		}
		// 方向：drop/rise 的 CASE 分支必须不同。
		rise, _ := quality.BuildWith(rule(quality.KindRowCountDelta, map[string]any{"direction": "rise"}), "", "t", dialect.Postgres{})
		drop, _ := quality.BuildWith(rule(quality.KindRowCountDelta, map[string]any{"direction": "drop"}), "", "t", dialect.Postgres{})
		if rise.SQL == out.SQL || drop.SQL == out.SQL || rise.SQL == drop.SQL {
			t.Fatal("不同方向的环比 SQL 必须不同")
		}
		if !strings.Contains(drop.SQL, "WHEN") {
			t.Fatalf("drop 方向缺少条件分支: %s", drop.SQL)
		}
	})
	t.Run("未知类型不panic", func(t *testing.T) {
		out, err := quality.BuildWith(rule("nope", nil), "s", "t", dialect.Postgres{})
		if err != nil {
			t.Fatalf("未知类型应交给上层校验而不是报错: %v", err)
		}
		if out.SQL != "" || out.Severity != "error" {
			t.Fatalf("未知类型应返回空 SQL 与默认严重级别: %+v", out)
		}
	})
}

// 新鲜度：阈值换算与判定全在 Go 里，写错单位就会「永远通过」。
func TestBuildWithFreshness(t *testing.T) {
	r := model.QualityRule{Name: "f", Kind: quality.KindFreshness, Column: "dt", Severity: "warn",
		Config: map[string]any{"max_age_hours": 6}}
	out, err := quality.BuildWith(r, "dwd", "dwd_order", dialect.ClickHouse{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != quality.KindFreshness || out.FreshColumn != "dt" || out.FreshMaxMin != 360 {
		t.Fatalf("新鲜度配置错误: %+v", out)
	}
	if out.Compare != "le" || out.Threshold != 360 {
		t.Fatalf("比较方式错误: %+v", out)
	}
	if !strings.Contains(out.SQL, "SELECT MAX(`dt`) AS latest") {
		t.Fatalf("SQL 错误: %s", out.SQL)
	}
	if !strings.Contains(out.Expect, "6 小时") {
		t.Fatalf("展示文案错误: %s", out.Expect)
	}
	// 缺时间列必须报错，否则编译出的 MAX() 是非法 SQL。
	if _, err := quality.BuildWith(model.QualityRule{Name: "f", Kind: quality.KindFreshness}, "", "t", dialect.Postgres{}); err == nil {
		t.Fatal("缺少时间列应报错")
	}
	if _, err := quality.BuildWith(model.QualityRule{Name: "f", Kind: quality.KindFreshness, Column: "a b"},
		"", "t", dialect.Postgres{}); err == nil {
		t.Fatal("非法列名应报错")
	}
}

// 阈值解析：天/小时/分钟三档以及非法值的降级。
func TestFreshnessWindow(t *testing.T) {
	cases := []struct {
		cfg   map[string]any
		want  float64
		label string
	}{
		{map[string]any{"max_age_min": 90}, 90, "90 分钟"},
		{map[string]any{"max_age_hours": 2}, 120, "2 小时"},
		{map[string]any{"max_age_days": 2}, 2880, "2 天"},
		{map[string]any{"max_age_hours": "3"}, 180, "3 小时"},
		{map[string]any{"max_age_min": 0, "max_age_hours": -1}, 1440, "1 天"},
		{nil, 1440, "1 天"},
	}
	for _, tc := range cases {
		got, label := quality.FreshnessWindow(tc.cfg)
		if got != tc.want || label != tc.label {
			t.Fatalf("FreshnessWindow(%v) = %v/%s，期望 %v/%s", tc.cfg, got, label, tc.want, tc.label)
		}
	}
}

// custom_sql 的只读守卫：写操作与无条件扫描必须在编译期拦住。
func TestValidateCustomSQL(t *testing.T) {
	good := []string{
		"SELECT COUNT(*) FROM t WHERE a = 1",
		"select count(*) from t\nwhere a = 1",
	}
	for _, s := range good {
		if err := quality.ValidateCustomSQL(s); err != nil {
			t.Fatalf("%q 应通过: %v", s, err)
		}
	}
	bad := []struct {
		sql  string
		code errs.Code
	}{
		{"   ", errs.CodeInvalidArgument},
		{"DELETE FROM t WHERE id = 1", errs.CodeInvalidArgument},
		{"SELECT * FROM t", errs.CodeInvalidArgument},
		{"select 1", errs.CodeInvalidArgument},
		{"UPDATE t SET a = 1 WHERE id = 1", errs.CodeInvalidArgument},
	}
	for _, tc := range bad {
		err := quality.ValidateCustomSQL(tc.sql)
		if !errs.Is(err, tc.code) {
			t.Fatalf("%q 应返回 %v，实际 %v", tc.sql, tc.code, err)
		}
	}
	// 写操作错误要带命中的关键字，方便用户定位。
	err := quality.ValidateCustomSQL("INSERT INTO t WHERE x = 1")
	ae := err.(*errs.AppError)
	if ae.Meta["keyword"] == nil {
		t.Fatalf("写操作错误应带 keyword: %+v", ae.Meta)
	}
}

// 作用域收窄：质量规则默认校验全表，日更管道必须只看当前分区，
// 但 freshness 是「最新一条有多旧」，绝不能收窄。
func TestScope(t *testing.T) {
	rules := []quality.Rule{
		{Name: "nn", SQL: `SELECT COUNT(*) AS observed, 0 AS expected FROM "dwd"."dwd_order" WHERE "amount" IS NULL`},
		{Name: "f", Kind: quality.KindFreshness, SQL: `SELECT MAX("dt") AS latest FROM "dwd"."dwd_order"`},
	}
	if got := quality.Scope(rules, "dwd", "dwd_order", dialect.Postgres{}, "  "); &got[0] != &rules[0] {
		t.Fatal("空 where 应原样返回")
	}
	out := quality.Scope(rules, "dwd", "dwd_order", dialect.Postgres{}, "dt = '2024-01-01'")
	if !strings.Contains(out[0].SQL, `FROM (SELECT * FROM "dwd"."dwd_order" WHERE dt = '2024-01-01') godw_scope`) {
		t.Fatalf("普通规则的主表未被收窄: %s", out[0].SQL)
	}
	if !strings.Contains(out[0].SQL, "godw_scope") {
		t.Fatalf("收窄 SQL 错误: %s", out[0].SQL)
	}
	if out[1].SQL != rules[1].SQL {
		t.Fatalf("freshness 不应被收窄: %s", out[1].SQL)
	}
	if rules[0].SQL != `SELECT COUNT(*) AS observed, 0 AS expected FROM "dwd"."dwd_order" WHERE "amount" IS NULL` {
		t.Fatal("Scope 不能修改入参切片中的元素")
	}
}

func rows(vals ...[]any) connector.Rows {
	return connector.NewMemRows([]connector.ColumnMeta{{Name: "observed"}, {Name: "expected"}}, vals)
}

// 判定分支：默认等值、le 阈值、扫描失败、无结果、迭代错误。
func TestRun(t *testing.T) {
	ctx := context.Background()
	t.Run("等值通过", func(t *testing.T) {
		c := &fakeConn{rows: rows([]any{"0", "0"})}
		out, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || !out[0].Passed || out[0].Observed != "0" {
			t.Fatalf("结果错误: %+v", out)
		}
	})
	t.Run("等值失败", func(t *testing.T) {
		c := &fakeConn{rows: rows([]any{int64(3), int64(0)})}
		out, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}})
		if err != nil || out[0].Passed {
			t.Fatalf("应判定失败: %+v %v", out, err)
		}
	})
	t.Run("le 各数值类型", func(t *testing.T) {
		cases := []struct {
			obs, exp any
			passed   bool
		}{
			{5, 10.0, true},
			{float32(5), int32(5), true},
			{int64(9), int64(5), false},
			{[]byte("3"), []byte("5"), true},
			{"4", "8", true},
			{"abc", "8", false},
			{true, false, false},
		}
		for _, tc := range cases {
			c := &fakeConn{rows: rows([]any{tc.obs, tc.exp})}
			out, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", Compare: "le", SQL: "select"}})
			if err != nil {
				t.Fatalf("%v: %v", tc, err)
			}
			if out[0].Passed != tc.passed {
				t.Fatalf("%v 判定 = %v，期望 %v", tc, out[0].Passed, tc.passed)
			}
		}
	})
	t.Run("查询失败", func(t *testing.T) {
		c := &fakeConn{err: errors.New("db down")}
		if _, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}}); !errs.Is(err, errs.CodeExecutionFailed) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("无结果行", func(t *testing.T) {
		c := &fakeConn{rows: &emptyRows{}}
		if _, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}}); !errs.Is(err, errs.CodeExecutionFailed) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("迭代出错", func(t *testing.T) {
		want := errors.New("iter failed")
		c := &fakeConn{rows: &emptyRows{err: want}}
		if _, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}}); !errors.Is(err, want) {
			t.Fatalf("应返回底层错误: %v", err)
		}
	})
	t.Run("扫描失败", func(t *testing.T) {
		c := &fakeConn{rows: connector.NewMemRows(nil, [][]any{{"only-one"}})}
		if _, err := quality.Run(ctx, c, []quality.Rule{{Name: "a", SQL: "select"}}); err == nil {
			t.Fatal("扫描失败应报错")
		}
	})
	t.Run("空规则集", func(t *testing.T) {
		out, err := quality.Run(ctx, &fakeConn{}, nil)
		if err != nil || len(out) != 0 {
			t.Fatalf("空规则集结果错误: %+v %v", out, err)
		}
	})
}

// 新鲜度判定：空表必须判失败（没有数据比数据旧更严重），
// 各种时间表示都要能解析。
func TestRunFreshness(t *testing.T) {
	ctx := context.Background()
	fresh := quality.Rule{Name: "f", Kind: quality.KindFreshness, SQL: "select", FreshMaxMin: 120, Severity: "error", Expect: "≤ 2 小时"}
	oneCol := func(v any) connector.Rows {
		return connector.NewMemRows([]connector.ColumnMeta{{Name: "latest"}}, [][]any{{v}})
	}

	cases := []struct {
		name   string
		val    any
		passed bool
		obs    string
	}{
		{"刚更新", time.Now().Add(-time.Minute), true, "分钟前"},
		{"过旧", time.Now().Add(-4 * time.Hour), false, "分钟前"},
		{"字符串", "2020-01-02 15:04:05", false, "2020-01-02 15:04"},
		{"字节串", []byte("2020-01-02T15:04:05"), false, "2020-01-02 15:04"},
		{"空值", nil, false, "无数据"},
		{"无法解析", "not-a-time", false, "无数据"},
		{"整数", 123, false, "无数据"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := quality.Run(ctx, &fakeConn{rows: oneCol(tc.val)}, []quality.Rule{fresh})
			if err != nil {
				t.Fatal(err)
			}
			if out[0].Passed != tc.passed || !strings.Contains(out[0].Observed, tc.obs) {
				t.Fatalf("结果错误: passed=%v observed=%q", out[0].Passed, out[0].Observed)
			}
			if out[0].Expected != fresh.Expect || out[0].SQL != fresh.SQL {
				t.Fatalf("元信息未透传: %+v", out[0])
			}
		})
	}

	t.Run("查询失败", func(t *testing.T) {
		_, err := quality.Run(ctx, &fakeConn{err: errors.New("down")}, []quality.Rule{fresh})
		if !errs.Is(err, errs.CodeExecutionFailed) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("无结果", func(t *testing.T) {
		_, err := quality.Run(ctx, &fakeConn{rows: &emptyRows{}}, []quality.Rule{fresh})
		if !errs.Is(err, errs.CodeExecutionFailed) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("迭代出错", func(t *testing.T) {
		want := errors.New("iter")
		_, err := quality.Run(ctx, &fakeConn{rows: &emptyRows{err: want}}, []quality.Rule{fresh})
		if !errors.Is(err, want) {
			t.Fatalf("应返回底层错误: %v", err)
		}
	})
	t.Run("扫描失败", func(t *testing.T) {
		_, err := quality.Run(ctx, &fakeConn{rows: connector.NewMemRows(nil, [][]any{{}})}, []quality.Rule{fresh})
		if err == nil {
			t.Fatal("扫描失败应报错")
		}
	})
}

// 门禁：只有 error 级失败拦截发布，warn 级失败只记录。
func TestGate(t *testing.T) {
	if err := quality.Gate(nil); err != nil {
		t.Fatalf("无结果应放行: %v", err)
	}
	pass := []quality.Result{{RuleName: "a", Passed: true, Severity: "error"}}
	if err := quality.Gate(pass); err != nil {
		t.Fatalf("全部通过应放行: %v", err)
	}
	warn := []quality.Result{{RuleName: "w", Passed: false, Severity: "warn"}}
	if err := quality.Gate(warn); err != nil {
		t.Fatalf("warn 不应拦截: %v", err)
	}
	fail := []quality.Result{
		{RuleName: "a", Passed: false, Severity: "error", Observed: "3", Expected: "0"},
		{RuleName: "b", Passed: false, Severity: "error", Observed: "1", Expected: "0"},
	}
	err := quality.Gate(fail)
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("错误码错误: %v", err)
	}
	if !strings.Contains(err.Error(), "a(实际 3，期望 0)") || !strings.Contains(err.Error(), "b(") {
		t.Fatalf("失败详情缺失: %v", err)
	}
}

// 规则类型清单必须与 ValidKind 同步：前端下拉少一项，用户就配不出对应规则。
func TestKinds(t *testing.T) {
	kinds := quality.Kinds()
	if len(kinds) != 11 {
		t.Fatalf("规则类型数量错误: %v", kinds)
	}
	for _, k := range kinds {
		if !quality.ValidKind(k) {
			t.Fatalf("%s 应在 ValidKind 中合法", k)
		}
	}
	if quality.ValidKind("nope") {
		t.Fatal("未知类型不应合法")
	}
}
