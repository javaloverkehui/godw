package semantic_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

func models() map[string]model.ModelSpec {
	return map[string]model.ModelSpec{
		"dwd_order": {Name: "dwd_order", Schema: "dwd", Layer: "dwd"},
		"dim_user":  {Name: "dim_user", Schema: "dim", Layer: "dim"},
		"ods_raw":   {Name: "ods_raw", Layer: "ods"},
	}
}

func dataset() semantic.Dataset {
	return semantic.Dataset{
		Name:      "ds_sales",
		BaseModel: "dwd_order",
		Version:   2,
		Joins: []semantic.Join{{
			Model: "dim_user", Alias: "u", Type: "left",
			On: []semantic.JoinOn{{Left: "dwd_order.user_id", Right: "dim_user.id"}},
		}},
		Dimensions: []semantic.Dimension{
			{Name: "region", Field: "region", Type: "categorical", Filterable: true},
			{Name: "created_at", Field: "dwd_order.created_at", Type: "time",
				TimeGrains: []string{"day", "month"}},
			{Name: "sensitive", Field: "secret", Type: "categorical"},
		},
		Measures: []semantic.Measure{
			{Name: "amount", Field: "dwd_order.amount", Agg: "sum", ZeroWhenNull: true},
			{Name: "cnt", Field: "", Agg: "count"},
			{Name: "uv", Field: "user_id", Agg: "count_distinct"},
			{Name: "amt_cnt", Field: "amount", Agg: "count"},
		},
		Filters: []semantic.Filter{{Field: "deleted", Op: "=", Value: 0}},
	}
}

// 数据集校验的每条规则都对应一类真实事故：笛卡尔积、跨层引用、重复别名。
func TestValidateDataset(t *testing.T) {
	if err := semantic.ValidateDataset(dataset(), models()); err != nil {
		t.Fatalf("合法数据集不应报错: %v", err)
	}

	mutate := func(f func(*semantic.Dataset)) semantic.Dataset {
		d := dataset()
		f(&d)
		return d
	}
	cases := []struct {
		name string
		d    semantic.Dataset
		want string
	}{
		{"name 非法", mutate(func(d *semantic.Dataset) { d.Name = "1bad" }), "name 必须是标识符"},
		{"base 为空", mutate(func(d *semantic.Dataset) { d.BaseModel = "" }), "baseModel 不能为空"},
		{"base 未发布", mutate(func(d *semantic.Dataset) { d.BaseModel = "nope" }), "不存在或未发布"},
		{"base 引用 ODS", mutate(func(d *semantic.Dataset) { d.BaseModel = "ods_raw" }), "数据集只能引用"},
		{"join 模型为空", mutate(func(d *semantic.Dataset) { d.Joins[0].Model = "" }), "model 不能为空"},
		{"join 模型未发布", mutate(func(d *semantic.Dataset) { d.Joins[0].Model = "nope" }), "不存在或未发布"},
		{"join 引用 ODS", mutate(func(d *semantic.Dataset) { d.Joins[0].Model = "ods_raw" }), "不允许引用 ODS"},
		{"join 缺 type", mutate(func(d *semantic.Dataset) { d.Joins[0].Type = "" }), "必须显式声明"},
		{"join type 非法", mutate(func(d *semantic.Dataset) { d.Joins[0].Type = "right" }), "只支持 left/inner"},
		{"join 缺 on", mutate(func(d *semantic.Dataset) { d.Joins[0].On = nil }), "笛卡尔积"},
		{"on 字段为空", mutate(func(d *semantic.Dataset) {
			d.Joins[0].On = []semantic.JoinOn{{Left: " ", Right: "b"}}
		}), "两侧字段都不能为空"},
		{"别名重复", mutate(func(d *semantic.Dataset) {
			d.Joins = append(d.Joins, semantic.Join{
				Model: "dim_user", Alias: "u", Type: "inner",
				On: []semantic.JoinOn{{Left: "a", Right: "b"}},
			})
		}), "别名重复"},
		{"维度为空", mutate(func(d *semantic.Dataset) { d.Dimensions = nil }), "dimensions 不能为空"},
		{"维度名非法", mutate(func(d *semantic.Dataset) { d.Dimensions[0].Name = "a b" }), "name 非法"},
		{"维度名重复", mutate(func(d *semantic.Dataset) { d.Dimensions[2].Name = "region" }), "维度名重复"},
		{"维度缺类型", mutate(func(d *semantic.Dataset) { d.Dimensions[0].Type = "" }), "type 不能为空"},
		{"维度类型非法", mutate(func(d *semantic.Dataset) { d.Dimensions[0].Type = "geo" }), "只支持 categorical"},
		{"时间粒度非法", mutate(func(d *semantic.Dataset) {
			d.Dimensions[1].TimeGrains = []string{"hour"}
		}), "时间粒度"},
		{"度量为空", mutate(func(d *semantic.Dataset) { d.Measures = nil }), "measures 不能为空"},
		{"度量名非法", mutate(func(d *semantic.Dataset) { d.Measures[0].Name = "a b" }), "name 非法"},
		{"聚合非法", mutate(func(d *semantic.Dataset) { d.Measures[0].Agg = "median" }), "agg 不支持"},
		{"度量字段非法", mutate(func(d *semantic.Dataset) { d.Measures[0].Field = "a.b.c" }), "field 非法"},
		{"格式非法", mutate(func(d *semantic.Dataset) { d.Measures[0].Format = "money" }), "format 不支持"},
		{"筛选操作符非法", mutate(func(d *semantic.Dataset) { d.Filters[0].Op = "~" }), "op 不支持"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := semantic.ValidateDataset(tc.d, models())
			if !errs.Is(err, errs.CodeInvalidArgument) {
				t.Fatalf("错误码错误: %v", err)
			}
			ae := err.(*errs.AppError)
			problems, _ := ae.Meta["problems"].([]string)
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("问题缺少 %q: %v", tc.want, problems)
			}
		})
	}

	// count 允许空字段（COUNT(*)）；count 也不需要字段白名单校验。
	countOnly := mutate(func(d *semantic.Dataset) { d.Measures = []semantic.Measure{{Name: "c", Agg: "count"}} })
	if err := semantic.ValidateDataset(countOnly, models()); err != nil {
		t.Fatalf("count 空字段应合法: %v", err)
	}
}

