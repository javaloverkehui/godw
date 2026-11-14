package dialect

// Feature 是需要按引擎降级的能力点。编译器只在能力缺失时才改写 SQL。
type Feature string

const (
	FeatureMergeUpsert    Feature = "merge_upsert"
	FeatureWindow         Feature = "window"
	FeatureApproxDistinct Feature = "approx_distinct"
	FeatureJSON           Feature = "json"
	FeatureQualify        Feature = "qualify"
)

// Dialect 集中管理引擎差异。所有 SQL 生成必须经过它，禁止业务代码拼字符串。
type Dialect interface {
	Name() string
	QuoteIdent(ident string) string
	Placeholder(i int) string
	LimitOffset(limit, offset int64) string
	DateTrunc(granularity, expr string) string
	DateAdd(expr string, n int, unit string) string
	CastType(logical string) string
	Func(name string, args ...string) string
	SupportsFeature(f Feature) bool
}

// Logical 是模型层使用的逻辑类型，编译时映射为引擎类型。
const (
	LString   = "string"
	LText     = "text"
	LInt      = "int"
	LBigInt   = "bigint"
	LDecimal  = "decimal"
	LDouble   = "double"
	LBool     = "bool"
	LDate     = "date"
	LDatetime = "datetime"
	LJSON     = "json"
)

// splitIdent 按点拆分标识符，忽略双引号内的点。
func splitIdent(ident string) []string {
	parts := []string{}
	var cur []byte
	inQuote := false
	for i := 0; i < len(ident); i++ {
		c := ident[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			cur = append(cur, c)
		case c == '.' && !inQuote:
			parts = append(parts, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, c)
		}
	}
	parts = append(parts, string(cur))
	for i := range parts {
		parts[i] = stripQuote(parts[i])
	}
	return parts
}

func stripQuote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func joinIdent(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}
