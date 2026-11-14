package quality

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// Rule 是编译后的可执行规则。
type Rule struct {
	Name     string
	SQL      string
	Expect   string
	Severity string
	// Compare 是判定方式：空/"eq" 表示 observed == expected；"le" 表示 observed <= expected。
	Compare string
	// Threshold 供展示用（例如变化率阈值）。
	Threshold float64
	// Kind 仅在判定方式与「单 SQL 返回两列」不同时使用。
	// freshness 要在 Go 里算时间差：把时差写进 SQL 需要 INTERVAL/EPOCH 之类的方言差异，
	// 每种引擎写法都不一样，写错一次就是静默通过，比没有这条规则更危险。
	Kind string
	// FreshColumn / FreshMaxMin 仅 freshness 使用：时间列与阈值（分钟）。
	FreshColumn string
	FreshMaxMin float64
}

// Build 把质量规则编译为一条返回单值的 SQL。
// 统一约定：SQL 返回 (observed, expected) 两列，避免每种规则各写一套判定。
func Build(rule model.QualityRule, schema, table string, d dialect.Dialect) (Rule, error) {
	t := qualify(schema, table, d)
	col := d.QuoteIdent(rule.Column)
	out := Rule{Name: rule.Name, Severity: rule.Severity}

	switch rule.Kind {
	case "not_null":
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, 0 AS expected FROM %s WHERE %s IS NULL", t, col)
		out.Expect = "空值数 = 0"
	case "unique":
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, 0 AS expected FROM (SELECT %s FROM %s GROUP BY %s HAVING COUNT(*) > 1) dup", col, t, col)
		out.Expect = "重复键数 = 0"
	case "accepted_values":
		vals, ok := rule.Config["values"].([]any)
		if !ok || len(vals) == 0 {
			return out, errs.New(errs.CodeInvalidArgument, "accepted_values 需要 config.values")
		}
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
		}
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, 0 AS expected FROM %s WHERE %s NOT IN (%s)",
			t, col, strings.Join(quoted, ","))
		out.Expect = "越界值数 = 0"
	case "range":
		min, hasMin := number(rule.Config["min"])
		max, hasMax := number(rule.Config["max"])
		var conds []string
		if hasMin {
			conds = append(conds, fmt.Sprintf("%s < %v", col, min))
		}
		if hasMax {
			conds = append(conds, fmt.Sprintf("%s > %v", col, max))
		}
		if len(conds) == 0 {
			return out, errs.New(errs.CodeInvalidArgument, "range 需要 config.min 或 config.max")
		}
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, 0 AS expected FROM %s WHERE %s",
			t, strings.Join(conds, " OR "))
		out.Expect = fmt.Sprintf("[%v, %v] 区间外数 = 0", rule.Config["min"], rule.Config["max"])
	case "row_count":
		min, _ := number(rule.Config["min"])
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, %v AS expected FROM %s", min, t)
		out.Expect = fmt.Sprintf("行数 ≥ %v", min)
	case "custom_sql":
		sqlText, _ := rule.Config["sql"].(string)
		if strings.TrimSpace(sqlText) == "" {
			return out, errs.New(errs.CodeInvalidArgument, "custom_sql 需要 config.sql")
		}
		if err := ValidateCustomSQL(sqlText); err != nil {
			return out, err
		}
		out.SQL = sqlText
		out.Expect = fmt.Sprint(rule.Config["expect"])
	default:
		return out, errs.New(errs.CodeInvalidArgument, "未知规则类型: "+rule.Kind)
	}
	return out, nil
}

// Result 是单条规则的判定结果。
type Result struct {
	RuleName string `json:"ruleName"`
	Passed   bool   `json:"passed"`
	Observed string `json:"observed"`
	Expected string `json:"expected"`
	Severity string `json:"severity"`
	SQL      string `json:"sql"`
}

