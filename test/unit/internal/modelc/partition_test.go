package modelc_test

import (
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

// 分区表达式要跨引擎翻译，解析错误会让分区裁剪失效（最坏情况是全表扫描）。
func TestParsePartition(t *testing.T) {
	ok := []struct {
		in   string
		kind string
		col  string
	}{
		{"dt", "column", "dt"},
		{"  dt  ", "column", "dt"},
		{"date_trunc('year', dt)", "year", "dt"},
		{"date_trunc('month', dt)", "month", "dt"},
		{"date_trunc('day', \"dt\")", "day", "dt"},

		{"toYYYYMM(dt)", "month", "dt"},
		{"toYYYYMM(\"dt\")", "month", "dt"},
		{"toDate(dt)", "day", "dt"},
	}
	for _, tc := range ok {
		got, err := modelc.ParsePartition(tc.in)
		if err != nil {
			t.Fatalf("%q 应合法: %v", tc.in, err)
		}
		if got.Kind != tc.kind || got.Column != tc.col {
			t.Fatalf("%q 解析错误: %+v", tc.in, got)
		}
	}
	bad := []struct{ in, want string }{
		{"", "不能为空"},
		{"date_trunc('minute', dt)", "不支持的时间粒度"},
		{"a b", "无法识别"},
		{"sum(dt)", "无法识别"},
	}
	for _, tc := range bad {
		_, err := modelc.ParsePartition(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q 应按 %q 报错: %v", tc.in, tc.want, err)
		}
	}
	// 错误要带可选项，前端才能给出修复建议。
	_, err := modelc.ParsePartition("date_trunc('minute', dt)")
	if err.(*errs.AppError).Meta["supported"] == nil {
		t.Fatal("粒度错误应带 supported")
	}
	_, err = modelc.ParsePartition("a b")
	if err.(*errs.AppError).Meta["hint"] == nil {
		t.Fatal("表达式错误应带 hint")
	}
}

// 分区翻译的方言差异：写错一类就是真实集群上的语法错误。
func TestCompilePartition(t *testing.T) {
	cases := []struct {
		name string
		spec modelc.PartitionSpec
		d    dialect.Dialect
		want string
	}{
		{"ch 列", modelc.PartitionSpec{Kind: "column", Column: "dt"}, dialect.ClickHouse{}, "`dt`"},
		{"ch 月", modelc.PartitionSpec{Kind: "month", Column: "dt"}, dialect.ClickHouse{}, "toYYYYMM(`dt`)"},
		{"ch 日", modelc.PartitionSpec{Kind: "day", Column: "dt"}, dialect.ClickHouse{}, "toDate(`dt`)"},
		{"ch 年", modelc.PartitionSpec{Kind: "year", Column: "dt"}, dialect.ClickHouse{}, "toYear(`dt`)"},
		{"ch 小时", modelc.PartitionSpec{Kind: "hour", Column: "dt"}, dialect.ClickHouse{}, "toStartOfHour(`dt`)"},
		{"pg 列", modelc.PartitionSpec{Kind: "column", Column: "dt"}, dialect.Postgres{}, `"dt"`},
		{"pg 月", modelc.PartitionSpec{Kind: "month", Column: "dt"}, dialect.Postgres{}, `date_trunc('month', "dt")`},
		{"duckdb 日", modelc.PartitionSpec{Kind: "day", Column: "dt"}, dialect.DuckDB{}, `date_trunc('day', "dt")`},
		{"mysql 列", modelc.PartitionSpec{Kind: "column", Column: "dt"}, dialect.MySQL{}, "`dt`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := modelc.CompilePartition(tc.spec, tc.d)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q，want %q", got, tc.want)
			}
		})
	}
	// MySQL 与未登记引擎必须明确报错，不能静默忽略分区。
	if _, err := modelc.CompilePartition(modelc.PartitionSpec{Kind: "month", Column: "dt"}, dialect.MySQL{}); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("MySQL 表达式分区应报错: %v", err)
	}
	if _, err := modelc.CompilePartition(modelc.PartitionSpec{Kind: "column", Column: "dt"}, dialect.ANSI{}); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("ANSI 应报不支持: %v", err)
	}
	if _, err := modelc.CompilePartition(modelc.PartitionSpec{Kind: "weird", Column: "dt"}, dialect.ClickHouse{}); err == nil {
		t.Fatal("未知粒度应报错")
	}
}

