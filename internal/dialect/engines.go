package dialect

import "fmt"

func init() {
	Register(Postgres{})
	Register(MySQL{})
	Register(ClickHouse{})
	Register(DuckDB{})
	Register(ANSI{})
}

var registry = map[string]Dialect{}

// Register 注册方言，core 代码只通过名字获取。
func Register(d Dialect) { registry[d.Name()] = d }

// Get 返回指定引擎的方言，未知引擎回退 ANSI 并在调用方报错。
func Get(name string) (Dialect, bool) {
	d, ok := registry[name]
	return d, ok
}

// Names 返回已支持引擎，用于报错提示。
func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}

// Postgres 使用 $n 占位符、双引号、标准 DATE_TRUNC。
type Postgres struct{}

func (Postgres) Name() string { return "postgres" }
func (Postgres) QuoteIdent(s string) string {
	return quoteWith(s, '"')
}
func (Postgres) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (Postgres) LimitOffset(l, o int64) string {
	if o > 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", l, o)
	}
	return fmt.Sprintf("LIMIT %d", l)
}
func (Postgres) DateTrunc(g, e string) string { return fmt.Sprintf("DATE_TRUNC('%s', %s)", g, e) }
func (Postgres) DateAdd(e string, n int, u string) string {
	sign := "+"
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("(%s %s make_interval(%ss => %d))", e, sign, unitPlural(u), n)
}
func (Postgres) CastType(l string) string { return typeMap(l, pgTypes, "text") }
func (Postgres) Func(n string, a ...string) string {
	return call(funcMap(n, pgFuncs), a)
}
func (Postgres) SupportsFeature(f Feature) bool { return pgFeatures[f] }

var pgTypes = map[string]string{
	LString: "varchar(255)", LText: "text", LInt: "integer", LBigInt: "bigint",
	LDecimal: "numeric(20,4)", LDouble: "double precision", LBool: "boolean",
	LDate: "date", LDatetime: "timestamptz", LJSON: "jsonb",
}
var pgFuncs = map[string]string{"ifnull": "coalesce", "nvl": "coalesce", "now": "now", "concat": "concat"}
var pgFeatures = map[Feature]bool{FeatureMergeUpsert: true, FeatureWindow: true, FeatureApproxDistinct: true, FeatureJSON: true}

// MySQL 使用 ? 占位符、反引号、DATE_FORMAT 风格。
type MySQL struct{}

func (MySQL) Name() string               { return "mysql" }
func (MySQL) QuoteIdent(s string) string { return quoteWith(s, '`') }
func (MySQL) Placeholder(int) string     { return "?" }
func (MySQL) LimitOffset(l, o int64) string {
	if o > 0 {
		return fmt.Sprintf("LIMIT %d, %d", o, l)
	}
	return fmt.Sprintf("LIMIT %d", l)
}
func (MySQL) DateTrunc(g, e string) string {
	switch g {
	case "hour":
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:00:00')", e)
	case "month":
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-01')", e)
	default:
		return fmt.Sprintf("DATE(%s)", e)
	}
}
func (MySQL) DateAdd(e string, n int, u string) string {
	return fmt.Sprintf("DATE_ADD(%s, INTERVAL %d %s)", e, n, u)
}
func (MySQL) CastType(l string) string { return typeMap(l, myTypes, "text") }
func (MySQL) Func(n string, a ...string) string {
	return call(funcMap(n, myFuncs), a)
}
func (MySQL) SupportsFeature(f Feature) bool { return myFeatures[f] }

var myTypes = map[string]string{
	LString: "varchar(255)", LText: "text", LInt: "int", LBigInt: "bigint",
	LDecimal: "decimal(20,4)", LDouble: "double", LBool: "tinyint(1)",
	LDate: "date", LDatetime: "datetime(3)", LJSON: "json",
}
var myFuncs = map[string]string{"ifnull": "ifnull", "nvl": "ifnull", "now": "now", "concat": "concat"}
var myFeatures = map[Feature]bool{FeatureMergeUpsert: true, FeatureWindow: true, FeatureApproxDistinct: true, FeatureJSON: true}

// ClickHouse 用反引号、? 占位符，日期函数为 toStartOfX。
type ClickHouse struct{}

