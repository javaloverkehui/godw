package quality

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0601 质量规则 DSL。
//
// 统一契约：每条规则编译成「返回 (observed, expected) 两列」的 SQL。
// 这样判定逻辑只有一处（observed == expected），新增规则类型不需要改执行器。
//
// 九类规则：not_null / unique / unique_combination / accepted_values / range /
//          regex / referential / row_count_delta / custom_sql

// Kind 是规则类型。
const (
	KindNotNull           = "not_null"
	KindUnique            = "unique"
	KindUniqueCombination = "unique_combination"
	KindAcceptedValues    = "accepted_values"
	KindRange             = "range"
	KindRegex             = "regex"
	KindReferential       = "referential"
	KindRowCountDelta     = "row_count_delta"
	KindCustomSQL         = "custom_sql"
	KindRowCount          = "row_count"
)

// ValidKind 校验规则类型。
func ValidKind(k string) bool {
	switch k {
	case KindNotNull, KindUnique, KindUniqueCombination, KindAcceptedValues,
		KindRange, KindRegex, KindReferential, KindRowCountDelta, KindCustomSQL, KindRowCount:
		return true
	}
	return false
}

// Kinds 返回全部规则类型，供前端下拉。
func Kinds() []string {
	return []string{KindNotNull, KindUnique, KindUniqueCombination, KindAcceptedValues,
		KindRange, KindRegex, KindReferential, KindRowCountDelta, KindRowCount, KindCustomSQL}
}

