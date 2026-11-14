package quality

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

func rule(kind string, cfg map[string]any) model.QualityRule {
	return model.QualityRule{Name: "r", Kind: kind, Column: "amount", Severity: "error", Config: cfg}
}

func TestBuildSQL(t *testing.T) {
	cases := []struct {
		name string
		rule model.QualityRule
		want string
	}{
		{"not_null", rule("not_null", nil), `WHERE "amount" IS NULL`},
		{"unique", rule("unique", nil), "GROUP BY \"amount\" HAVING COUNT(*) > 1"},
		{"accepted", rule("accepted_values", map[string]any{"values": []any{"a", "b"}}), `NOT IN ('a','b')`},
		{"range", rule("range", map[string]any{"min": float64(0), "max": float64(100)}), `"amount" < 0 OR "amount" > 100`},
		{"row_count", rule("row_count", map[string]any{"min": float64(10)}), "COUNT(*)"},
	}
	for _, c := range cases {
		got, err := Build(c.rule, "dw", "t", dialect.Postgres{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(got.SQL, c.want) {
			t.Errorf("%s SQL 缺少 %q\n%s", c.name, c.want, got.SQL)
		}
	}
}

func TestBuildInvalid(t *testing.T) {
	if _, err := Build(rule("accepted_values", nil), "", "t", dialect.Postgres{}); err == nil {
		t.Error("accepted_values 缺 values 应报错")
	}
	if _, err := Build(rule("range", nil), "", "t", dialect.Postgres{}); err == nil {
		t.Error("range 缺边界应报错")
	}
	if _, err := Build(rule("unknown", nil), "", "t", dialect.Postgres{}); err == nil {
		t.Error("未知规则应报错")
	}
}

func TestGate(t *testing.T) {
	err := Gate([]Result{{RuleName: "a", Passed: false, Severity: "error", Observed: "3", Expected: "0"}})
	if err == nil {
		t.Fatal("error 级失败应阻断")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("应指出规则名: %v", err)
	}

	err = Gate([]Result{{RuleName: "b", Passed: false, Severity: "warn"}})
	if err != nil {
		t.Errorf("warn 级失败不应阻断: %v", err)
	}
}

func TestClickHouseQuoting(t *testing.T) {
	got, err := Build(rule("not_null", nil), "dw", "t", dialect.ClickHouse{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.SQL, "`amount` IS NULL") {
		t.Errorf("ClickHouse 应使用反引号: %s", got.SQL)
	}
}