func TestParseTTL(t *testing.T) {
	ok := []struct {
		in    string
		value int
		unit  string
	}{
		{"30d", 30, "day"},
		{"12m", 12, "month"},
		{"3y", 3, "year"},
		{" 30D ", 30, "day"},
	}
	for _, tc := range ok {
		got, err := modelc.ParseTTL(tc.in)
		if err != nil || got.Value != tc.value || got.Unit != tc.unit {
			t.Fatalf("%q 解析错误: %+v %v", tc.in, got, err)
		}
	}
	if got, err := modelc.ParseTTL("  "); err != nil || got.Raw != "" {
		t.Fatalf("空 TTL 应返回零值: %+v %v", got, err)
	}
	for _, in := range []string{"0d", "d30", "30w", "-1d"} {
		if _, err := modelc.ParseTTL(in); err == nil {
			t.Fatalf("%q 应报错", in)
		}
	}
}

// TTL 子句只有 ClickHouse 原生支持，其它引擎必须返回空串交给维护任务。
func TestCompileTTL(t *testing.T) {
	ttl := modelc.TTL{Raw: "30d", Value: 30, Unit: "day"}
	if got := modelc.CompileTTL(ttl, "dt", dialect.ClickHouse{}); got != "TTL `dt` + INTERVAL 30 DAY" {
		t.Fatalf("ClickHouse TTL = %q", got)
	}
	if got := modelc.CompileTTL(ttl, "dt", dialect.Postgres{}); got != "" {
		t.Fatalf("PG 不应生成 TTL: %q", got)
	}
	if got := modelc.CompileTTL(modelc.TTL{}, "dt", dialect.ClickHouse{}); got != "" {
		t.Fatalf("空 TTL 应返回空串: %q", got)
	}
	if got := modelc.CompileTTL(ttl, "", dialect.ClickHouse{}); got != "" {
		t.Fatalf("空列应返回空串: %q", got)
	}
}

// 过期时间算错会误删数据，这是合规事故而不是普通 bug。
func TestExpireBefore(t *testing.T) {
	now := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		unit string
		val  int
		want time.Time
	}{
		{"day", 3, time.Date(2024, 3, 12, 12, 0, 0, 0, time.UTC)},
		{"month", 1, time.Date(2024, 2, 15, 12, 0, 0, 0, time.UTC)},
		{"year", 1, time.Date(2023, 3, 15, 12, 0, 0, 0, time.UTC)},
		{"hour", 1, now},
	}
	for _, tc := range cases {
		got := modelc.TTL{Unit: tc.unit, Value: tc.val}.ExpireBefore(now)
		if !got.Equal(tc.want) {
			t.Fatalf("%s 过期时间 = %v，期望 %v", tc.unit, got, tc.want)
		}
	}
}

// 预创建分区：日/月粒度必须给出连续区间，其它粒度交给引擎。
func TestUpcomingPartitions(t *testing.T) {
	from := time.Date(2024, 1, 30, 0, 0, 0, 0, time.UTC)
	if got := modelc.UpcomingPartitions(modelc.PartitionSpec{Kind: "day"}, from, 0); got != nil {
		t.Fatalf("days<=0 应返回 nil: %v", got)
	}
	day := modelc.UpcomingPartitions(modelc.PartitionSpec{Kind: "day"}, from, 2)
	if len(day) != 3 {
		t.Fatalf("日分区数错误: %+v", day)
	}
	first := day[0]
	if first.Name != "p_20240130" || first.From != "2024-01-30" || first.To != "2024-01-31" || first.Value != "2024-01-30" {
		t.Fatalf("首个日分区错误: %+v", first)
	}
	month := modelc.UpcomingPartitions(modelc.PartitionSpec{Kind: "month"}, from, 45)
	// days/30+1 = 2 → 生成 3 个月（含当月）。
	if len(month) != 3 || month[0].Name != "p_202401" || month[0].From != "2024-01-01" ||
		month[0].To != "2024-02-01" || month[0].Value != "2024-01" {
		t.Fatalf("月分区错误: %+v", month)
	}
	if got := modelc.UpcomingPartitions(modelc.PartitionSpec{Kind: "hour"}, from, 3); len(got) != 0 {
		t.Fatalf("其它粒度不应生成分区: %+v", got)
	}
}