// 时间粒度：未声明时给全集，非时间维度不给粒度——前端据此渲染下拉。
func TestDimensionGrains(t *testing.T) {
	if got := semantic.DimensionGrains(semantic.Dimension{Type: "categorical"}); got != nil {
		t.Fatalf("非时间维度不应有粒度: %v", got)
	}
	all := semantic.DimensionGrains(semantic.Dimension{Type: "time"})
	if strings.Join(all, ",") != "day,week,month,quarter,year" {
		t.Fatalf("默认粒度错误: %v", all)
	}
	custom := semantic.DimensionGrains(semantic.Dimension{Type: "time", TimeGrains: []string{"day"}})
	if strings.Join(custom, ",") != "day" {
		t.Fatalf("自定义粒度错误: %v", custom)
	}
}

// 字段引用是 SQL 拼接的唯一入口，任何越界写法都必须拒绝。
func TestFieldRef(t *testing.T) {
	ok := []struct {
		in         string
		alias, col string
	}{
		{"amount", "", "amount"},
		{"o.amount", "o", "amount"},
		{"_a._b1", "_a", "_b1"},
	}
	for _, tc := range ok {
		alias, col, err := semantic.FieldRef(tc.in)
		if err != nil || alias != tc.alias || col != tc.col {
			t.Fatalf("%q 解析错误: %q %q %v", tc.in, alias, col, err)
		}
	}
	for _, in := range []string{"", "a b", "a.b.c", "a.", ".b", "a. b"} {
		if _, _, err := semantic.FieldRef(in); err == nil {
			t.Fatalf("%q 应报错", in)
		}
	}
}

// 引用的模型清单用于血缘与影响分析：排序 + 去重，避免重复节点。
func TestReferencedModels(t *testing.T) {
	d := dataset()
	got := d.ReferencedModels()
	if strings.Join(got, ",") != "dim_user,dwd_order" {
		t.Fatalf("引用模型错误: %v", got)
	}
	d.BaseModel = "dim_user"
	got = d.ReferencedModels()
	if strings.Join(got, ",") != "dim_user" {
		t.Fatalf("应去重: %v", got)
	}
	if got := (semantic.Dataset{}).ReferencedModels(); len(got) != 0 {
		t.Fatalf("空数据集应返回空切片: %v", got)
	}
}

