package dialect_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
)

// 标识符拆分的缺陷在于「带点的引号标识符」：库里真有人建名为 "a.b" 的列，
// 早先按点裸拆会把一个标识符拆成两段，生成的 SQL 直接指向不存在的对象。
// 转义不正确的直接后果是注入或语法错误：标识符里的引号必须双写。
func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		name string
		d    dialect.Dialect
		in   string
		want string
	}{
		{"pg 普通", dialect.Postgres{}, "users", `"users"`},
		{"pg 带库表", dialect.Postgres{}, "public.users", `"public"."users"`},
		{"pg 通配符不转义", dialect.Postgres{}, "public.*", `"public".*`},
		{"pg 内嵌双引号转义", dialect.Postgres{}, `a"b`, `"a""b"`},
		{"pg 已带引号", dialect.Postgres{}, `"a.b"`, `"a.b"`},
		{"mysql 反引号", dialect.MySQL{}, "users", "`users`"},
		{"mysql 内嵌反引号转义", dialect.MySQL{}, "a`b", "`a``b`"},
		{"ch 反引号", dialect.ClickHouse{}, "a.b", "`a`.`b`"},
		{"duckdb 双引号", dialect.DuckDB{}, "a.b", `"a"."b"`},
		{"ansi 双引号", dialect.ANSI{}, "a.b", `"a"."b"`},
		{"空标识符", dialect.Postgres{}, "", `""`},
		{"尾部点", dialect.Postgres{}, "a.", `"a".""`},
		{"引号段参与多点", dialect.Postgres{}, `"a.b".c`, `"a.b"."c"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.QuoteIdent(tc.in); got != tc.want {
				t.Fatalf("QuoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// 分页方言的经典坑：MySQL 的 LIMIT 是「偏移,条数」，其余是「条数 OFFSET 偏移」。
// 偏移为 0 时不能多写 OFFSET，否则部分引擎会走全表扫描。
func TestLimitOffset(t *testing.T) {
	cases := []struct {
		name string
		d    dialect.Dialect
		l, o int64
		want string
	}{
		{"pg 无偏移", dialect.Postgres{}, 10, 0, "LIMIT 10"},
		{"pg 有偏移", dialect.Postgres{}, 10, 5, "LIMIT 10 OFFSET 5"},
		{"pg 负偏移按无偏移", dialect.Postgres{}, 3, -1, "LIMIT 3"},
		{"mysql 无偏移", dialect.MySQL{}, 10, 0, "LIMIT 10"},
		{"mysql 有偏移", dialect.MySQL{}, 10, 5, "LIMIT 5, 10"},
		{"ch 无偏移", dialect.ClickHouse{}, 10, 0, "LIMIT 10"},
		{"ch 有偏移", dialect.ClickHouse{}, 10, 5, "LIMIT 10 OFFSET 5"},
		{"duckdb 无偏移", dialect.DuckDB{}, 10, 0, "LIMIT 10"},
		{"duckdb 有偏移", dialect.DuckDB{}, 10, 5, "LIMIT 10 OFFSET 5"},
		{"ansi 永远带 OFFSET", dialect.ANSI{}, 10, 0, "LIMIT 10 OFFSET 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.LimitOffset(tc.l, tc.o); got != tc.want {
				t.Fatalf("LimitOffset(%d,%d) = %q, want %q", tc.l, tc.o, got, tc.want)
			}
		})
	}
}

// 各引擎的日期截断函数家族完全不同，漏一个分支就会生成别家语法。
func TestDateTrunc(t *testing.T) {
	cases := []struct {
		name string
		d    dialect.Dialect
		g, e string
		want string
	}{
		{"pg", dialect.Postgres{}, "day", "ts", "DATE_TRUNC('day', ts)"},
		{"mysql 小时", dialect.MySQL{}, "hour", "ts", "DATE_FORMAT(ts, '%Y-%m-%d %H:00:00')"},
		{"mysql 月", dialect.MySQL{}, "month", "ts", "DATE_FORMAT(ts, '%Y-%m-01')"},
		{"mysql 兜底", dialect.MySQL{}, "day", "ts", "DATE(ts)"},
		{"ch 小时", dialect.ClickHouse{}, "hour", "ts", "toStartOfHour(ts)"},
		{"ch 周", dialect.ClickHouse{}, "week", "ts", "toStartOfWeek(ts)"},
		{"ch 月", dialect.ClickHouse{}, "month", "ts", "toStartOfMonth(ts)"},
		{"ch 兜底", dialect.ClickHouse{}, "day", "ts", "toDate(ts)"},
		{"duckdb", dialect.DuckDB{}, "week", "ts", "DATE_TRUNC('week', ts)"},
		{"ansi", dialect.ANSI{}, "month", "ts", "DATE_TRUNC('month', ts)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.DateTrunc(tc.g, tc.e); got != tc.want {
				t.Fatalf("DateTrunc(%q) = %q, want %q", tc.g, got, tc.want)
			}
		})
	}
}

// 负偏移必须变成减法："- INTERVAL -3 day" 是非法 SQL。
func TestDateAdd(t *testing.T) {
	cases := []struct {
		name string
		d    dialect.Dialect
		n    int
		u    string
		want string
	}{
		{"pg 正数", dialect.Postgres{}, 3, "day", "(ts + make_interval(days => 3))"},
		{"pg 负数", dialect.Postgres{}, -2, "hour", "(ts - make_interval(hours => 2))"},
		{"pg 分钟", dialect.Postgres{}, 5, "minute", "(ts + make_interval(minutes => 5))"},
		{"pg 月", dialect.Postgres{}, 1, "month", "(ts + make_interval(months => 1))"},
		{"pg 未知单位保持原样", dialect.Postgres{}, 1, "week", "(ts + make_interval(weeks => 1))"},
		{"mysql", dialect.MySQL{}, 3, "day", "DATE_ADD(ts, INTERVAL 3 day)"},
		{"ch", dialect.ClickHouse{}, 3, "day", "ts + INTERVAL 3 day"},
		{"duckdb", dialect.DuckDB{}, -1, "day", "(ts + INTERVAL -1 day)"},
		{"ansi", dialect.ANSI{}, 2, "hour", "(ts + INTERVAL 2 hour)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.d.DateAdd("ts", tc.n, tc.u)
			if got != tc.want {
				t.Fatalf("DateAdd(%d,%q) = %q, want %q", tc.n, tc.u, got, tc.want)
			}
		})
	}
}

func TestPlaceholder(t *testing.T) {
	cases := []struct {
		d    dialect.Dialect
		i    int
		want string
	}{
		{dialect.Postgres{}, 1, "$1"},
		{dialect.Postgres{}, 7, "$7"},
		{dialect.MySQL{}, 1, "?"},
		{dialect.MySQL{}, 7, "?"},
		{dialect.ClickHouse{}, 2, "?"},
		{dialect.DuckDB{}, 3, "$3"},
		{dialect.ANSI{}, 4, "?"},
	}
	for _, tc := range cases {
		if got := tc.d.Placeholder(tc.i); got != tc.want {
			t.Errorf("%s.Placeholder(%d) = %q, want %q", tc.d.Name(), tc.i, got, tc.want)
		}
	}
}

// dialect.ANSI 兜底方言复用 pg 的类型表，避免自造一套不在任何引擎上验证过的类型。
func TestANSICastType(t *testing.T) {
	cases := map[string]string{
		dialect.LString: "varchar(255)", dialect.LText: "text", dialect.LInt: "integer", dialect.LBigInt: "bigint",
		dialect.LDecimal: "numeric(20,4)", dialect.LDouble: "double precision", dialect.LBool: "boolean",
		dialect.LDate: "date", dialect.LDatetime: "timestamptz", dialect.LJSON: "jsonb", "blob": "text",
	}
	for logical, want := range cases {
		if got := (dialect.ANSI{}).CastType(logical); got != want {
			t.Errorf("dialect.ANSI.CastType(%s) = %q, want %q", logical, got, want)
		}
	}
}

func TestCastTypeMatrix(t *testing.T) {
	// 声明每个引擎对其逻辑类型的期望，防止矩阵被无声改坏。
	type row struct{ engine, logical, want string }
	rows := []row{
		{"postgres", dialect.LString, "varchar(255)"},
		{"postgres", dialect.LText, "text"},
		{"postgres", dialect.LInt, "integer"},
		{"postgres", dialect.LBigInt, "bigint"},
		{"postgres", dialect.LDecimal, "numeric(20,4)"},
		{"postgres", dialect.LDouble, "double precision"},
		{"postgres", dialect.LBool, "boolean"},
		{"postgres", dialect.LDate, "date"},
		{"postgres", dialect.LDatetime, "timestamptz"},
		{"postgres", dialect.LJSON, "jsonb"},
		{"postgres", "unknown", "text"},
		{"mysql", dialect.LString, "varchar(255)"},
		{"mysql", dialect.LText, "text"},
		{"mysql", dialect.LInt, "int"},
		{"mysql", dialect.LBigInt, "bigint"},
		{"mysql", dialect.LDecimal, "decimal(20,4)"},
		{"mysql", dialect.LDouble, "double"},
		{"mysql", dialect.LBool, "tinyint(1)"},
		{"mysql", dialect.LDate, "date"},
		{"mysql", dialect.LDatetime, "datetime(3)"},
		{"mysql", dialect.LJSON, "json"},
		{"mysql", "unknown", "text"},
		{"clickhouse", dialect.LString, "String"},
		{"clickhouse", dialect.LText, "String"},
		{"clickhouse", dialect.LInt, "Int32"},
		{"clickhouse", dialect.LBigInt, "Int64"},
		{"clickhouse", dialect.LDecimal, "Decimal(20,4)"},
		{"clickhouse", dialect.LDouble, "Float64"},
		{"clickhouse", dialect.LBool, "UInt8"},
		{"clickhouse", dialect.LDate, "Date"},
		{"clickhouse", dialect.LDatetime, "DateTime64(3)"},
		{"clickhouse", dialect.LJSON, "String"},
		{"clickhouse", "unknown", "String"},
		{"duckdb", dialect.LString, "varchar(255)"},
		{"duckdb", dialect.LText, "text"},
		{"duckdb", dialect.LInt, "integer"},
		{"duckdb", dialect.LBigInt, "bigint"},
		{"duckdb", dialect.LDecimal, "numeric(20,4)"},
		{"duckdb", dialect.LDouble, "double precision"},
		{"duckdb", dialect.LBool, "boolean"},
		{"duckdb", dialect.LDate, "date"},
		{"duckdb", dialect.LDatetime, "timestamptz"},
		{"duckdb", dialect.LJSON, "jsonb"},
		{"duckdb", "unknown", "text"},
	}
	ds := map[string]dialect.Dialect{
		"postgres": dialect.Postgres{}, "mysql": dialect.MySQL{}, "clickhouse": dialect.ClickHouse{}, "duckdb": dialect.DuckDB{},
	}
	for _, r := range rows {
		d := ds[r.engine]
		if got := d.CastType(r.logical); got != r.want {
			t.Errorf("%s.CastType(%s) = %q, want %q", r.engine, r.logical, got, r.want)
		}
	}
}

// 函数名映射必须按引擎走：把 coalesce 写进 dialect.MySQL 会直接报函数不存在。
func TestFunc(t *testing.T) {
	cases := []struct {
		name string
		d    dialect.Dialect
		fn   string
		args []string
		want string
	}{
		{"pg ifnull", dialect.Postgres{}, "ifnull", []string{"a", "b"}, "coalesce(a, b)"},
		{"pg nvl", dialect.Postgres{}, "nvl", []string{"a", "0"}, "coalesce(a, 0)"},
		{"pg 未知函数原样", dialect.Postgres{}, "upper", []string{"a"}, "upper(a)"},
		{"pg 无参", dialect.Postgres{}, "now", nil, "now()"},
		{"mysql ifnull", dialect.MySQL{}, "ifnull", []string{"a", "b"}, "ifnull(a, b)"},
		{"mysql nvl 映射", dialect.MySQL{}, "nvl", []string{"a", "b"}, "ifnull(a, b)"},
		{"mysql 未知函数", dialect.MySQL{}, "concat_ws", []string{"-", "a"}, "concat_ws(-, a)"},
		{"ch ifnull 大小写", dialect.ClickHouse{}, "ifnull", []string{"a", "b"}, "ifNull(a, b)"},
		{"ch nvl 映射", dialect.ClickHouse{}, "nvl", []string{"a", "b"}, "ifNull(a, b)"},
		{"ch 无参", dialect.ClickHouse{}, "now", nil, "now()"},
		{"duckdb 复用 pg 映射", dialect.DuckDB{}, "ifnull", []string{"a", "b"}, "coalesce(a, b)"},
		{"ansi 不映射", dialect.ANSI{}, "ifnull", []string{"a", "b"}, "ifnull(a, b)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.Func(tc.fn, tc.args...); got != tc.want {
				t.Fatalf("Func(%q) = %q, want %q", tc.fn, got, tc.want)
			}
		})
	}
}

// 能力位决定上层是否降级；错一位就会生成跑不通的 SQL。
func TestSupportsFeature(t *testing.T) {
	features := []dialect.Feature{dialect.FeatureMergeUpsert, dialect.FeatureWindow, dialect.FeatureApproxDistinct, dialect.FeatureJSON, dialect.FeatureQualify}
	cases := []struct {
		d    dialect.Dialect
		want map[dialect.Feature]bool
	}{
		{dialect.Postgres{}, map[dialect.Feature]bool{dialect.FeatureMergeUpsert: true, dialect.FeatureWindow: true, dialect.FeatureApproxDistinct: true, dialect.FeatureJSON: true}},
		{dialect.MySQL{}, map[dialect.Feature]bool{dialect.FeatureMergeUpsert: true, dialect.FeatureWindow: true, dialect.FeatureApproxDistinct: true, dialect.FeatureJSON: true}},
		{dialect.ClickHouse{}, map[dialect.Feature]bool{dialect.FeatureWindow: true, dialect.FeatureApproxDistinct: true, dialect.FeatureJSON: true}},
		{dialect.DuckDB{}, map[dialect.Feature]bool{dialect.FeatureMergeUpsert: true, dialect.FeatureWindow: true, dialect.FeatureJSON: true}},
		{dialect.ANSI{}, map[dialect.Feature]bool{}},
	}
	for _, tc := range cases {
		for _, f := range features {
			if got, want := tc.d.SupportsFeature(f), tc.want[f]; got != want {
				t.Errorf("%s.SupportsFeature(%s) = %v, want %v", tc.d.Name(), f, got, want)
			}
		}
	}
}

func TestRegistry(t *testing.T) {
	for _, name := range []string{"postgres", "mysql", "clickhouse", "duckdb", "ansi"} {
		d, ok := dialect.Get(name)
		if !ok || d.Name() != name {
			t.Fatalf("方言未注册或名字不一致: %s", name)
		}
	}
	if _, ok := dialect.Get("oracle"); ok {
		t.Fatal("未注册的引擎不应返回方言")
	}
	names := dialect.Names()
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"postgres", "mysql", "clickhouse", "duckdb", "ansi"} {
		if !got[want] {
			t.Fatalf("dialect.Names() 缺少 %s: %v", want, names)
		}
	}
	if len(names) != len(got) {
		t.Fatalf("dialect.Names() 返回重复项: %v", names)
	}
}

type stubDialect struct{ dialect.ANSI }

func (stubDialect) Name() string { return "stub-for-registry-test" }

func TestRegisterCustomDialect(t *testing.T) {
	dialect.Register(stubDialect{})
	if d, ok := dialect.Get("stub-for-registry-test"); !ok || d.Name() != "stub-for-registry-test" {
		t.Fatal("自定义方言注册后应可获取")
	}
	// 重复注册按名字覆盖，不应 panic（与 connector 的严格语义不同）。
	dialect.Register(stubDialect{})
}

func TestTypeSpecString(t *testing.T) {
	cases := []struct {
		spec dialect.TypeSpec
		want string
	}{
		{dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 18, Scale: 2}, "decimal(18,2)"},
		{dialect.TypeSpec{Logical: dialect.LDecimal}, "decimal"},
		{dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 0, Scale: 5}, "decimal"},
		{dialect.TypeSpec{Logical: dialect.LArray, Elem: "int"}, "array<int>"},
		{dialect.TypeSpec{Logical: dialect.LArray}, "array"},
		{dialect.TypeSpec{Logical: dialect.LMap}, "map"},
		{dialect.TypeSpec{Logical: dialect.LString}, "string"},
	}
	for _, tc := range cases {
		if got := tc.spec.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// 矩阵必须逐引擎逐逻辑类型都能给出确定结果：某格缺失会导致建表编译失败。
func TestMapTypeAllEngines(t *testing.T) {
	for _, engine := range dialect.MatrixEngines() {
		for _, logical := range dialect.MatrixLogicalTypes() {
			spec := dialect.TypeSpec{Logical: logical}
			if logical == dialect.LDecimal {
				spec.Precision, spec.Scale = 18, 2
			}
			if logical == dialect.LArray {
				spec.Elem = dialect.LString
			}
			m, err := dialect.MapType(engine, spec)
			if err != nil {
				t.Fatalf("%s/%s 映射失败: %v", engine, logical, err)
			}
			if m.Physical == "" {
				t.Fatalf("%s/%s 物理类型为空", engine, logical)
			}
			if strings.Contains(m.Physical, "%") {
				t.Fatalf("%s/%s 物理类型未渲染参数: %q", engine, logical, m.Physical)
			}
			if m.Lossy && m.Note == "" {
				t.Fatalf("%s/%s 标记有损却没有说明", engine, logical)
			}
		}
	}
}

func TestMapTypeUnknown(t *testing.T) {
	_, err := dialect.MapType("oracle", dialect.TypeSpec{Logical: dialect.LString})
	if err == nil {
		t.Fatal("未知引擎必须报错，不能静默兜底")
	}
	if !strings.Contains(err.Error(), "未知引擎") || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("错误信息应列出已支持引擎: %v", err)
	}

	_, err = dialect.MapType("postgres", dialect.TypeSpec{Logical: "blob"})
	if err == nil || !strings.Contains(err.Error(), "不支持逻辑类型") {
		t.Fatalf("未知逻辑类型应报错: %v", err)
	}
}

func TestMapTypeDecimal(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		p, s      int
		wantPhys  string
		wantLossy bool
		wantNote  string
	}{
		{"默认精度", "postgres", 0, 0, "numeric(20,4)", false, ""},
		{"正常精度", "postgres", 18, 2, "numeric(18,2)", false, ""},
		{"精度超限截断", "postgres", 40, 2, "numeric(38,2)", true, "截断"},
		{"标度超限截断", "postgres", 20, 25, "numeric(20,20)", true, "截断"},
		{"标度大于精度", "postgres", 5, 10, "numeric(5,10)", true, "截断"},
		{"mysql 上限更宽", "mysql", 60, 20, "decimal(60,20)", false, ""},
		{"ch 上限最宽", "clickhouse", 70, 30, "Decimal(70,30)", false, ""},
		{"duckdb 与 pg 一致", "duckdb", 39, 1, "DECIMAL(38,1)", true, "截断"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := dialect.MapType(tc.engine, dialect.TypeSpec{Logical: dialect.LDecimal, Precision: tc.p, Scale: tc.s})
			if err != nil {
				t.Fatalf("映射失败: %v", err)
			}
			if m.Lossy != tc.wantLossy {
				t.Fatalf("Lossy = %v, want %v (note=%q)", m.Lossy, tc.wantLossy, m.Note)
			}
			want := tc.wantPhys
			if m.Physical != want {
				t.Fatalf("Physical = %q, want %q", m.Physical, want)
			}
			if !m.NeedsParam {
				t.Fatal("decimal 映射必须标记 NeedsParam")
			}
			if tc.wantNote != "" && !strings.Contains(m.Note, tc.wantNote) {
				t.Fatalf("Note 应说明损失点，得到 %q", m.Note)
			}
		})
	}
}

func TestMapTypeArray(t *testing.T) {
	// 元素缺省时按 string 处理，而不是生成 "%s[]" 这种未渲染模板。
	m, err := dialect.MapType("postgres", dialect.TypeSpec{Logical: dialect.LArray})
	if err != nil {
		t.Fatal(err)
	}
	if m.Physical != "varchar(255)[]" || m.Lossy {
		t.Fatalf("缺省元素数组映射错误: %+v", m)
	}

	// 元素有损时数组整体必须打上有损标记，否则精度丢失不会被要求确认。
	m, err = dialect.MapType("clickhouse", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LJSON})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Lossy || !strings.Contains(m.Note, "数组元素有损") {
		t.Fatalf("有损元素未传播: %+v", m)
	}
	if m.Physical != "Array(String)" {
		t.Fatalf("数组物理类型错误: %q", m.Physical)
	}

	// dialect.MySQL 数组退化为 json，本身就有损。
	m, err = dialect.MapType("mysql", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LString})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Lossy || m.Physical != "json" {
		t.Fatalf("dialect.MySQL 数组应退化为 json 且有损: %+v", m)
	}

	// 元素非法时要报错并保留上下文，而不是生成畸形类型。
	if _, err := dialect.MapType("postgres", dialect.TypeSpec{Logical: dialect.LArray, Elem: "blob"}); err == nil {
		t.Fatal("非法元素类型应报错")
	} else if !strings.Contains(err.Error(), "数组元素类型") {
		t.Fatalf("错误应带上下文: %v", err)
	}
}

func TestReverseMap(t *testing.T) {
	cases := []struct {
		engine   string
		physical string
		want     dialect.TypeSpec
	}{
		{"clickhouse", "Nullable(Int32)", dialect.TypeSpec{Logical: dialect.LInt}},
		{"clickhouse", "Decimal(20,4)", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"clickhouse", "Array(String)", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LString}},
		{"clickhouse", "Map(String, String)", dialect.TypeSpec{Logical: dialect.LMap}},
		{"clickhouse", "String", dialect.TypeSpec{Logical: dialect.LString}},
		{"clickhouse", "FixedString(16)", dialect.TypeSpec{Logical: dialect.LString}},
		{"clickhouse", "UInt64", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"clickhouse", "Int128", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"clickhouse", "Int32", dialect.TypeSpec{Logical: dialect.LInt}},
		{"clickhouse", "Float64", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"clickhouse", "Date", dialect.TypeSpec{Logical: dialect.LDate}},
		{"clickhouse", "Date32", dialect.TypeSpec{Logical: dialect.LDate}},
		{"clickhouse", "DateTime64(3)", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"clickhouse", "IPv4", dialect.TypeSpec{Logical: dialect.LString}},

		{"mysql", "decimal(20,4)", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"mysql", "numeric(10,2)", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"mysql", "json", dialect.TypeSpec{Logical: dialect.LJSON}},
		{"mysql", "tinyint(1)", dialect.TypeSpec{Logical: dialect.LBool}},
		{"mysql", "bool", dialect.TypeSpec{Logical: dialect.LBool}},
		{"mysql", "boolean", dialect.TypeSpec{Logical: dialect.LBool}},
		{"mysql", "bigint(20)", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"mysql", "int(11)", dialect.TypeSpec{Logical: dialect.LInt}},
		{"mysql", "mediumint(9)", dialect.TypeSpec{Logical: dialect.LInt}},
		{"mysql", "smallint", dialect.TypeSpec{Logical: dialect.LInt}},
		{"mysql", "double", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"mysql", "float", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"mysql", "real", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"mysql", "date", dialect.TypeSpec{Logical: dialect.LDate}},
		{"mysql", "datetime(3)", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"mysql", "timestamp", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"mysql", "text", dialect.TypeSpec{Logical: dialect.LText}},
		{"mysql", "longtext", dialect.TypeSpec{Logical: dialect.LText}},
		{"mysql", "mediumtext", dialect.TypeSpec{Logical: dialect.LText}},
		{"mysql", "varchar(255)", dialect.TypeSpec{Logical: dialect.LString}},
		{"mysql", "blob", dialect.TypeSpec{Logical: dialect.LString}},

		{"postgres", "numeric(10,2)", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"postgres", "decimal", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"postgres", "json", dialect.TypeSpec{Logical: dialect.LJSON}},
		{"postgres", "jsonb", dialect.TypeSpec{Logical: dialect.LJSON}},
		{"postgres", "bool", dialect.TypeSpec{Logical: dialect.LBool}},
		{"postgres", "boolean", dialect.TypeSpec{Logical: dialect.LBool}},
		{"postgres", "bigint", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"postgres", "int8", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"postgres", "int", dialect.TypeSpec{Logical: dialect.LInt}},
		{"postgres", "integer", dialect.TypeSpec{Logical: dialect.LInt}},
		{"postgres", "int4", dialect.TypeSpec{Logical: dialect.LInt}},
		{"postgres", "smallint", dialect.TypeSpec{Logical: dialect.LInt}},
		{"postgres", "double precision", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"postgres", "double", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"postgres", "float8", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"postgres", "real", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"postgres", "date", dialect.TypeSpec{Logical: dialect.LDate}},
		{"postgres", "timestamp with time zone", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"postgres", "timestamp", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"postgres", "text", dialect.TypeSpec{Logical: dialect.LString}},
		{"postgres", "varchar(64)", dialect.TypeSpec{Logical: dialect.LString}},
		{"postgres", "character varying(64)", dialect.TypeSpec{Logical: dialect.LString}},
		{"postgres", "char", dialect.TypeSpec{Logical: dialect.LString}},
		{"postgres", "text[]", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LString}},
		{"postgres", "uuid", dialect.TypeSpec{Logical: dialect.LString}},
		{"postgres", "  BOOLEAN  ", dialect.TypeSpec{Logical: dialect.LBool}},
		{"postgres", "(weird", dialect.TypeSpec{Logical: dialect.LString}},

		{"duckdb", "DECIMAL(18,2)", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 20, Scale: 4}},
		{"duckdb", "JSON", dialect.TypeSpec{Logical: dialect.LJSON}},
		{"duckdb", "BOOLEAN", dialect.TypeSpec{Logical: dialect.LBool}},
		{"duckdb", "BIGINT", dialect.TypeSpec{Logical: dialect.LBigInt}},
		{"duckdb", "INTEGER", dialect.TypeSpec{Logical: dialect.LInt}},
		{"duckdb", "DOUBLE", dialect.TypeSpec{Logical: dialect.LDouble}},
		{"duckdb", "TIMESTAMP WITH TIME ZONE", dialect.TypeSpec{Logical: dialect.LDatetime}},
		{"duckdb", "VARCHAR", dialect.TypeSpec{Logical: dialect.LString}},
		{"duckdb", "VARCHAR[]", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LString}},
		{"duckdb", "MAP(VARCHAR, VARCHAR)", dialect.TypeSpec{Logical: dialect.LString}},
	}
	for _, tc := range cases {
		if got := dialect.ReverseMap(tc.engine, tc.physical); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("dialect.ReverseMap(%s, %q) = %+v, want %+v", tc.engine, tc.physical, got, tc.want)
		}
	}
}

func TestMatrixHelpers(t *testing.T) {
	if got, want := dialect.MatrixEngines(), []string{"clickhouse", "duckdb", "mysql", "postgres"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dialect.MatrixEngines() = %v, want %v", got, want)
	}
	wantTypes := []string{dialect.LString, dialect.LText, dialect.LInt, dialect.LBigInt, dialect.LDouble, dialect.LDecimal, dialect.LBool, dialect.LDate, dialect.LDatetime, dialect.LJSON, dialect.LArray, dialect.LMap}
	if got := dialect.MatrixLogicalTypes(); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("dialect.MatrixLogicalTypes() = %v, want %v", got, wantTypes)
	}

	m := dialect.Matrix()
	if len(m) != len(dialect.MatrixEngines()) {
		t.Fatalf("dialect.Matrix() 引擎数不对: %d", len(m))
	}
	// 导出的是副本：调用方改矩阵不能污染全局定义。
	orig := m["postgres"][dialect.LString]
	m["postgres"][dialect.LString] = dialect.TypeMapping{Physical: "hacked"}
	if dialect.Matrix()["postgres"][dialect.LString].Physical == "hacked" {
		t.Fatal("dialect.Matrix() 必须返回深拷贝，外部修改影响了全局矩阵")
	}
	if dialect.Matrix()["postgres"][dialect.LString] != orig {
		t.Fatal("dialect.Matrix() 二次调用结果不一致")
	}
}