func TestCreatePartitionDDL(t *testing.T) {
	p := modelc.PartitionRange{Name: "p_202401", From: "2024-01-01", To: "2024-02-01"}
	got := modelc.CreatePartitionDDL("dwd", "dwd_order", p, "dt", dialect.Postgres{})
	want := `CREATE TABLE IF NOT EXISTS "dwd"."p_202401" PARTITION OF "dwd"."dwd_order" FOR VALUES FROM ('2024-01-01') TO ('2024-02-01');`
	if got != want {
		t.Fatalf("分区 DDL 错误:\n%s", got)
	}
	// 字面量必须转义单引号，否则就是注入/语法错误。
	got = modelc.CreatePartitionDDL("", "t", modelc.PartitionRange{Name: "p", From: "a'b", To: "c"}, "dt", dialect.Postgres{})
	if !strings.Contains(got, "'a''b'") {
		t.Fatalf("字面量转义错误: %s", got)
	}
}

// 存储校验的每条规则都对应一次真实事故风险。
func TestValidateStorage(t *testing.T) {
	spec := baseSpec()
	if got := modelc.ValidateStorage(spec); len(got) != 0 {
		t.Fatalf("合法存储配置不应有违规: %+v", got)
	}

	cases := []struct {
		name string
		spec model.ModelSpec
		want string
	}{
		{"表达式非法", model.ModelSpec{PartitionBy: []string{"a b"}}, "partition_expr"},
		{"列不存在", model.ModelSpec{PartitionBy: []string{"nope"}}, "partition_column"},
		{"列可空", model.ModelSpec{PartitionBy: []string{"dt"},
			Columns: []model.ColumnSpec{{Name: "dt", Type: "date", Nullable: true}}}, "partition_nullable"},
		{"粒度过细", model.ModelSpec{PartitionBy: []string{"date_trunc('hour', dt)"},
			Columns: []model.ColumnSpec{{Name: "dt", Type: "datetime"}}}, "partition_too_fine"},
		{"TTL 非法", model.ModelSpec{TTL: "10w"}, "ttl_format"},
		{"TTL 过短", model.ModelSpec{TTL: "3d", Columns: []model.ColumnSpec{{Name: "dt"}}}, "ttl_too_short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := modelc.ValidateStorage(tc.spec)
			if len(got) == 0 || got[0].Rule != tc.want {
				t.Fatalf("期望 %s，实际 %+v", tc.want, got)
			}
		})
	}
}

func TestValidMat(t *testing.T) {
	for _, m := range []string{"", "table", "view", "mv", "incremental"} {
		if !modelc.ValidMat(m) {
			t.Fatalf("%q 应合法", m)
		}
	}
	for _, m := range []string{"stream", "TABLE"} {
		if modelc.ValidMat(m) {
			t.Fatalf("%q 应非法", m)
		}
	}
}

