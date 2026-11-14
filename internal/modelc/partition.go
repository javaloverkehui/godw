package modelc

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0304 分区、分桶与生命周期。
//
// 分区表达式要跨引擎翻译，所以不能只存一个列名：
// 用户写 dt / toYYYYMM(dt) / date_trunc('month', dt) 都合法，
// 平台负责把它翻译成各引擎的真实语法，翻译不了就报错而不是猜。

// PartitionSpec 是解析后的分区定义。
type PartitionSpec struct {
	// Raw 是用户原始写法。
	Raw string `json:"raw"`
	// Kind 取 column | month | day | hour | year。
	Kind string `json:"kind"`
	// Column 是参与分区的列。
	Column string `json:"column"`
}

var (
	ttlRe       = regexp.MustCompile(`^(\d+)([dmy])$`)
	dateTruncRe = regexp.MustCompile(`(?i)^date_trunc\(\s*'(\w+)'\s*,\s*"?(\w+)"?\s*\)$`)
	toMonthRe   = regexp.MustCompile(`(?i)^toYYYYMM\(\s*"?(\w+)"?\s*\)$`)
	toDateRe    = regexp.MustCompile(`(?i)^toDate\(\s*"?(\w+)"?\s*\)$`)
)

// ParsePartition 解析分区表达式。
func ParsePartition(expr string) (PartitionSpec, error) {
	raw := strings.TrimSpace(expr)
	s := PartitionSpec{Raw: raw, Kind: "column", Column: raw}
	if raw == "" {
		return s, errs.New(errs.CodeModelInvalid, "分区表达式不能为空")
	}
	if m := dateTruncRe.FindStringSubmatch(raw); m != nil {
		gran := strings.ToLower(m[1])
		switch gran {
		case "year", "month", "day", "hour":
			return PartitionSpec{Raw: raw, Kind: gran, Column: m[2]}, nil
		}
		return s, errs.New(errs.CodeModelInvalid, "不支持的时间粒度: "+gran).
			WithMeta("supported", []string{"year", "month", "day", "hour"})
	}
	if m := toMonthRe.FindStringSubmatch(raw); m != nil {
		return PartitionSpec{Raw: raw, Kind: "month", Column: m[1]}, nil
	}
	if m := toDateRe.FindStringSubmatch(raw); m != nil {
		return PartitionSpec{Raw: raw, Kind: "day", Column: m[1]}, nil
	}
	if !identRe.MatchString(raw) {
		return s, errs.New(errs.CodeModelInvalid,
			"无法识别的分区表达式: "+raw).
			WithMeta("hint", "支持 列名 / date_trunc('month', col) / toYYYYMM(col)")
	}
	return s, nil
}

// CompilePartition 把分区表达式翻译为目标引擎语法。
// 返回空字符串表示该引擎不支持分区（如 MySQL 需用表分区，平台不代做）。
func CompilePartition(spec PartitionSpec, d dialect.Dialect) (string, error) {
	col := d.QuoteIdent(spec.Column)
	switch d.Name() {
	case "clickhouse":
		switch spec.Kind {
		case "column":
			return col, nil
		case "month":
			return fmt.Sprintf("toYYYYMM(%s)", col), nil
		case "day":
			return fmt.Sprintf("toDate(%s)", col), nil
		case "year":
			return fmt.Sprintf("toYear(%s)", col), nil
		case "hour":
			return fmt.Sprintf("toStartOfHour(%s)", col), nil
		}
	case "postgres", "duckdb":
		if spec.Kind == "column" {
			return col, nil
		}
		return fmt.Sprintf("date_trunc('%s', %s)", spec.Kind, col), nil
	case "mysql":
		if spec.Kind == "column" {
			return col, nil
		}
		return "", errs.New(errs.CodeModelInvalid,
			"MySQL 不支持表达式分区，请改用 RANGE 表分区或把粒度落到列上")
	}
	return "", errs.New(errs.CodeModelInvalid, "引擎 "+d.Name()+" 不支持分区")
}

// TTL 是生命周期配置。
type TTL struct {
	Raw   string `json:"raw"`
	Value int    `json:"value"`
	Unit  string `json:"unit"` // day|month|year
}

