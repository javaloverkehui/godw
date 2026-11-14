package semantic

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

func testModels() map[string]model.ModelSpec {
	return map[string]model.ModelSpec{
		"ods_order": {Name: "ods_order", Layer: "ods", Schema: "ods", Table: "order",
			Columns: []model.ColumnSpec{{Name: "order_id", Type: "bigint"}, {Name: "dt", Type: "date"}}},
		"dwd_order": {Name: "dwd_order", Layer: "dwd", Schema: "dwd",
			Columns: []model.ColumnSpec{
				{Name: "order_id", Type: "bigint"}, {Name: "user_id", Type: "bigint"},
				{Name: "amount", Type: "decimal"}, {Name: "dt", Type: "date"},
				{Name: "status", Type: "string"},
			}},
		"dim_user": {Name: "dim_user", Layer: "dim", Schema: "dim",
			Columns: []model.ColumnSpec{{Name: "user_id", Type: "bigint"}, {Name: "city", Type: "string"}}},
	}
}

func sampleDataset() Dataset {
	return Dataset{
		Name: "sales", BaseModel: "dwd_order", Version: 1,
		Joins: []Join{{
			Model: "dim_user", Type: "left",
			On: []JoinOn{{Left: "dwd_order.user_id", Right: "dim_user.user_id"}},
		}},
		Dimensions: []Dimension{
			{Name: "dt", Field: "dwd_order.dt", Type: "time", Filterable: true},
			{Name: "status", Field: "dwd_order.status", Type: "categorical", Filterable: true},
			{Name: "city", Field: "dim_user.city", Type: "categorical"},
		},
		Measures: []Measure{
			{Name: "gmv", Field: "dwd_order.amount", Agg: "sum", Format: "amount", ZeroWhenNull: true},
			{Name: "orders", Field: "dwd_order.order_id", Agg: "count_distinct"},
		},
	}
}

// T0701 验收：引用 ods 被拒；join 无 on 被拒；时间维度粒度齐全。
func TestDatasetRejectsODS(t *testing.T) {
	d := sampleDataset()
	d.BaseModel = "ods_order"
	err := ValidateDataset(d, testModels())
	if err == nil {
		t.Fatal("引用 ods 应被拒")
	}
	if !strings.Contains(err.Error(), "dwd/dws/ads/dim") {
		t.Fatalf("错误信息应说明允许的分层: %v", err)
	}
}

func TestDatasetRejectsJoinWithoutOn(t *testing.T) {
	d := sampleDataset()
	d.Joins[0].On = nil
	if err := ValidateDataset(d, testModels()); err == nil {
		t.Fatal("无 on 条件应被拒")
	} else if !strings.Contains(err.Error(), "笛卡尔积") {
		t.Fatalf("错误信息应指出笛卡尔积: %v", err)
	}
}

func TestDatasetRejectsBadJoinType(t *testing.T) {
	d := sampleDataset()
	d.Joins[0].Type = "right"
	if err := ValidateDataset(d, testModels()); err == nil {
		t.Fatal("right join 应被拒")
	}
	d.Joins[0].Type = ""
	if err := ValidateDataset(d, testModels()); err == nil {
		t.Fatal("未声明 join 类型应被拒")
	}
}

func TestDatasetTimeGrains(t *testing.T) {
	dim := Dimension{Name: "dt", Type: "time"}
	grains := DimensionGrains(dim)
	if len(grains) != 5 {
		t.Fatalf("时间维度应派生 5 种粒度，实际 %v", grains)
	}
	dim.TimeGrains = []string{"day", "week"}
	if len(DimensionGrains(dim)) != 2 {
		t.Fatal("显式粒度应生效")
	}
	// 非法粒度在校验时报错
	d := sampleDataset()
	d.Dimensions[0].TimeGrains = []string{"hour"}
	if err := ValidateDataset(d, testModels()); err == nil {
		t.Fatal("非法粒度应被拒")
	}
}

func TestDatasetPass(t *testing.T) {
	if err := ValidateDataset(sampleDataset(), testModels()); err != nil {
		t.Fatalf("合法数据集应通过: %v", err)
	}
}

