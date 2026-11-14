package quality

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

func qrule(kind string, cfg map[string]any) model.QualityRule {
	return model.QualityRule{Name: "r_" + kind, Kind: kind, Column: "amount", Severity: "error", Config: cfg}
}

// T0601 验收：各类规则各 1 个用例；custom_sql 含 DELETE 被拒；SQL 在四引擎可编译。
func TestAllKindsCompile(t *testing.T) {
	cases := []model.QualityRule{
		qrule(KindNotNull, nil),
		qrule(KindUnique, nil),
		qrule(KindUniqueCombination, map[string]any{"columns": []any{"a", "b"}}),
		qrule(KindAcceptedValues, map[string]any{"values": []any{"paid", "refund"}}),
		qrule(KindRange, map[string]any{"min": 0, "max": 100}),
		qrule(KindRegex, map[string]any{"pattern": "^[0-9]+$"}),
		qrule(KindReferential, map[string]any{"references": "dwd_user", "references_column": "user_id"}),
		qrule(KindRowCountDelta, map[string]any{"max_delta_pct": 30}),
		qrule(KindFreshness, map[string]any{"max_age_hours": 6}),
		qrule(KindCustomSQL, map[string]any{"sql": "SELECT COUNT(*) AS observed, 0 AS expected FROM t WHERE amount < 0"}),
	}
	if len(cases) != 10 {
		t.Fatalf("应覆盖 10 类规则，实际 %d", len(cases))
	}
	for _, engine := range []dialect.Dialect{dialect.Postgres{}, dialect.MySQL{}, dialect.ClickHouse{}, dialect.DuckDB{}} {
		for _, r := range cases {
			built, err := BuildWith(r, "dwd", "dwd_order", engine)
			if err != nil {
				t.Fatalf("%s/%s 编译失败: %v", engine.Name(), r.Kind, err)
			}
			if !strings.Contains(strings.ToUpper(built.SQL), "SELECT") {
				t.Fatalf("%s/%s 未生成 SELECT: %s", engine.Name(), r.Kind, built.SQL)
			}
			if built.Severity == "" {
				t.Fatalf("%s/%s 缺少 severity", engine.Name(), r.Kind)
			}
		}
	}
}

func TestCustomSQLRejectsDML(t *testing.T) {
	for _, bad := range []string{
		"DELETE FROM t WHERE x = 1",
		"UPDATE t SET x=1 WHERE id=1",
		"DROP TABLE t WHERE 1=1",
		"INSERT INTO t SELECT 1 WHERE 1=1",
		"TRUNCATE TABLE t WHERE 1=1",
	} {
		if err := ValidateCustomSQL(bad); err == nil {
			t.Fatalf("应拒绝写操作: %s", bad)
		}
	}
	// 无 WHERE 也拒绝
	if err := ValidateCustomSQL("SELECT COUNT(*) FROM t"); err == nil {
		t.Fatal("无 WHERE 的 custom_sql 应拒绝")
	}
	if err := ValidateCustomSQL("SELECT COUNT(*) AS observed, 0 AS expected FROM t WHERE x IS NULL"); err != nil {
		t.Fatalf("合法只读查询被误拒: %v", err)
	}
}

func TestUniqueCombinationNeedsTwoColumns(t *testing.T) {
	r := qrule(KindUniqueCombination, map[string]any{"columns": []any{"a"}})
	if _, err := BuildWith(r, "dwd", "t", dialect.Postgres{}); err == nil {
		t.Fatal("单列组合应报错")
	}
}

func TestRegexPerEngine(t *testing.T) {
	r := qrule(KindRegex, map[string]any{"pattern": "^1[3-9]\\d{9}$"})
	sqls := map[string]string{}
	for _, e := range []dialect.Dialect{dialect.Postgres{}, dialect.MySQL{}, dialect.ClickHouse{}} {
		b, err := BuildWith(r, "", "t", e)
		if err != nil {
			t.Fatal(err)
		}
		sqls[e.Name()] = b.SQL
	}
	if !strings.Contains(sqls["postgres"], "!~") {
		t.Fatalf("PG 应用 !~: %s", sqls["postgres"])
	}
	if !strings.Contains(sqls["mysql"], "NOT REGEXP") {
		t.Fatalf("MySQL 应用 NOT REGEXP: %s", sqls["mysql"])
	}
	if !strings.Contains(sqls["clickhouse"], "match(") {
		t.Fatalf("CH 应用 match(): %s", sqls["clickhouse"])
	}
	// 非法正则
	bad := qrule(KindRegex, map[string]any{"pattern": "("})
	if _, err := BuildWith(bad, "", "t", dialect.Postgres{}); err == nil {
		t.Fatal("非法正则应报错")
	}
}