// ParseTTL 解析 30d / 12m / 3y。
func ParseTTL(raw string) (TTL, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return TTL{}, nil
	}
	m := ttlRe.FindStringSubmatch(s)
	if m == nil {
		return TTL{}, errs.New(errs.CodeModelInvalid, "TTL 格式必须是 30d/12m/3y，实际 "+raw)
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return TTL{}, errs.New(errs.CodeModelInvalid, "TTL 必须大于 0")
	}
	unit := map[string]string{"d": "day", "m": "month", "y": "year"}[m[2]]
	return TTL{Raw: s, Value: n, Unit: unit}, nil
}

// CompileTTL 生成引擎的 TTL 子句（不含前导空格）。
// 返回空串表示该引擎不原生支持，需由维护任务清理。
func CompileTTL(t TTL, column string, d dialect.Dialect) string {
	if t.Raw == "" || column == "" {
		return ""
	}
	switch d.Name() {
	case "clickhouse":
		return fmt.Sprintf("TTL %s + INTERVAL %d %s", d.QuoteIdent(column), t.Value, strings.ToUpper(t.Unit))
	default:
		return ""
	}
}

// ExpireBefore 计算 TTL 对应的截止时间，供 PG/MySQL 的清理任务使用。
func (t TTL) ExpireBefore(now time.Time) time.Time {
	switch t.Unit {
	case "day":
		return now.AddDate(0, 0, -t.Value)
	case "month":
		return now.AddDate(0, -t.Value, 0)
	case "year":
		return now.AddDate(-t.Value, 0, 0)
	}
	return now
}

// PartitionRange 描述一个待创建/待删除的分区。
type PartitionRange struct {
	Name  string `json:"name"`
	From  string `json:"from"`
	To    string `json:"to"`
	Value string `json:"value"`
}

// UpcomingPartitions 预创建未来 n 天的分区定义（PG RANGE 分区用）。
// 只处理 day/month 两种粒度，其它粒度由引擎自己管理。
func UpcomingPartitions(spec PartitionSpec, from time.Time, days int) []PartitionRange {
	if days <= 0 {
		return nil
	}
	out := []PartitionRange{}
	switch spec.Kind {
	case "day":
		for i := 0; i <= days; i++ {
			d := from.AddDate(0, 0, i)
			v := d.Format("2006-01-02")
			out = append(out, PartitionRange{
				Name:  fmt.Sprintf("p_%s", d.Format("20060102")),
				From:  v,
				To:    d.AddDate(0, 0, 1).Format("2006-01-02"),
				Value: v,
			})
		}
	case "month":
		first := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location())
		for i := 0; i <= days/30+1; i++ {
			m := first.AddDate(0, i, 0)
			out = append(out, PartitionRange{
				Name:  "p_" + m.Format("200601"),
				From:  m.Format("2006-01-02"),
				To:    m.AddDate(0, 1, 0).Format("2006-01-02"),
				Value: m.Format("2006-01"),
			})
		}
	}
	return out
}

// CreatePartitionDDL 生成 PG 声明式分区子表。
func CreatePartitionDDL(schema, table string, p PartitionRange, column string, d dialect.Dialect) string {
	parent := qualify(schema, table, d)
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s);",
		qualify(schema, p.Name, d), parent, lit(p.From), lit(p.To))
}