func (ClickHouse) Name() string               { return "clickhouse" }
func (ClickHouse) QuoteIdent(s string) string { return quoteWith(s, '`') }
func (ClickHouse) Placeholder(int) string     { return "?" }
func (ClickHouse) LimitOffset(l, o int64) string {
	if o > 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", l, o)
	}
	return fmt.Sprintf("LIMIT %d", l)
}
func (ClickHouse) DateTrunc(g, e string) string {
	switch g {
	case "hour":
		return fmt.Sprintf("toStartOfHour(%s)", e)
	case "week":
		return fmt.Sprintf("toStartOfWeek(%s)", e)
	case "month":
		return fmt.Sprintf("toStartOfMonth(%s)", e)
	default:
		return fmt.Sprintf("toDate(%s)", e)
	}
}
func (ClickHouse) DateAdd(e string, n int, u string) string {
	return fmt.Sprintf("%s + INTERVAL %d %s", e, n, u)
}
func (ClickHouse) CastType(l string) string {
	if l == LString || l == LText {
		return "String"
	}
	return typeMap(l, chTypes, "String")
}
func (ClickHouse) Func(n string, a ...string) string {
	return call(funcMap(n, chFuncs), a)
}
func (ClickHouse) SupportsFeature(f Feature) bool { return chFeatures[f] }

var chTypes = map[string]string{
	LString: "String", LText: "String", LInt: "Int32", LBigInt: "Int64",
	LDecimal: "Decimal(20,4)", LDouble: "Float64", LBool: "UInt8",
	LDate: "Date", LDatetime: "DateTime64(3)", LJSON: "String",
}
var chFuncs = map[string]string{"ifnull": "ifNull", "nvl": "ifNull", "now": "now", "concat": "concat"}
var chFeatures = map[Feature]bool{FeatureWindow: true, FeatureApproxDistinct: true, FeatureJSON: true}

// DuckDB 语法接近 PostgreSQL，便于单机演示。
type DuckDB struct{}

func (DuckDB) Name() string               { return "duckdb" }
func (DuckDB) QuoteIdent(s string) string { return quoteWith(s, '"') }
func (DuckDB) Placeholder(i int) string   { return fmt.Sprintf("$%d", i) }
func (DuckDB) LimitOffset(l, o int64) string {
	if o > 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", l, o)
	}
	return fmt.Sprintf("LIMIT %d", l)
}
func (DuckDB) DateTrunc(g, e string) string { return fmt.Sprintf("DATE_TRUNC('%s', %s)", g, e) }
func (DuckDB) DateAdd(e string, n int, u string) string {
	return fmt.Sprintf("(%s + INTERVAL %d %s)", e, n, u)
}
func (DuckDB) CastType(l string) string { return typeMap(l, pgTypes, "text") }
func (DuckDB) Func(n string, a ...string) string {
	return call(funcMap(n, pgFuncs), a)
}
func (DuckDB) SupportsFeature(f Feature) bool {
	return map[Feature]bool{FeatureMergeUpsert: true, FeatureWindow: true, FeatureJSON: true}[f]
}

// ANSI 是兜底方言：只保证最小可用集合。
type ANSI struct{}

func (ANSI) Name() string               { return "ansi" }
func (ANSI) QuoteIdent(s string) string { return quoteWith(s, '"') }
func (ANSI) Placeholder(int) string     { return "?" }
func (ANSI) LimitOffset(l, o int64) string {
	return fmt.Sprintf("LIMIT %d OFFSET %d", l, o)
}
func (ANSI) DateTrunc(g, e string) string { return fmt.Sprintf("DATE_TRUNC('%s', %s)", g, e) }
func (ANSI) DateAdd(e string, n int, u string) string {
	return fmt.Sprintf("(%s + INTERVAL %d %s)", e, n, u)
}
func (ANSI) CastType(l string) string { return typeMap(l, pgTypes, "text") }
func (ANSI) Func(n string, a ...string) string {
	return call(n, a)
}
func (ANSI) SupportsFeature(Feature) bool { return false }

func quoteWith(ident string, q byte) string {
	parts := splitIdent(ident)
	for i, p := range parts {
		if p == "*" {
			continue
		}
		parts[i] = string(q) + escape(p, q) + string(q)
	}
	return joinIdent(parts)
}

func escape(s string, q byte) string {
	out := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		if s[i] == q {
			out = append(out, q)
		}
		out = append(out, s[i])
	}
	return string(out)
}

func typeMap(l string, m map[string]string, fallback string) string {
	if t, ok := m[l]; ok {
		return t
	}
	return fallback
}

func funcMap(name string, m map[string]string) string {
	if v, ok := m[name]; ok {
		return v
	}
	return name
}

func call(name string, args []string) string {
	out := name + "("
	for i, a := range args {
		if i > 0 {
			out += ", "
		}
		out += a
	}
	return out + ")"
}

func unitPlural(u string) string {
	switch u {
	case "day":
		return "day"
	case "hour":
		return "hour"
	case "minute":
		return "minute"
	case "month":
		return "month"
	default:
		return u
	}
}