func TestReferential(t *testing.T) {
	r := qrule(KindReferential, map[string]any{"references": "dwd_user", "references_column": "user_id"})
	b, err := BuildWith(r, "dwd", "dwd_order", dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.SQL, "NOT EXISTS") || !strings.Contains(b.SQL, "dwd_user") {
		t.Fatalf("引用检查 SQL 异常: %s", b.SQL)
	}
	// 空值不算违规
	if !strings.Contains(b.SQL, "IS NOT NULL") {
		t.Fatalf("应排除空值: %s", b.SQL)
	}
}

func TestRowCountDelta(t *testing.T) {
	r := qrule(KindRowCountDelta, map[string]any{"max_delta_pct": 20, "direction": "drop"})
	b, err := BuildWith(r, "dwd", "dwd_order", dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Compare != "le" || b.Threshold != 20 {
		t.Fatalf("阈值规则应使用 <= 判定: %+v", b)
	}
	if !strings.Contains(b.SQL, "INTERVAL '1 day'") {
		t.Fatalf("环比 SQL 异常: %s", b.SQL)
	}
}

func TestRunWithThreshold(t *testing.T) {
	// observed=10, expected=20 → 10 <= 20 通过
	r := Rule{Name: "delta", Severity: "error", Compare: "le"}
	res := compareOnly(r, "10", "20")
	if !res {
		t.Fatal("10 <= 20 应通过")
	}
	res = compareOnly(r, "30", "20")
	if res {
		t.Fatal("30 > 20 应失败")
	}
}

// compareOnly 复刻 runOne 的判定逻辑，避免依赖 DB。
func compareOnly(r Rule, observed, expected string) bool {
	switch r.Compare {
	case "le":
		o, ok1 := toFloat(observed)
		e, ok2 := toFloat(expected)
		return ok1 && ok2 && o <= e
	}
	return observed == expected
}

func TestGateWithSeverity(t *testing.T) {
	results := []Result{
		{RuleName: "a", Passed: true, Severity: "error"},
		{RuleName: "b", Passed: false, Severity: "warn"},
	}
	if err := Gate(results); err != nil {
		t.Fatal("warn 不应阻断")
	}
	results = append(results, Result{RuleName: "c", Passed: false, Severity: "error", Observed: "5", Expected: "0"})
	if err := Gate(results); err == nil {
		t.Fatal("error 规则失败应阻断")
	}
}

func TestValidKind(t *testing.T) {
	for _, k := range Kinds() {
		if !ValidKind(k) {
			t.Fatalf("%s 应为合法类型", k)
		}
	}
	if ValidKind("nope") {
		t.Fatal("未知类型不应合法")
	}
}

func TestScopeNarrowsMainTable(t *testing.T) {
	d := dialect.Postgres{}
	rules := []Rule{{
		Name: "amount",
		SQL:  `SELECT COUNT(*) AS observed, 0 AS expected FROM "dwd"."dwd_order" WHERE "amount" < 0`,
	}}
	got := Scope(rules, "dwd", "dwd_order", d, `"dt" = '2026-11-09'`)
	want := `SELECT COUNT(*) AS observed, 0 AS expected FROM (SELECT * FROM "dwd"."dwd_order" WHERE "dt" = '2026-11-09') godw_scope WHERE "amount" < 0`
	if got[0].SQL != want {
		t.Fatalf("作用域未生效:\n got=%s\nwant=%s", got[0].SQL, want)
	}
	// 原切片不能被就地修改，否则同一批规则被复用时作用域会叠加。
	if !strings.Contains(rules[0].SQL, `FROM "dwd"."dwd_order" WHERE`) {
		t.Fatal("原规则被修改")
	}
}

func TestScopeEmptyIsNoop(t *testing.T) {
	r := []Rule{{SQL: "SELECT 1"}}
	if got := Scope(r, "s", "t", dialect.Postgres{}, "  "); got[0].SQL != "SELECT 1" {
		t.Fatalf("空作用域不应改写: %s", got[0].SQL)
	}
}
