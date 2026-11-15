package quality

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// identRe 与列名/规则名的约定保持一致：只接受裸标识符，避免引号与大小写方言。
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateDef 校验一条规则定义本身是否完整，不依赖具体数据源与方言。
//
// 存在的理由：规则以前只活在管道 YAML 的 map[string]any 里，写错字段名不会报错，
// 要等到跑数那一刻才以「SQL 执行失败」的形式暴露。规则独立成对象后，
// 保存时就必须把「缺 column」「正则写错」「custom_sql 没有 WHERE」拦下来。
func ValidateDef(rule model.QualityRule) error {
	name := strings.TrimSpace(rule.Name)
	if name == "" {
		return errs.New(errs.CodeInvalidArgument, "规则名不能为空")
	}
	if !identRe.MatchString(name) {
		return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("规则名必须是标识符（字母/数字/下划线），实际 %q", rule.Name))
	}
	switch rule.Severity {
	case "", "warn", "error":
	default:
		return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("severity 只能是 warn 或 error，实际 %q", rule.Severity))
	}

	needColumn := func() error {
		if strings.TrimSpace(rule.Column) == "" {
			return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("规则 %s（%s）必须指定 column", rule.Name, rule.Kind))
		}
		if !identRe.MatchString(rule.Column) {
			return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("规则 %s 的 column %q 不是合法列名", rule.Name, rule.Column))
		}
		return nil
	}

	switch rule.Kind {
	case KindNotNull, KindUnique, KindFreshness, KindRegex, KindReferential:
		if err := needColumn(); err != nil {
			return err
		}
	case KindAcceptedValues:
		if err := needColumn(); err != nil {
			return err
		}
		vals, ok := rule.Config["values"].([]any)
		if !ok || len(vals) == 0 {
			return errs.New(errs.CodeInvalidArgument, "accepted_values 需要非空的 config.values")
		}
	case KindRange:
		if err := needColumn(); err != nil {
			return err
		}
		_, hasMin := number(rule.Config["min"])
		_, hasMax := number(rule.Config["max"])
		if !hasMin && !hasMax {
			return errs.New(errs.CodeInvalidArgument, "range 需要 config.min 或 config.max")
		}
	case KindUniqueCombination:
		cols := stringSlice(rule.Config["columns"])
		if len(cols) < 2 {
			return errs.New(errs.CodeInvalidArgument, "unique_combination 需要至少 2 列（config.columns）")
		}
		for _, c := range cols {
			if !identRe.MatchString(c) {
				return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("unique_combination 的列名 %q 非法", c))
			}
		}
	case KindRowCount, KindRowCountDelta:
		// 两者只看行数，不需要列。row_count_delta 的阈值语义在编译期有默认值。
	case KindCustomSQL:
		sqlText, _ := rule.Config["sql"].(string)
		if err := ValidateCustomSQL(sqlText); err != nil {
			return err
		}
		if expect, _ := rule.Config["expect"].(string); strings.TrimSpace(expect) == "" {
			// expect 会原样出现在结果页与告警里，空值等于让人看不懂为什么失败。
			return errs.New(errs.CodeInvalidArgument, "custom_sql 需要 config.expect（失败时展示的期望说明）")
		}
	default:
		return errs.New(errs.CodeInvalidArgument, "未知规则类型: "+rule.Kind)
	}

	if rule.Kind == KindRegex {
		pattern, _ := rule.Config["pattern"].(string)
		if strings.TrimSpace(pattern) == "" {
			return errs.New(errs.CodeInvalidArgument, "regex 需要 config.pattern")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return errs.Wrap(errs.CodeInvalidArgument, err, "config.pattern 不是合法正则")
		}
	}
	if rule.Kind == KindReferential {
		ref, _ := rule.Config["references"].(string)
		if strings.TrimSpace(ref) == "" {
			return errs.New(errs.CodeInvalidArgument, "referential 需要 config.references（被引用表）")
		}
	}
	if rule.Kind == KindRowCountDelta {
		if dir, _ := rule.Config["direction"].(string); dir != "" && dir != "drop" && dir != "rise" {
			return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("row_count_delta 的 direction 只能是 drop 或 rise，实际 %q", dir))
		}
		if pct, ok := number(rule.Config["max_delta_pct"]); ok && pct <= 0 {
			return errs.New(errs.CodeInvalidArgument, "row_count_delta 的 max_delta_pct 必须为正")
		}
	}
	if rule.Kind == KindFreshness {
		for _, k := range []string{"max_age_min", "max_age_hours", "max_age_days"} {
			if n, ok := number(rule.Config[k]); ok && n <= 0 {
				return errs.New(errs.CodeInvalidArgument, fmt.Sprintf("freshness 的 %s 必须为正", k))
			}
		}
	}
	return nil
}

// IsEnabled 统一「未显式设置即启用」的语义。
// 规则默认是开着的：新建一条规则却默认不跑，比默认跑起来更危险。
func IsEnabled(rule model.QualityRule) bool {
	return rule.Enabled == nil || *rule.Enabled
}