// Run 执行规则集合。severity=error 的失败会让整体失败；warn 只记录。
func Run(ctx context.Context, c connector.Connector, rules []Rule) ([]Result, error) {
	out := make([]Result, 0, len(rules))
	for _, r := range rules {
		res, err := runOne(ctx, c, r)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func runOne(ctx context.Context, c connector.Connector, r Rule) (Result, error) {
	if r.Kind == KindFreshness {
		return runFreshness(ctx, c, r)
	}
	rows, err := c.Query(ctx, r.SQL)
	if err != nil {
		return Result{RuleName: r.Name, Severity: r.Severity, SQL: r.SQL},
			errs.Wrap(errs.CodeExecutionFailed, err, "质量规则执行失败: "+r.Name)
	}
	defer rows.Close()

	res := Result{RuleName: r.Name, Severity: r.Severity, Expected: r.Expect, SQL: r.SQL}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return res, err
		}
		return res, errs.New(errs.CodeExecutionFailed, "质量规则未返回结果: "+r.Name)
	}
	var observed, expected any
	if err := rows.Scan(&observed, &expected); err != nil {
		return res, err
	}
	res.Observed = fmt.Sprint(observed)
	switch r.Compare {
	case "le":
		obs, ok1 := toFloat(observed)
		exp, ok2 := toFloat(expected)
		res.Passed = ok1 && ok2 && obs <= exp
	default:
		res.Passed = fmt.Sprint(observed) == fmt.Sprint(expected)
	}
	return res, nil
}

// runFreshness 判定「表里最新的一条数据有多旧」。
//
// 与其它规则的区别只有一处：不靠 SQL 做判定，而是取出 MAX(时间列) 后在 Go 里比。
// 原因是「当前时间 - 最大值」在各引擎的写法完全不同（PG 要 EXTRACT(EPOCH ...)，
// MySQL 要 TIMESTAMPDIFF，ClickHouse 要 dateDiff），写错不会报错，只会永远通过。
func runFreshness(ctx context.Context, c connector.Connector, r Rule) (Result, error) {
	res := Result{RuleName: r.Name, Severity: r.Severity, Expected: r.Expect, SQL: r.SQL}
	rows, err := c.Query(ctx, r.SQL)
	if err != nil {
		return res, errs.Wrap(errs.CodeExecutionFailed, err, "新鲜度规则执行失败: "+r.Name)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return res, err
		}
		return res, errs.New(errs.CodeExecutionFailed, "新鲜度规则未返回结果: "+r.Name)
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return res, err
	}
	latest, ok := toTime(raw)
	if !ok {
		// 空表：没有数据比「数据很旧」更严重，必须判失败而不是跳过。
		res.Observed = "无数据"
		res.Passed = false
		return res, nil
	}
	age := time.Since(latest).Minutes()
	res.Observed = fmt.Sprintf("%s（%.0f 分钟前）", latest.Format("2006-01-02 15:04"), age)
	res.Passed = age <= r.FreshMaxMin
	return res, nil
}

// toTime 把驱动返回的任意时间表示转成 time.Time。
func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case string:
		return parseTimeString(t)
	case []byte:
		return parseTimeString(string(t))
	}
	return time.Time{}, false
}

func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05",
		"2006-01-02 15:04", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// toFloat 把驱动返回的任意数值类型转成 float64，用于阈值比较。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case []byte:
		var f float64
		if _, err := fmt.Sscan(string(n), &f); err == nil {
			return f, true
		}
	case string:
		var f float64
		if _, err := fmt.Sscan(n, &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Gate 判断规则集合是否放行。
func Gate(results []Result) error {
	var failed []string
	for _, r := range results {
		if !r.Passed && r.Severity == "error" {
			failed = append(failed, fmt.Sprintf("%s(实际 %s，期望 %s)", r.RuleName, r.Observed, r.Expected))
		}
	}
	if len(failed) > 0 {
		return errs.New(errs.CodeQualityGate, "质量门禁未通过: "+strings.Join(failed, "; "))
	}
	return nil
}

func qualify(schema, table string, d dialect.Dialect) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscan(n, &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
