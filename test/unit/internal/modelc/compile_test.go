package modelc_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

func baseSpec() model.ModelSpec {
	return model.ModelSpec{
		Kind:            "Model",
		Name:            "dwd_order",
		Layer:           "dwd",
		Datasource:      "pg_main",
		Materialization: "table",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"},
			{Name: "amount", Type: "decimal(18,2)", Nullable: true},
			{Name: "dt", Type: "date"},
		},
		PrimaryKey:  []string{"id"},
		PartitionBy: []string{"dt"},
	}
}

func mustProblems(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	ae, ok := err.(*errs.AppError)
	if !ok {
		t.Fatalf("应为 AppError: %T", err)
	}
	list, _ := ae.Meta["problems"].([]string)
	return strings.Join(list, "\n")
}

// 模型校验的缺陷面：错误必须一次列全，且每类字段的非法值都要拦住。
// 漏掉一条的后果是「接口说合法、发布建表失败」。
func TestValidate(t *testing.T) {
	if err := modelc.Validate(baseSpec()); err != nil {
		t.Fatalf("合法模型不应报错: %v", err)
	}

	with := func(f func(*model.ModelSpec)) model.ModelSpec {
		s := baseSpec()
		f(&s)
		return s
	}

	cases := []struct {
		name string
		spec model.ModelSpec
		want string
	}{
		{"kind 非法", with(func(s *model.ModelSpec) { s.Kind = "Table" }), "kind 必须为 Model"},
		{"name 为空", with(func(s *model.ModelSpec) { s.Name = "" }), "name 不能为空"},
		{"name 非法", with(func(s *model.ModelSpec) { s.Name = "bad-name" }), "name 只能包含"},
		{"layer 非法", with(func(s *model.ModelSpec) { s.Layer = "mid" }), "layer 必须为"},
		{"datasource 为空", with(func(s *model.ModelSpec) { s.Datasource = "" }), "datasource 不能为空"},
		{"columns 为空", with(func(s *model.ModelSpec) { s.Columns = nil }), "columns 不能为空"},
		{"列名为空", with(func(s *model.ModelSpec) { s.Columns[0].Name = "" }), "columns[0].name 不能为空"},
		{"列名非法", with(func(s *model.ModelSpec) { s.Columns[0].Name = "1id" }), "列名 \"1id\" 非法"},
		{"列名重复忽略大小写", with(func(s *model.ModelSpec) {
			s.Columns[1].Name = "ID"
		}), "列名重复: ID"},
		{"列缺类型", with(func(s *model.ModelSpec) { s.Columns[0].Type = "" }), "列 id 缺少 type"},
		{"列类型不支持", with(func(s *model.ModelSpec) { s.Columns[0].Type = "blob" }), "不受支持"},
		{"主键列不存在", with(func(s *model.ModelSpec) { s.PrimaryKey = []string{"nope"} }), "primary_key 引用了不存在的列"},
		{"唯一键列不存在", with(func(s *model.ModelSpec) { s.UniqueKey = []string{"nope"} }), "unique_key 引用了不存在的列"},
		{"分区列不存在", with(func(s *model.ModelSpec) { s.PartitionBy = []string{"nope"} }), "partition_by 引用了不存在的列"},
		{"索引空列", with(func(s *model.ModelSpec) { s.Indexes = []model.IndexSpec{{}} }), "indexes 存在空列定义"},
		{"索引列不存在", with(func(s *model.ModelSpec) {
			s.Indexes = []model.IndexSpec{{Columns: []string{"nope"}}}
		}), "索引引用了不存在的列"},
		{"分桶列不存在", with(func(s *model.ModelSpec) { s.BucketBy = []string{"nope"} }), "bucket_by 引用了不存在的列"},
		{"分桶数为负", with(func(s *model.ModelSpec) { s.Buckets = -1 }), "buckets 不能为负数"},
		{"物化方式非法", with(func(s *model.ModelSpec) { s.Materialization = "stream" }), "materialization 必须为"},
		{"TTL 非法", with(func(s *model.ModelSpec) { s.TTL = "10w" }), "TTL 格式"},
		{"view 缺 query", with(func(s *model.ModelSpec) { s.Materialization = "view" }), "view 必须提供 query"},
		{"mv 缺 query", with(func(s *model.ModelSpec) { s.Materialization = "mv" }), "mv 必须提供 query"},
		{"分区列可空", with(func(s *model.ModelSpec) { s.Columns[2].Nullable = true }), "不允许为可空"},
		{"分区粒度过细", with(func(s *model.ModelSpec) {
			s.PartitionBy = []string{"date_trunc('hour', dt)"}
		}), "不得细于小时"},
		{"分区表达式非法", with(func(s *model.ModelSpec) {
			s.PartitionBy = []string{"date_trunc('minute', dt)"}
		}), "不支持的时间粒度"},
		{"非分区表 TTL 过短", with(func(s *model.ModelSpec) {
			s.PartitionBy = nil
			s.TTL = "3d"
		}), "TTL 小于 7 天"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := modelc.Validate(tc.spec)
			if err == nil {
				t.Fatal("应报错")
			}
			if !errs.Is(err, errs.CodeModelInvalid) {
				t.Fatalf("错误码错误: %v", err)
			}
			if got := mustProblems(t, err); !strings.Contains(got, tc.want) {
				t.Fatalf("问题列表缺少 %q:\n%s", tc.want, got)
			}
		})
	}
}