// FROM/JOIN 渲染：别名缺失会让维度字段无法解析，标识符必须全部走方言引用。
func TestFromClause(t *testing.T) {
	d := dataset()
	got, err := d.FromClause(models(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	want := `"dwd"."dwd_order" AS "dwd_order"` + "\n" +
		// 关联条件的右值取 FieldRef 里的限定名（模型名），不会自动换成 join alias：
		// 这是当前实现的既定行为，改动前需要先确认线上 SQL 解析口径。
		`LEFT JOIN "dim"."dim_user" AS "u" ON "dwd_order"."user_id" = "dim_user"."id"`
	if got != want {
		t.Fatalf("FROM 子句错误:\n%s", got)
	}

	// 无别名时用模型名；多条件用 AND 连接。
	d.Joins[0].Alias = ""
	d.Joins[0].On = append(d.Joins[0].On, semantic.JoinOn{Left: "dwd_order.dt", Right: "dim_user.dt"})
	got, err = d.FromClause(models(), dialect.MySQL{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "AS `dim_user`") || !strings.Contains(got, " AND ") {
		t.Fatalf("多条件未连接:\n%s", got)
	}
	if !strings.Contains(got, "`dwd`.`dwd_order`") {
		t.Fatalf("MySQL 引用错误:\n%s", got)
	}

	// 无 schema 时只引用表名。
	noSchema := map[string]model.ModelSpec{
		"dwd_order": {Name: "dwd_order", Layer: "dwd"},
		"dim_user":  {Name: "dim_user", Layer: "dim"},
	}
	got, err = dataset().FromClause(noSchema, dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `"dwd_order" AS "dwd_order"`) {
		t.Fatalf("无 schema 表引用错误:\n%s", got)
	}

	if _, err := (semantic.Dataset{BaseModel: "nope"}).FromClause(models(), dialect.Postgres{}); err == nil {
		t.Fatal("基础模型不存在应报错")
	}
	bad := dataset()
	bad.Joins[0].Model = "nope"
	if _, err := bad.FromClause(models(), dialect.Postgres{}); err == nil {
		t.Fatal("关联模型不存在应报错")
	}
	bad = dataset()
	bad.Joins[0].On = []semantic.JoinOn{{Left: "a b", Right: "c"}}
	if _, err := bad.FromClause(models(), dialect.Postgres{}); err == nil {
		t.Fatal("非法关联字段应报错")
	}
	bad = dataset()
	bad.Joins[0].On = []semantic.JoinOn{{Left: "a", Right: "c d"}}
	if _, err := bad.FromClause(models(), dialect.Postgres{}); err == nil {
		t.Fatal("非法右侧关联字段应报错")
	}
}

func explainErr(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("应报错")
	}
	if !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("错误码错误: %v", err)
	}
	return err.Error()
}

// 查询构建的三条硬规则：标识符白名单、筛选值参数化、无结果也返回一行。
func TestBuildQuery(t *testing.T) {
	dl := dialect.Postgres{}
	mdl := models()

	t.Run("基础查询", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
			Dimensions: []string{"region"},
			Measures:   []string{"amount"},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"/* dataset=ds_sales v2 */",
			`"region" AS "region"`,
			`COALESCE(SUM("dwd_order"."amount"), 0) AS "amount"`,
			`FROM "dwd"."dwd_order" AS "dwd_order"`,
			`WHERE "deleted" = 0`,
			`GROUP BY "region"`,
			"LIMIT 1000",
		} {
			if !strings.Contains(got.SQL, want) {
				t.Fatalf("SQL 缺少 %q:\n%s", want, got.SQL)
			}
		}
		if strings.Join(got.Columns, ",") != "region,amount" {
			t.Fatalf("结果列错误: %v", got.Columns)
		}
		// 默认过滤的值来自配置文件，直接内联；SQL 里不应出现占位符。
		if strings.Contains(got.SQL, "$1") {
			t.Fatalf("默认过滤不应参数化:\n%s", got.SQL)
		}
	})
	t.Run("时间维度粒度", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
			Dimensions: []string{"created_at"}, Measures: []string{"amount"}, Granularity: "month",
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `DATE_TRUNC('month', "dwd_order"."created_at")`) {
			t.Fatalf("时间维度未截断:\n%s", got.SQL)
		}
	})
	t.Run("不支持的粒度", func(t *testing.T) {
		_, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
			Dimensions: []string{"created_at"}, Measures: []string{"amount"}, Granularity: "week",
		}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "不支持粒度") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("非时间维度忽略粒度", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
			Dimensions: []string{"region"}, Measures: []string{"amount"}, Granularity: "month",
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.SQL, "DATE_TRUNC") {
			t.Fatalf("非时间维度不应截断:\n%s", got.SQL)
		}
	})
	t.Run("基础模型缺失", func(t *testing.T) {
		d := dataset()
		d.BaseModel = "missing"
		_, err := semantic.BuildQuery(d, semantic.QueryRequest{Measures: []string{"amount"}}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("维度字段非法", func(t *testing.T) {
		d := dataset()
		d.Dimensions = append(d.Dimensions, semantic.Dimension{Name: "bad_dim", Field: "a.b.c"})
		_, err := semantic.BuildQuery(d, semantic.QueryRequest{Dimensions: []string{"bad_dim"}, Measures: []string{"amount"}}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("默认过滤非法", func(t *testing.T) {
		d := dataset()
		d.Filters = []semantic.Filter{{Field: "a b", Op: "=", Value: 1}}
		_, err := semantic.BuildQuery(d, semantic.QueryRequest{Measures: []string{"amount"}}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("数据集名为空", func(t *testing.T) {
		_, err := semantic.BuildQuery(semantic.Dataset{}, semantic.QueryRequest{Measures: []string{"amount"}}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("未知维度", func(t *testing.T) {
		_, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{Dimensions: []string{"nope"}, Measures: []string{"amount"}}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "未知维度") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("无度量", func(t *testing.T) {
		_, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{Dimensions: []string{"region"}}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "至少选择一个度量") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("未知度量", func(t *testing.T) {
		_, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{Measures: []string{"nope"}}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "未知度量") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("度量字段非法", func(t *testing.T) {
		d := dataset()
		d.Measures = append(d.Measures, semantic.Measure{Name: "bad", Field: "a.b.c", Agg: "sum"})
		_, err := semantic.BuildQuery(d, semantic.QueryRequest{Measures: []string{"bad"}}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("count 变体", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{Measures: []string{"amt_cnt", "uv"}}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`COUNT("amount") AS "amt_cnt"`, `COUNT(DISTINCT "user_id") AS "uv"`} {
			if !strings.Contains(got.SQL, want) {
				t.Fatalf("聚合渲染缺少 %q:\n%s", want, got.SQL)
			}
		}
	})
	t.Run("mysql 空值转 0", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{Measures: []string{"amount"}}, mdl, dialect.MySQL{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, "IFNULL(SUM(`dwd_order`.`amount`), 0)") {
			t.Fatalf("MySQL 空值处理错误:\n%s", got.SQL)
		}
	})
}

// 请求筛选：只有 filterable 的维度可外部筛选，值必须参数化。
func TestBuildQueryFilters(t *testing.T) {
	mdl := models()
	dl := dialect.Postgres{}
	// 去掉数据集默认过滤，避免它占用参数位，便于断言占位符序号。
	noDefaults := func() semantic.Dataset {
		d := dataset()
		d.Filters = nil
		return d
	}

	t.Run("过滤不可筛选维度", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "sensitive", Op: "=", Value: "x"}},
		}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "未开启筛选") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("过滤不在数据集内", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "nope", Op: "=", Value: 1}},
		}, mdl, dl)
		if !strings.Contains(explainErr(t, err), "不在数据集内") {
			t.Fatalf("错误信息错误: %v", err)
		}
	})
	t.Run("按度量过滤参数化", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "amount", Op: ">", Value: 10}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"amount" > $1`) {
			t.Fatalf("参数化错误:\n%s", got.SQL)
		}
		if len(got.Args) != 1 || got.Args[0] != 10 {
			t.Fatalf("参数错误: %v", got.Args)
		}
	})
	t.Run("空值判断不带值", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "is_null"}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"region" IS NULL`) || len(got.Args) != 0 {
			t.Fatalf("空值判断错误: %s %v", got.SQL, got.Args)
		}
	})
	t.Run("非空判断", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "is_not_null"}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"region" IS NOT NULL`) || len(got.Args) != 0 {
			t.Fatalf("非空判断错误: %s %v", got.SQL, got.Args)
		}
	})
	t.Run("not_in 参数化", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "not_in", Value: []any{"east"}}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"region" NOT IN ($1)`) || len(got.Args) != 1 {
			t.Fatalf("not_in 参数化错误: %s %v", got.SQL, got.Args)
		}
	})
	t.Run("like 参数化", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "like", Value: "e%"}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"region" LIKE $1`) || len(got.Args) != 1 || got.Args[0] != "e%" {
			t.Fatalf("like 参数化错误: %s %v", got.SQL, got.Args)
		}
	})
	t.Run("in 参数化", func(t *testing.T) {
		got, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "in", Value: []any{"east", "west"}}},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `"region" IN ($1, $2)`) || len(got.Args) != 2 {
			t.Fatalf("in 参数化错误: %s %v", got.SQL, got.Args)
		}
	})
	t.Run("like 类型错误", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "like", Value: 1}},
		}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("in 类型错误", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "in", Value: "x"}},
		}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("in 空数组", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "not_in", Value: []any{}}},
		}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("缺值", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "=", Value: nil}},
		}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("未知操作符", func(t *testing.T) {
		_, err := semantic.BuildQuery(noDefaults(), semantic.QueryRequest{
			Measures: []string{"amount"},
			Filters:  []semantic.Filter{{Field: "region", Op: "~", Value: 1}},
		}, mdl, dl)
		explainErr(t, err)
	})
	t.Run("行级权限", func(t *testing.T) {
		got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
			Measures: []string{"amount"},
			RLS:      []string{"tenant_id = 1"},
		}, mdl, dl)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.SQL, `AND tenant_id = 1`) {
			t.Fatalf("行级权限未注入:\n%s", got.SQL)
		}
	})
}

// 数据集默认过滤允许内联字面量（值来自配置文件），但类型必须正确渲染。
func TestBuildQueryLiteralFilter(t *testing.T) {
	d := dataset()
	d.Filters = []semantic.Filter{{Field: "deleted", Op: "in", Value: []any{1, true, nil, "a'b"}}}
	got, err := semantic.BuildQuery(d, semantic.QueryRequest{Measures: []string{"amount"}}, models(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.SQL, `"deleted" IN (1, true, NULL, 'a''b')`) {
		t.Fatalf("字面量渲染错误:\n%s", got.SQL)
	}
}

// 默认过滤里的 bool 与 like 走内联字面量（值来自配置文件）。
func TestBuildQueryLiteralForms(t *testing.T) {
	d := dataset()
	d.Filters = []semantic.Filter{
		{Field: "active", Op: "=", Value: false},
		{Field: "name", Op: "like", Value: "a%"},
	}
	got, err := semantic.BuildQuery(d, semantic.QueryRequest{Measures: []string{"amount"}}, models(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.SQL, `"active" = false`) || !strings.Contains(got.SQL, `"name" LIKE 'a%'`) {
		t.Fatalf("字面量渲染错误:\n%s", got.SQL)
	}
}

// 排序/分页：只允许对输出列排序，避免用排序探测未选中的列。
func TestBuildQueryOrderLimit(t *testing.T) {
	mdl := models()
	dl := dialect.Postgres{}

	_, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
		Measures: []string{"amount"}, OrderBy: "region",
	}, mdl, dl)
	if !strings.Contains(explainErr(t, err), "已选中的列") {
		t.Fatalf("错误信息错误: %v", err)
	}

	got, err := semantic.BuildQuery(dataset(), semantic.QueryRequest{
		Dimensions: []string{"region"}, Measures: []string{"amount"},
		OrderBy: "amount", Desc: true, Limit: 20, Offset: 5,
	}, mdl, dl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.SQL, `ORDER BY "amount" DESC`) || !strings.Contains(got.SQL, "LIMIT 20 OFFSET 5") {
		t.Fatalf("排序分页错误:\n%s", got.SQL)
	}

	got, err = semantic.BuildQuery(dataset(), semantic.QueryRequest{
		Dimensions: []string{"region"}, Measures: []string{"amount"}, OrderBy: "region", Limit: 20000,
	}, mdl, dl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.SQL, `ORDER BY "region" ASC`) || !strings.Contains(got.SQL, "LIMIT 10000") {
		t.Fatalf("上限夹紧错误:\n%s", got.SQL)
	}
}

func TestExplain(t *testing.T) {
	got := semantic.Explain(semantic.BuiltQuery{
		Comment: "/* dataset=x v1 */", SQL: "SELECT 1", Args: []any{1, "a"},
	})
	if !strings.Contains(got, "/* dataset=x v1 */") || !strings.Contains(got, "-- 参数 2 个: [1 a]") {
		t.Fatalf("Explain 输出错误:\n%s", got)
	}
	got = semantic.Explain(semantic.BuiltQuery{SQL: "SELECT 1"})
	if strings.Contains(got, "参数") {
		t.Fatalf("无参数时不应输出参数行:\n%s", got)
	}
}