// T0703 验收：快照正确；SQL 注入不生效；无 group by 聚合返回一行。
func TestBuildQueryBasic(t *testing.T) {
	q, err := BuildQuery(sampleDataset(), QueryRequest{
		Dimensions:  []string{"dt"},
		Measures:    []string{"gmv", "orders"},
		Granularity: "day",
	}, testModels(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.SQL, `DATE_TRUNC('day', "dwd_order"."dt") AS "dt"`) {
		t.Fatalf("时间维度未截断: %s", q.SQL)
	}
	if !strings.Contains(q.SQL, `SUM("dwd_order"."amount")`) {
		t.Fatalf("度量未编译: %s", q.SQL)
	}
	// ZeroWhenNull
	if !strings.Contains(q.SQL, "COALESCE") {
		t.Fatalf("零值处理缺失: %s", q.SQL)
	}
	if !strings.Contains(q.SQL, `LEFT JOIN "dim"."dim_user" AS "dim_user"`) {
		t.Fatalf("JOIN 未生成: %s", q.SQL)
	}
	if !strings.Contains(q.SQL, "GROUP BY") {
		t.Fatalf("缺少 GROUP BY: %s", q.SQL)
	}
	if !strings.Contains(q.Comment, "dataset=sales") {
		t.Fatalf("缺少来源注释: %s", q.Comment)
	}
}

func TestBuildQueryInjection(t *testing.T) {
	q, err := BuildQuery(sampleDataset(), QueryRequest{
		Dimensions: []string{"status"},
		Measures:   []string{"gmv"},
		Filters:    []Filter{{Field: "status", Op: "=", Value: "paid' OR 1=1 --"}},
	}, testModels(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.SQL, "OR 1=1") {
		t.Fatalf("注入值被直接拼进 SQL: %s", q.SQL)
	}
	if len(q.Args) != 1 || q.Args[0] != "paid' OR 1=1 --" {
		t.Fatalf("注入值应作为参数: %+v", q.Args)
	}
	if !strings.Contains(q.SQL, "$1") {
		t.Fatalf("应使用占位符: %s", q.SQL)
	}
}

func TestBuildQueryUnknownField(t *testing.T) {
	_, err := BuildQuery(sampleDataset(), QueryRequest{
		Dimensions: []string{"secret_col"}, Measures: []string{"gmv"},
	}, testModels(), dialect.Postgres{})
	if err == nil {
		t.Fatal("未知维度应报错")
	}
	_, err = BuildQuery(sampleDataset(), QueryRequest{
		Dimensions: []string{"status"}, Measures: []string{"gmv"},
		Filters: []Filter{{Field: "amount", Op: "=", Value: 1}},
	}, testModels(), dialect.Postgres{})
	if err == nil {
		t.Fatal("筛选未声明字段应报错")
	}
}

func TestBuildQueryNonFilterable(t *testing.T) {
	_, err := BuildQuery(sampleDataset(), QueryRequest{
		Dimensions: []string{"city"}, Measures: []string{"gmv"},
		Filters: []Filter{{Field: "city", Op: "=", Value: "SH"}},
	}, testModels(), dialect.Postgres{})
	if err == nil || !strings.Contains(err.Error(), "filterable") {
		t.Fatalf("未开启筛选的维度应被拒: %v", err)
	}
}

func TestBuildQueryOrderByWhitelist(t *testing.T) {
	_, err := BuildQuery(sampleDataset(), QueryRequest{
		Dimensions: []string{"status"}, Measures: []string{"gmv"}, OrderBy: "amount",
	}, testModels(), dialect.Postgres{})
	if err == nil {
		t.Fatal("orderBy 非选中列应报错")
	}
}

func TestBuildQueryMultiEngine(t *testing.T) {
	for _, d := range []dialect.Dialect{dialect.Postgres{}, dialect.MySQL{}, dialect.ClickHouse{}, dialect.DuckDB{}} {
		q, err := BuildQuery(sampleDataset(), QueryRequest{
			Dimensions: []string{"status"}, Measures: []string{"gmv"}, Limit: 10,
		}, testModels(), d)
		if err != nil {
			t.Fatalf("%s 构建失败: %v", d.Name(), err)
		}
		if !strings.Contains(q.SQL, "LIMIT 10") {
			t.Fatalf("%s LIMIT 异常: %s", d.Name(), q.SQL)
		}
	}
}

// T0707 验收：非法编码组合被拒；同一图表被两个看板引用时提示影响面。
func TestChartValidation(t *testing.T) {
	ds := sampleDataset()
	base := Chart{
		Name: "gmv_trend", Type: ChartLine, Dataset: "sales",
		Dimensions: []string{"dt"}, Measures: []string{"gmv"},
		Encoding: Encoding{X: "dt", Y: "gmv"},
	}
	if err := ValidateChart(base, &ds); err != nil {
		t.Fatalf("合法折线图应通过: %v", err)
	}

	// 饼图只能 1 维度
	bad := base
	bad.Type = ChartPie
	bad.Dimensions = []string{"dt", "status"}
	if err := ValidateChart(bad, &ds); err == nil {
		t.Fatal("饼图 2 维度应被拒")
	}

	// 指标卡不能有维度
	kpi := base
	kpi.Type = ChartKPI
	kpi.Encoding = Encoding{Y: "gmv"}
	if err := ValidateChart(kpi, &ds); err == nil {
		t.Fatal("指标卡带维度应被拒")
	}
	kpi.Dimensions = nil
	if err := ValidateChart(kpi, &ds); err != nil {
		t.Fatalf("合法指标卡应通过: %v", err)
	}

	// 字段不在数据集
	missing := base
	missing.Measures = []string{"nope"}
	if err := ValidateChart(missing, &ds); err == nil {
		t.Fatal("不存在的度量应被拒")
	}

	// 槽位冲突
	conflict := base
	conflict.Encoding = Encoding{X: "dt", Y: "dt"}
	if err := ValidateChart(conflict, &ds); err == nil || !strings.Contains(err.Error(), "同时用于") {
		t.Fatalf("字段重复占用槽位应被拒: %v", err)
	}

	// 堆叠必须指定 series
	stack := Chart{
		Name: "stack", Type: ChartStackBar, Dataset: "sales",
		Dimensions: []string{"dt", "status"}, Measures: []string{"gmv"},
		Encoding: Encoding{X: "dt", Y: "gmv"},
	}
	if err := ValidateChart(stack, &ds); err == nil {
		t.Fatal("堆叠图缺 series 应被拒")
	}
	stack.Encoding.Series = "status"
	if err := ValidateChart(stack, &ds); err != nil {
		t.Fatalf("合法堆叠图应通过: %v", err)
	}
}

func TestChartImpact(t *testing.T) {
	c := Chart{Name: "gmv_trend"}
	dashboards := []Dashboard{
		{Name: "经营日报", Cards: []Card{{Metric: "gmv_trend"}}},
		{Name: "财务周报", Cards: []Card{{Metric: "gmv_trend"}}},
		{Name: "其他", Cards: []Card{{Metric: "order_cnt"}}},
	}
	impact := c.Impact(dashboards)
	if len(impact) != 2 {
		t.Fatalf("影响面应为 2 个看板，实际 %v", impact)
	}
}

func TestChartToQuery(t *testing.T) {
	c := Chart{
		Name: "gmv_trend", Type: ChartLine, Dataset: "sales",
		Dimensions: []string{"dt"}, Measures: []string{"gmv"},
		Encoding: Encoding{X: "dt", Y: "gmv"},
	}
	q := c.ToQuery()
	if q.OrderBy != "dt" || q.Desc {
		t.Fatalf("折线图应按 x 升序: %+v", q)
	}
	c.Type = ChartBar
	q = c.ToQuery()
	if q.OrderBy != "gmv" || !q.Desc {
		t.Fatalf("柱状图应按度量降序: %+v", q)
	}
	c.Type = ChartPie
	if q = c.ToQuery(); q.Limit != 20 {
		t.Fatalf("饼图应限制扇区数: %+v", q)
	}
}

func TestChartSchema(t *testing.T) {
	s := ChartSchema()
	if s["type"] != "object" {
		t.Fatal("schema 结构异常")
	}
	props := s["properties"].(map[string]any)
	if _, ok := props["type"]; !ok {
		t.Fatal("schema 缺少 type 字段")
	}
}