// 类型校验：语法错误、引擎不支持、有损映射三种情况必须区分，
// 且平台注入的审计列不走 allow_lossy 流程。
func TestValidateTypes(t *testing.T) {
	spec := baseSpec()
	spec.Columns = []model.ColumnSpec{
		{Name: "ok", Type: "decimal(18,2)"},
		{Name: "bad", Type: "array<foo>"},
		{Name: "dt", Type: "datetime"},
		{Name: "_godw_load_at", Type: "datetime"},
	}
	got := modelc.ValidateTypes(spec, "mysql")
	rules := map[string]int{}
	for _, v := range got {
		rules[v.Rule]++
	}
	if rules["type_syntax"] != 1 {
		t.Fatalf("语法错误应报 1 条: %+v", got)
	}
	// dt 与 _godw_load_at 都是 datetime：只有用户列要求确认有损。
	if rules["lossy_type"] != 1 {
		t.Fatalf("有损映射应只报用户列: %+v", got)
	}

	spec.AllowLossy = true
	for _, v := range modelc.ValidateTypes(spec, "mysql") {
		if v.Rule == "lossy_type" {
			t.Fatalf("allow_lossy 后不应再报有损: %+v", v)
		}
	}

	// 空类型在 ValidateTypes 里必须走语法错误分支（Validate 会先拦，但设计器可能直接调这里）。
	empty := modelc.ValidateTypes(model.ModelSpec{Columns: []model.ColumnSpec{{Name: "c"}}}, "postgres")
	if len(empty) != 1 || empty[0].Rule != "type_syntax" {
		t.Fatalf("空类型应报 type_syntax: %+v", empty)
	}

	// 未知引擎：MapType 报错，必须转成 type_unsupported。
	unsupported := modelc.ValidateTypes(baseSpec(), "oracle")
	if len(unsupported) == 0 || unsupported[0].Rule != "type_unsupported" {
		t.Fatalf("未知引擎应报 type_unsupported: %+v", unsupported)
	}
}