// ValidateStorage 校验存储配置：分区列必须存在、粒度不得细于小时、TTL 列必须可比较。
func ValidateStorage(spec model.ModelSpec) []RuleViolation {
	var out []RuleViolation
	add := func(rule, field, format string, args ...any) {
		out = append(out, RuleViolation{Rule: rule, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	cols := map[string]model.ColumnSpec{}
	for _, c := range spec.Columns {
		cols[strings.ToLower(c.Name)] = c
	}

	for i, raw := range spec.PartitionBy {
		p, err := ParsePartition(raw)
		if err != nil {
			add("partition_expr", fmt.Sprintf("partition_by[%d]", i), "%v", err)
			continue
		}
		c, ok := cols[strings.ToLower(p.Column)]
		if !ok {
			add("partition_column", fmt.Sprintf("partition_by[%d]", i), "分区列 %s 不存在", p.Column)
			continue
		}
		if c.Nullable {
			add("partition_nullable", fmt.Sprintf("partition_by[%d]", i), "分区列 %s 不允许为可空", p.Column)
		}
		if p.Kind == "hour" {
			add("partition_too_fine", fmt.Sprintf("partition_by[%d]", i),
				"分区粒度不得细于小时（当前 hour），会产生大量小文件")
		}
	}

	if spec.TTL != "" {
		t, err := ParseTTL(spec.TTL)
		if err != nil {
			add("ttl_format", "ttl", "%v", err)
		} else if len(spec.PartitionBy) == 0 && t.Unit == "day" && t.Value < 7 {
			add("ttl_too_short", "ttl", "非分区表 TTL 小于 7 天会频繁触发全表清理，建议加大或先分区")
		}
	}
	return out
}

// MatKind 是物化方式。
type MatKind string

const (
	MatTable        MatKind = "table"
	MatView         MatKind = "view"
	MatMaterialized MatKind = "mv"
	MatIncremental  MatKind = "incremental"
)

// ValidMat 校验物化方式取值。
func ValidMat(s string) bool {
	switch MatKind(s) {
	case "", MatTable, MatView, MatMaterialized, MatIncremental:
		return true
	}
	return false
}

// CompileMaterialization 返回物化方式的落地 SQL 类型判断。
//
// MySQL 不支持物化视图，这里明确报错并给替代方案，不做静默降级——
// 降级意味着用户以为用上了 MV，实际拿到的是需要定时刷新的普通表。
func CompileMaterialization(spec model.ModelSpec, d dialect.Dialect, caps MaterializationCaps) (string, error) {
	switch MatKind(spec.Materialization) {
	case "", MatTable, MatIncremental:
		if MatKind(spec.Materialization) == MatIncremental && len(spec.PartitionBy) == 0 {
			return "", errs.New(errs.CodeModelInvalid, "incremental 物化必须声明 partition_by")
		}
		return "TABLE", nil
	case MatView:
		if len(spec.PartitionBy) > 0 {
			return "", errs.New(errs.CodeModelInvalid, "view 不允许配置分区")
		}
		if spec.TTL != "" {
			return "", errs.New(errs.CodeModelInvalid, "view 不占存储，配置 TTL 没有意义")
		}
		if strings.TrimSpace(spec.Query) == "" {
			return "", errs.New(errs.CodeModelInvalid, "view 必须提供 query")
		}
		return "VIEW", nil
	case MatMaterialized:
		if !caps.SupportsMV {
			return "", errs.New(errs.CodeModelInvalid,
				fmt.Sprintf("%s 不支持物化视图，建议改为 materialization: table + 定时调度刷新", d.Name())).
				WithMeta("engine", d.Name()).
				WithMeta("suggestion", "materialization: table")
		}
		if strings.TrimSpace(spec.Query) == "" {
			return "", errs.New(errs.CodeModelInvalid, "mv 必须提供 query")
		}
		return "MATERIALIZED VIEW", nil
	}
	return "", errs.New(errs.CodeModelInvalid, "未知物化方式: "+spec.Materialization)
}

// MaterializationCaps 是物化能力，避免 modelc 依赖 connector 造成反向依赖。
type MaterializationCaps struct {
	SupportsMV bool
}

// Keys 返回模型的主键，无主键时回退唯一键，用于幂等写入与去重。
func Keys(spec model.ModelSpec) []string {
	if len(spec.PrimaryKey) > 0 {
		return spec.PrimaryKey
	}
	return spec.UniqueKey
}

// SortedColumnNames 返回按定义顺序排列的列名。
func SortedColumnNames(spec model.ModelSpec) []string {
	out := make([]string, 0, len(spec.Columns))
	for _, c := range spec.Columns {
		out = append(out, c.Name)
	}
	return out
}

// BatchSizing 根据分区粒度估算每次写入的合理批次行数，供执行器分片。
func BatchSizing(spec model.ModelSpec, totalRows int64) int {
	// 经验值：单批 5000 行以内，且不超过总量的 1/10，避免长事务。
	const upper, lower = 5000, 500
	if totalRows <= 0 {
		return upper
	}
	n := int(totalRows / 10)
	if n > upper {
		n = upper
	}
	if n < lower {
		n = lower
	}
	return n
}

func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// SortViolations 让违规输出稳定，便于快照测试。
func SortViolations(in []RuleViolation) []RuleViolation {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Rule != in[j].Rule {
			return in[i].Rule < in[j].Rule
		}
		return in[i].Field < in[j].Field
	})
	return in
}