// 物化方式：不支持的引擎必须报错并给替代方案，不能静默降级。
func TestCompileMaterialization(t *testing.T) {
	caps := modelc.MaterializationCaps{SupportsMV: true}
	pg := dialect.Postgres{}

	table := baseSpec()
	if got, err := modelc.CompileMaterialization(table, pg, caps); err != nil || got != "TABLE" {
		t.Fatalf("table 结果错误: %q %v", got, err)
	}

	noPart := baseSpec()
	noPart.PartitionBy = nil
	noPart.Materialization = "incremental"
	if _, err := modelc.CompileMaterialization(noPart, pg, caps); err == nil {
		t.Fatal("incremental 缺分区应报错")
	}
	inc := baseSpec()
	inc.Materialization = "incremental"
	if got, err := modelc.CompileMaterialization(inc, pg, caps); err != nil || got != "TABLE" {
		t.Fatalf("incremental 结果错误: %q %v", got, err)
	}

	view := baseSpec()
	view.Materialization = "view"
	view.PartitionBy = nil
	view.Query = "select 1"
	if got, err := modelc.CompileMaterialization(view, pg, caps); err != nil || got != "VIEW" {
		t.Fatalf("view 结果错误: %q %v", got, err)
	}
	viewPart := view
	viewPart.PartitionBy = []string{"dt"}
	if _, err := modelc.CompileMaterialization(viewPart, pg, caps); err == nil {
		t.Fatal("view 带分区应报错")
	}
	viewTTL := view
	viewTTL.TTL = "30d"
	if _, err := modelc.CompileMaterialization(viewTTL, pg, caps); err == nil {
		t.Fatal("view 带 TTL 应报错")
	}
	viewNoQuery := model.ModelSpec{Materialization: "view"}
	if _, err := modelc.CompileMaterialization(viewNoQuery, pg, caps); err == nil {
		t.Fatal("view 缺 query 应报错")
	}

	mv := baseSpec()
	mv.Materialization = "mv"
	mv.Query = "select 1"
	if got, err := modelc.CompileMaterialization(mv, dialect.MySQL{}, modelc.MaterializationCaps{}); err == nil || got != "" {
		t.Fatal("MySQL 不支持 MV 应报错")
	} else if err.(*errs.AppError).Meta["suggestion"] == nil {
		t.Fatalf("错误应带替代方案: %+v", err)
	}
	noQuery := mv
	noQuery.Query = " "
	if _, err := modelc.CompileMaterialization(noQuery, pg, caps); err == nil {
		t.Fatal("mv 缺 query 应报错")
	}
	if got, err := modelc.CompileMaterialization(mv, pg, caps); err != nil || got != "MATERIALIZED VIEW" {
		t.Fatalf("mv 结果错误: %q %v", got, err)
	}
	if _, err := modelc.CompileMaterialization(model.ModelSpec{Materialization: "stream"}, pg, caps); err == nil {
		t.Fatal("未知物化方式应报错")
	}
}

// 主键回退与列顺序：写入去重、断点重跑都依赖这两个口径。
func TestKeysAndColumns(t *testing.T) {
	spec := baseSpec()
	if got := modelc.Keys(spec); len(got) != 1 || got[0] != "id" {
		t.Fatalf("主键错误: %v", got)
	}
	spec.PrimaryKey = nil
	spec.UniqueKey = []string{"id", "dt"}
	if got := modelc.Keys(spec); len(got) != 2 {
		t.Fatalf("应回退唯一键: %v", got)
	}
	spec.UniqueKey = nil
	if got := modelc.Keys(spec); got != nil {
		t.Fatalf("无键应返回 nil: %v", got)
	}
	if got := modelc.SortedColumnNames(baseSpec()); strings.Join(got, ",") != "id,amount,dt" {
		t.Fatalf("列顺序错误: %v", got)
	}
}

// 批次估算：过大压垮库、过小拖慢同步，边界必须夹紧。
func TestBatchSizing(t *testing.T) {
	cases := map[int64]int{0: 5000, -1: 5000, 1000: 500, 6000: 600, 10000: 1000, 1000000: 5000}
	for rows, want := range cases {
		if got := modelc.BatchSizing(baseSpec(), rows); got != want {
			t.Fatalf("BatchSizing(%d) = %d，期望 %d", rows, got, want)
		}
	}
}

// 违规排序要稳定，否则快照测试天天红。
func TestSortViolations(t *testing.T) {
	in := []modelc.RuleViolation{
		{Rule: "b", Field: "x"},
		{Rule: "a", Field: "z"},
		{Rule: "a", Field: "a"},
	}
	got := modelc.SortViolations(in)
	if got[0].Rule != "a" || got[0].Field != "a" || got[1].Field != "z" || got[2].Rule != "b" {
		t.Fatalf("排序错误: %+v", got)
	}
}