// 类型解析的每一种合法/非法写法都要走到，否则一个拼写错误会被当成合法。
func TestTypeSpecParsing(t *testing.T) {
	valid := []string{"string", "text", "int", "bigint", "decimal", "decimal(18,2)",
		"double", "bool", "date", "datetime", "json", "array<int>", "array<decimal(10,2)>", "map", "map<string>"}
	for _, typ := range valid {
		spec := baseSpec()
		spec.Columns = []model.ColumnSpec{{Name: "c", Type: typ}}
		spec.PrimaryKey, spec.PartitionBy = nil, nil
		if err := modelc.Validate(spec); err != nil {
			t.Fatalf("%q 应合法: %v", typ, err)
		}
	}
	invalid := []string{"decimal(18)", "decimal(0,2)", "decimal(18,-1)", "decimal(a,b)",
		"array<foo>", "blob", "varchar", "decimal(18,30)"}
	for _, typ := range invalid {
		spec := baseSpec()
		spec.Columns = []model.ColumnSpec{{Name: "c", Type: typ}}
		spec.PrimaryKey, spec.PartitionBy = nil, nil
		if err := modelc.Validate(spec); err == nil {
			t.Fatalf("%q 应非法", typ)
		}
	}
}

// 建表 SQL 是快照内容，任何措辞差异都代表历史重跑会漂移。
// 这里守住各方言的关键结构。
func TestCompile(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		spec := baseSpec()
		spec.Schema = "dwd"
		spec.Description = "订单明细"
		spec.Columns[0].Comment = "主键"
		spec.Columns[1].Default = "0"
		spec.Indexes = []model.IndexSpec{
			{Columns: []string{"dt"}},
			{Name: "ux", Columns: []string{"id", "dt"}, Unique: true},
		}
		ddl, err := modelc.Compile(spec, dialect.Postgres{})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`CREATE SCHEMA IF NOT EXISTS "dwd";`,
			`CREATE TABLE IF NOT EXISTS "dwd"."dwd_order" (`,
			`"id" bigint NOT NULL`,
			`"amount" numeric(18,2) DEFAULT 0`,
			`CONSTRAINT "pk_dwd_order" PRIMARY KEY ("id")`,
			`COMMENT ON TABLE "dwd"."dwd_order" IS '订单明细';`,
			`COMMENT ON COLUMN "dwd"."dwd_order"."id" IS '主键';`,
			`CREATE INDEX IF NOT EXISTS "idx_dwd_order_dt" ON "dwd"."dwd_order" ("dt");`,
			`CREATE UNIQUE INDEX IF NOT EXISTS "ux"`,
		} {
			if !strings.Contains(ddl, want) {
				t.Fatalf("DDL 缺少 %q:\n%s", want, ddl)
			}
		}
	})
	t.Run("duckdb", func(t *testing.T) {
		spec := baseSpec()
		spec.Schema = "dw"
		ddl, err := modelc.Compile(spec, dialect.DuckDB{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ddl, `CREATE SCHEMA IF NOT EXISTS "dw";`) {
			t.Fatalf("DuckDB 应建 schema:\n%s", ddl)
		}
	})
	t.Run("mysql 先建库", func(t *testing.T) {
		spec := baseSpec()
		spec.Schema = "dw"
		ddl, err := modelc.Compile(spec, dialect.MySQL{})
		if err != nil {
			t.Fatal(err)
		}
		// MySQL 的 SCHEMA 就是 DATABASE，但必须显式建：缺库时报 1049 Unknown database，
		// 而不是把 CREATE TABLE 落到某个默认库里。
		if !strings.Contains(ddl, "CREATE DATABASE IF NOT EXISTS `dw`;") {
			t.Fatalf("MySQL 应先建库:\n%s", ddl)
		}
		// CREATE SCHEMA 在 MySQL 只是同义词，但混用会让 DDL 可读性变差，统一用 DATABASE。
		if strings.Contains(ddl, "CREATE SCHEMA") {
			t.Fatalf("MySQL 不应出现 CREATE SCHEMA:\n%s", ddl)
		}
		if !strings.Contains(ddl, "`dw`.`dwd_order`") {
			t.Fatalf("MySQL 引用错误:\n%s", ddl)
		}
	})
	t.Run("mysql 无 schema 不建库", func(t *testing.T) {
		spec := baseSpec()
		spec.Schema = ""
		ddl, err := modelc.Compile(spec, dialect.MySQL{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "CREATE DATABASE") {
			t.Fatalf("未声明 schema 时不应建库:\n%s", ddl)
		}
	})
	t.Run("clickhouse", func(t *testing.T) {
		spec := baseSpec()
		spec.Schema = "dw"
		spec.TTL = "30d"
		spec.Columns[1].Comment = "金额"
		spec.UniqueKey = []string{"id"}
		spec.Indexes = []model.IndexSpec{{Columns: []string{"dt"}}}
		ddl, err := modelc.Compile(spec, dialect.ClickHouse{})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"CREATE DATABASE IF NOT EXISTS `dw`;",
			"ENGINE = MergeTree()",
			"ORDER BY (`id`)",
			"COMMENT '金额'",
			"PARTITION BY (`dt`)",
			"TTL `dt` + INTERVAL 30 DAY",
			"Nullable(Decimal(18,2))",
		} {
			if !strings.Contains(ddl, want) {
				t.Fatalf("ClickHouse DDL 缺少 %q:\n%s", want, ddl)
			}
		}
		if strings.Contains(ddl, "CREATE INDEX") {
			t.Fatalf("ClickHouse 不应建二级索引:\n%s", ddl)
		}
	})
	t.Run("clickhouse 无主键退回首列", func(t *testing.T) {
		spec := baseSpec()
		spec.PrimaryKey = nil
		spec.UniqueKey = nil
		ddl, err := modelc.Compile(spec, dialect.ClickHouse{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ddl, "ORDER BY (`id`)") {
			t.Fatalf("应回退到首列排序:\n%s", ddl)
		}
	})
	t.Run("clickhouse 无分区时忽略 TTL", func(t *testing.T) {
		spec := baseSpec()
		spec.PartitionBy = nil
		spec.TTL = "30d"
		ddl, err := modelc.Compile(spec, dialect.ClickHouse{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "TTL") {
			t.Fatalf("无分区不应生成 TTL:\n%s", ddl)
		}
	})
	t.Run("非法模型不产出 DDL", func(t *testing.T) {
		spec := baseSpec()
		spec.Name = ""
		if ddl, err := modelc.Compile(spec, dialect.Postgres{}); err == nil || ddl != "" {
			t.Fatalf("非法模型应报错: %q %v", ddl, err)
		}
	})
	t.Run("mysql 表达式分区报错", func(t *testing.T) {
		spec := baseSpec()
		spec.PartitionBy = []string{"date_trunc('month', dt)"}
		if _, err := modelc.Compile(spec, dialect.MySQL{}); !errs.Is(err, errs.CodeModelInvalid) {
			t.Fatalf("应报模型错误: %v", err)
		}
	})
	t.Run("ansi 不生成分区子句", func(t *testing.T) {
		spec := baseSpec()
		ddl, err := modelc.Compile(spec, dialect.ANSI{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "PARTITION BY") {
			t.Fatalf("ANSI 不应生成分区子句:\n%s", ddl)
		}
	})
}

// 多语句脚本按层排序：ODS 先建，ADS 最后，符合依赖直觉。
func TestCompileAll(t *testing.T) {
	ads := baseSpec()
	ads.Name, ads.Layer, ads.Schema = "ads_metric", "ads", ""
	ods := baseSpec()
	ods.Name, ods.Layer, ods.Schema = "ods_order", "ods", ""
	dwd := baseSpec()
	dwd.Name, dwd.Layer, dwd.Schema = "dwd_order", "dwd", ""
	out, err := modelc.CompileAll([]model.ModelSpec{ads, dwd, ods}, dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	iODS := strings.Index(out, "-- ods.ods_order")
	iDWD := strings.Index(out, "-- dwd.dwd_order")
	iADS := strings.Index(out, "-- ads.ads_metric")
	if !(iODS >= 0 && iDWD > iODS && iADS > iDWD) {
		t.Fatalf("脚本顺序错误:\n%s", out)
	}

	bad := baseSpec()
	bad.Name = ""
	if _, err := modelc.CompileAll([]model.ModelSpec{bad}, dialect.Postgres{}); err == nil {
		t.Fatal("非法模型应中断编译")
	}
}

// 设计器需要逐字段定位：结构化违规必须与 Validate 的判断一致。
func TestValidateViolations(t *testing.T) {
	if got := modelc.ValidateViolations(baseSpec()); got != nil {
		t.Fatalf("合法模型不应有违规: %+v", got)
	}
	spec := baseSpec()
	spec.Name = ""
	spec.Layer = "bad"
	got := modelc.ValidateViolations(spec)
	if len(got) < 2 {
		t.Fatalf("违规数错误: %+v", got)
	}
	for _, v := range got {
		if v.Rule != "schema" || v.Field != "spec" || v.Message == "" {
			t.Fatalf("违规结构错误: %+v", v)
		}
	}
}

// 数据字典是「契约快照」：排序、物理类型、分级与脱敏都必须稳定。
func TestBuildDictionary(t *testing.T) {
	ods := model.ModelSpec{Name: "ods_order", Layer: "ods", Schema: "ods",
		Columns: []model.ColumnSpec{{Name: "id", Type: "bigint"}, {Name: "raw", Type: "weird"}}}
	dwd := model.ModelSpec{Name: "dwd_order", Layer: "dwd", Schema: "dwd",
		Description: "订单", Owner: "alice", Tags: []string{"core"},
		Columns: []model.ColumnSpec{{Name: "id", Type: "bigint", Comment: "主键"}}}
	dws := model.ModelSpec{Name: "dws_sales", Layer: "dws", Schema: "dws",
		Columns: []model.ColumnSpec{{Name: "amt", Type: "decimal(10,2)", Nullable: true}}}
	in := modelc.DictInput{
		Specs:      []model.ModelSpec{dws, dwd, ods},
		Upstream:   map[string][]string{"dwd_order": {"ods_order"}},
		Downstream: map[string][]string{"ods_order": {"dwd_order"}},
		Policies: map[string]map[string]model.ColumnPolicy{
			"dwd_order": {"id": {Classification: "high", MaskType: "hash"}},
		},
		Engine: "postgres",
	}
	// 同层再放一张表，覆盖「同层按名称排序」的分支。
	in.Specs = append(in.Specs, model.ModelSpec{Name: "dwd_a", Layer: "dwd", Schema: "dwd",
		Columns: []model.ColumnSpec{{Name: "id", Type: "bigint"}}})
	entries := modelc.BuildDictionary(in)
	if len(entries) != 4 || entries[0].Layer != "ods" || entries[1].Name != "dwd_a" || entries[2].Layer != "dwd" {
		t.Fatalf("字典排序错误: %+v", entries)
	}
	dwdEntry := entries[2]
	if dwdEntry.Table != "dwd_order" || dwdEntry.Owner != "alice" || len(dwdEntry.Upstream) != 1 {
		t.Fatalf("字典元信息错误: %+v", dwdEntry)
	}
	if dwdEntry.Columns[0].Physical != "bigint" {
		t.Fatalf("物理类型映射错误: %+v", dwdEntry.Columns)
	}
	if dwdEntry.Columns[0].Classification != "high" || dwdEntry.Columns[0].Mask != "hash" {
		t.Fatalf("分级策略未写入: %+v", dwdEntry.Columns)
	}
	// 未知逻辑类型不能映射物理类型，但也不能让整份字典失败。
	if entries[0].Columns[1].Physical != "" {
		t.Fatalf("未知类型不应映射出物理类型: %+v", entries[0].Columns[1])
	}

	md := modelc.DictionaryMarkdown(entries)
	for _, want := range []string{
		"# 数据字典", "共 4 张表", "## 目录", "**ODS**（1）", "## DWD 层", "### dwd_order",
		"负责人 alice", "标签 core", "上游：ods_order", "下游：dwd_order",
		"| id | bigint | bigint | false | 主键 | high | hash |",
		"| amt | decimal(10,2) | numeric(10,2) | true | - | - | - |",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("Markdown 缺少 %q:\n%s", want, md)
		}
	}
	if got := modelc.DictionaryMarkdown(nil); !strings.Contains(got, "共 0 张表") {
		t.Fatalf("空字典输出错误:\n%s", got)
	}
}