// dmlRe 用于拦截 custom_sql 里的写操作。只做「明显写语句」的拦截，
// 真正的权限兜底在连接层用只读事务。
var dmlRe = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|drop|truncate|alter|create|grant|revoke|copy)\b`)

// BuildWith 是增强版编译器，支持多列组合、引用检查、环比、正则。
// schema/table 是当前模型；d 决定方言；peer 用于 referential（被引用表）。
func BuildWith(rule model.QualityRule, schema, table string, d dialect.Dialect) (Rule, error) {
	// 先走老实现，覆盖最常用的简单规则。
	switch rule.Kind {
	case KindNotNull, KindUnique, KindAcceptedValues, KindRange, KindRowCount, KindCustomSQL:
		return Build(rule, schema, table, d)
	}

	t := qualify(schema, table, d)
	out := Rule{Name: rule.Name, Severity: rule.Severity}
	if out.Severity == "" {
		out.Severity = "error"
	}

	switch rule.Kind {
	case KindUniqueCombination:
		cols := stringSlice(rule.Config["columns"])
		if len(cols) < 2 {
			return out, errs.New(errs.CodeInvalidArgument, "unique_combination 需要至少 2 列")
		}
		for _, c := range cols {
			if err := assertColumn(rule, c, d); err != nil {
				return out, err
			}
		}
		colList := identList(cols, d)
		out.SQL = fmt.Sprintf(
			"SELECT COUNT(*) AS observed, 0 AS expected FROM (SELECT %s FROM %s GROUP BY %s HAVING COUNT(*) > 1) dup",
			colList, t, colList)
		out.Expect = "组合键重复数 = 0"

	case KindRegex:
		pattern, _ := rule.Config["pattern"].(string)
		if pattern == "" {
			return out, errs.New(errs.CodeInvalidArgument, "regex 需要 config.pattern")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return out, errs.Wrap(errs.CodeInvalidArgument, err, "正则表达式非法")
		}
		col := d.QuoteIdent(rule.Column)
		// 各引擎的正则语法不同：PG ~，MySQL REGEXP，CH match()，DuckDB regexp_matches。
		var cond string
		switch d.Name() {
		case "postgres":
			cond = fmt.Sprintf("%s IS NOT NULL AND %s !~ %s", col, col, quote(pattern))
		case "mysql":
			cond = fmt.Sprintf("%s IS NOT NULL AND %s NOT REGEXP %s", col, col, quote(pattern))
		case "clickhouse":
			cond = fmt.Sprintf("%s IS NOT NULL AND NOT match(%s, %s)", col, col, quote(pattern))
		default:
			cond = fmt.Sprintf("%s IS NOT NULL AND NOT regexp_matches(%s, %s)", col, col, quote(pattern))
		}
		out.SQL = fmt.Sprintf("SELECT COUNT(*) AS observed, 0 AS expected FROM %s WHERE %s", t, cond)
		out.Expect = "不匹配正则的行数 = 0"

	case KindReferential:
		peerTable, _ := rule.Config["references"].(string)
		if peerTable == "" {
			return out, errs.New(errs.CodeInvalidArgument, "referential 需要 config.references（被引用表）")
		}
		peerCol, _ := rule.Config["references_column"].(string)
		if peerCol == "" {
			peerCol = rule.Column
		}
		peerSchema, _ := rule.Config["references_schema"].(string)
		peer := qualify(peerSchema, peerTable, d)
		col := d.QuoteIdent(rule.Column)
		// 允许空值：外键语义下 NULL 不算违规。
		out.SQL = fmt.Sprintf(
			"SELECT COUNT(*) AS observed, 0 AS expected FROM %s c WHERE c.%s IS NOT NULL AND NOT EXISTS (SELECT 1 FROM %s p WHERE p.%s = c.%s)",
			t, col, peer, d.QuoteIdent(peerCol), col)
		out.Expect = "孤儿行数 = 0"

	case KindRowCountDelta:
		// 与上一个业务日期比较。没有历史时 observed=0 视为通过（首次跑数不该告警）。
		pct, hasPct := number(rule.Config["max_delta_pct"])
		if !hasPct {
			pct = 30
		}
		dir, _ := rule.Config["direction"].(string)
		dateCol := "dt"
		if c, ok := rule.Config["date_column"].(string); ok && c != "" {
			dateCol = c
		}
		col := d.QuoteIdent(dateCol)
		cur := fmt.Sprintf("(SELECT COUNT(*) FROM %s WHERE %s = ${biz_date})", t, col)
		prev := fmt.Sprintf("(SELECT COUNT(*) FROM %s WHERE %s = (${biz_date}::date - INTERVAL '1 day'))", t, col)
		// 用相对变化率作为 observed，0 表示正常；expected 为 0。
		expr := fmt.Sprintf(
			"SELECT CASE WHEN %s = 0 THEN 0 ELSE ABS(%s - %s) * 100.0 / %s END AS observed, 0 AS expected",
			prev, cur, prev, prev)
		if dir == "drop" {
			expr = fmt.Sprintf(
				"SELECT CASE WHEN %s = 0 THEN 0 WHEN %s < %s THEN (%s - %s) * 100.0 / %s ELSE 0 END AS observed, 0 AS expected",
				prev, cur, prev, prev, cur, prev)
		} else if dir == "rise" {
			expr = fmt.Sprintf(
				"SELECT CASE WHEN %s = 0 THEN 0 WHEN %s > %s THEN (%s - %s) * 100.0 / %s ELSE 0 END AS observed, 0 AS expected",
				prev, cur, prev, cur, prev, prev)
		}
		// observed 是变化率，expected 是阈值：判定改成 observed <= expected 才算通过，
		// 因此这里返回 expected = 阈值，由调用方用 <= 判定（见 Rule.Compare）。
		out.SQL = expr
		out.Expect = fmt.Sprintf("变化率 ≤ %.0f%%", pct)
		out.Compare = "le"
		out.Threshold = pct
	}
	return out, nil
}

// ValidateCustomSQL 拦截 custom_sql 中的写操作。
func ValidateCustomSQL(sqlText string) error {
	if strings.TrimSpace(sqlText) == "" {
		return errs.New(errs.CodeInvalidArgument, "custom_sql 不能为空")
	}
	if loc := dmlRe.FindStringIndex(sqlText); loc != nil {
		word := sqlText[loc[0]:loc[1]]
		return errs.New(errs.CodeInvalidArgument,
			"custom_sql 只允许只读查询，检测到写操作: "+strings.ToUpper(word)).
			WithMeta("keyword", word)
	}
	// 必须有 WHERE：没有过滤条件的计数在质量规则里几乎总是写错了。
	if !strings.Contains(strings.ToLower(sqlText), " where ") &&
		!strings.Contains(strings.ToLower(sqlText), "\nwhere ") {
		return errs.New(errs.CodeInvalidArgument,
			"custom_sql 必须带 WHERE 条件（返回违规行数）")
	}
	return nil
}

func assertColumn(rule model.QualityRule, col string, d dialect.Dialect) error {
	if strings.TrimSpace(col) == "" {
		return errs.New(errs.CodeInvalidArgument, "规则 "+rule.Name+" 的列名不能为空")
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(col) {
		return errs.New(errs.CodeInvalidArgument, "列名非法: "+col)
	}
	return nil
}

func identList(cols []string, d dialect.Dialect) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = d.QuoteIdent(c)
	}
	return strings.Join(out, ", ")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// Scope 把已编译规则的作用域收窄到 where 指定的数据子集。
//
// 为什么需要：质量节点默认校验整张表。日更管道跑到 11-09 时，如果 11-08 留了
// 一行脏数据，规则会一直失败——今天是它，明天还是它，用户被迫去「放行」，
// 门禁就形同虚设。正确语义是「校验本次运行影响的数据」，也就是当前分区。
//
// 实现上只替换主表的 FROM 子句：编译产物统一是 `FROM <qualified table>`，
// 换成同名派生表即可，9 类规则无需各自改造。referential 规则里的被引用表
// 不做收窄（它本来就该查全量维表），custom_sql 若查的是别的表也不受影响。
func Scope(rules []Rule, schema, table string, d dialect.Dialect, where string) []Rule {
	where = strings.TrimSpace(where)
	if where == "" {
		return rules
	}
	base := qualify(schema, table, d)
	from := "FROM " + base
	scoped := "FROM (SELECT * FROM " + base + " WHERE " + where + ") godw_scope"
	out := make([]Rule, len(rules))
	for i, r := range rules {
		r.SQL = strings.ReplaceAll(r.SQL, from, scoped)
		out[i] = r
	}
	return out
}
