package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

func datasetSample(name, desc string) semantic.Dataset {
	return semantic.Dataset{
		Name:        name,
		Description: desc,
		BaseModel:   "dwd_trade",
		Dimensions:  []semantic.Dimension{{Name: "dt", Field: "dt", Type: "time"}},
		Measures:    []semantic.Measure{{Name: "gmv", Field: "amount", Agg: "sum"}},
	}
}

func chartSample(name, dataset string) semantic.Chart {
	return semantic.Chart{
		Name: name, Title: name, Type: semantic.ChartLine, Dataset: dataset,
		Dimensions: []string{"dt"}, Measures: []string{"gmv"},
		Encoding: semantic.Encoding{X: "dt", Y: "gmv"},
	}
}

// 语义数据集只保留最新版本，version 在每次保存时自增；
// 与物理模型不同，这里不保留历史快照：同名保存即覆盖，只留最新定义。
func TestSemanticDatasetUpsert(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	d, err := r.SaveDataset(ctx, ws.ID, datasetSample("trade", "交易数据集"))
	if err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	// 结构体里不带 workspace（调用方已经知道），只回填 ID。
	if d.ID == "" {
		t.Fatalf("首次保存应回填 ID: %+v", d)
	}

	// 同名再保存：复用 ID，不新增行。
	d2, err := r.SaveDataset(ctx, ws.ID, datasetSample("trade", "交易数据集 v2"))
	if err != nil {
		t.Fatalf("更新数据集失败: %v", err)
	}
	if d2.ID != d.ID {
		t.Fatalf("更新应复用 ID: %+v", d2)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM semantic_dataset WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("只应保留一行: %d", n)
	}

	got, err := r.GetDataset(ctx, ws.ID, "trade")
	if err != nil {
		t.Fatalf("取数据集失败: %v", err)
	}
	if got.Description != "交易数据集 v2" || got.BaseModel != "dwd_trade" {
		t.Fatalf("应返回最新定义: %+v", got)
	}
	if len(got.Dimensions) != 1 || got.Dimensions[0].Name != "dt" || len(got.Measures) != 1 {
		t.Fatalf("维度与度量应往返: %+v", got)
	}

	// 另一个数据集用来验证排序。
	if _, err := r.SaveDataset(ctx, ws.ID, datasetSample("alpha", "先建")); err != nil {
		t.Fatalf("保存第二个数据集失败: %v", err)
	}
	list, err := r.ListDatasetsScoped(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列数据集失败: %v", err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "trade" {
		t.Fatalf("应按名称排序: %+v", list)
	}
	if list[1].Description != "交易数据集 v2" {
		t.Fatalf("描述应为最新: %+v", list[1])
	}

	if _, err := r.GetDataset(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的数据集应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 跨工作区隔离。
	if _, err := r.GetDataset(ctx, other.ID, "trade"); err == nil {
		t.Fatal("跨工作区不应查到数据集")
	}
	if list, err := r.ListDatasetsScoped(ctx, other.ID); err != nil || len(list) != 0 {
		t.Fatalf("其他工作区应为空: %v %v", list, err)
	}

	// 删除：删不存在的名字不报错（幂等）。
	if err := r.DeleteDataset(ctx, ws.ID, "alpha"); err != nil {
		t.Fatalf("删除数据集失败: %v", err)
	}
	if err := r.DeleteDataset(ctx, ws.ID, "alpha"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if list, err := r.ListDatasetsScoped(ctx, ws.ID); err != nil || len(list) != 1 {
		t.Fatalf("删除后应剩 1 个: %v %v", list, err)
	}
}

// 图表同样只留最新版本；引用计数用于「删数据集前的影响检查」。
func TestSemanticChartAndReferenceCount(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if _, err := r.SaveDataset(ctx, ws.ID, datasetSample("trade", "交易数据集")); err != nil {
		t.Fatalf("准备数据集失败: %v", err)
	}

	c, err := r.SaveChart(ctx, ws.ID, chartSample("gmv_trend", "trade"))
	if err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if c.ID == "" {
		t.Fatalf("首次保存应回填 ID: %+v", c)
	}

	c2, err := r.SaveChart(ctx, ws.ID, chartSample("gmv_trend", "trade"))
	if err != nil {
		t.Fatalf("更新图表失败: %v", err)
	}
	if c2.ID != c.ID {
		t.Fatalf("更新应复用 ID: %+v", c2)
	}
	// 换数据集要生效（图表可能改挂到另一个数据集）。
	c3, err := r.SaveChart(ctx, ws.ID, chartSample("gmv_trend", "trade_v2"))
	if err != nil {
		t.Fatalf("改挂数据集失败: %v", err)
	}
	if c3.ID != c.ID {
		t.Fatalf("改挂数据集也应复用 ID: %+v", c3)
	}
	if ds := queryStr(t, db, `SELECT dataset FROM chart WHERE id=$1`, c.ID); ds != "trade_v2" {
		t.Fatalf("数据集应更新: %q", ds)
	}

	got, err := r.GetChart(ctx, ws.ID, "gmv_trend")
	if err != nil {
		t.Fatalf("取图表失败: %v", err)
	}
	if got.Dataset != "trade_v2" || got.Type != semantic.ChartLine || got.Title != "gmv_trend" {
		t.Fatalf("图表字段不对: %+v", got)
	}
	if got.Encoding.X != "dt" || got.Encoding.Y != "gmv" {
		t.Fatalf("编码应往返: %+v", got.Encoding)
	}

	// 另一个图表挂回 trade，用来验证排序与计数。
	if _, err := r.SaveChart(ctx, ws.ID, chartSample("a_kpi", "trade")); err != nil {
		t.Fatalf("保存第二个图表失败: %v", err)
	}
	charts, err := r.ListCharts(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列图表失败: %v", err)
	}
	if len(charts) != 2 || charts[0].Name != "a_kpi" || charts[1].Name != "gmv_trend" {
		t.Fatalf("应按名称排序: %+v", charts)
	}

	// 引用计数：trade 被 a_kpi 引用 1 次，trade_v2 被 gmv_trend 引用 1 次。
	if n, err := r.CountChartsUsingDataset(ctx, ws.ID, "trade"); err != nil || n != 1 {
		t.Fatalf("引用计数不对: %d %v", n, err)
	}
	if n, err := r.CountChartsUsingDataset(ctx, ws.ID, "trade_v2"); err != nil || n != 1 {
		t.Fatalf("引用计数不对: %d %v", n, err)
	}
	if n, err := r.CountChartsUsingDataset(ctx, ws.ID, "unused"); err != nil || n != 0 {
		t.Fatalf("未引用应为 0: %d %v", n, err)
	}
	if n, err := r.CountChartsUsingDataset(ctx, other.ID, "trade"); err != nil || n != 0 {
		t.Fatalf("跨工作区不应计数: %d %v", n, err)
	}

	if _, err := r.GetChart(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的图表应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	if list, err := r.ListCharts(ctx, other.ID); err != nil || len(list) != 0 {
		t.Fatalf("其他工作区应为空: %v %v", list, err)
	}

	// 删除：删不存在的名字不报错。
	if err := r.DeleteChart(ctx, ws.ID, "a_kpi"); err != nil {
		t.Fatalf("删除图表失败: %v", err)
	}
	if err := r.DeleteChart(ctx, ws.ID, "a_kpi"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if charts, err := r.ListCharts(ctx, ws.ID); err != nil || len(charts) != 1 {
		t.Fatalf("删除后应剩 1 个: %v %v", charts, err)
	}
	if n, err := r.CountChartsUsingDataset(ctx, ws.ID, "trade"); err != nil || n != 0 {
		t.Fatalf("删除后引用应归零: %d %v", n, err)
	}
}
