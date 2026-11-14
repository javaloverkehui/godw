package service_test

import (
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

// seedSemantic 建一张真表 + 模型 + 数据集，供语义层查询用例复用。
func seedSemantic(t *testing.T, svc *service.Service, wsID string) (dsName, table string) {
	t.Helper()
	ds := newPgDatasource(t, svc, wsID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_sem_t"))
	d1 := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	d2 := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	p.exec(t, `INSERT INTO %s VALUES ('%s',1,10,'alice'),('%s',2,20,'bob'),('%s',3,30,'alice')`,
		p.table("dwd_sem_t"), d1, d1, d2)
	publishModel(t, svc, wsID, modelSpec(ds.Name, "dwd_sem", p.schema, "dwd_sem_t"))
	if _, err := svc.ApplyDataset(ctxBG(), wsID, datasetFor("ds_sem", "dwd_sem", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	return ds.Name, "dwd_sem_t"
}

// 数据集只允许引用已发布的非 ODS 模型：这是「口径与性能」的第一道保护。
func TestApplyDatasetValidation(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_x", "ghost", "dt")); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未发布模型应被拒绝，实际 %v", err)
	}
	// ODS 层直接暴露给业务会让口径失控。
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "ods_raw", "ods"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_ods", "ods_raw", "dt")); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("ODS 模型应被拒绝，实际 %v", err)
	}

	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_ok", "dw", "dwd_ok"))
	saved, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_ok", "dwd_ok", "dt"))
	if err != nil {
		t.Fatalf("合法数据集保存失败: %v", err)
	}
	if saved.Version != 1 {
		t.Fatalf("首版版本号应为 1: %+v", saved)
	}
	// 重复保存版本递增，便于排查「口径是什么时候改的」。
	saved, err = svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_ok", "dwd_ok", "dt"))
	if err != nil || saved.Version != 2 {
		t.Fatalf("重复保存版本应递增: %+v %v", saved, err)
	}

	// PublishedModels 只返回已发布模型：草稿不应被语义层引用。
	if _, err := svc.Repo.SaveModel(ctxBG(), ws.ID, modelSpec(ds.Name, "dwd_draft", "dw", "dwd_draft"), "", "", "tester", false); err == nil {
		// SaveModel 需要真实 datasource_id，这里直接用 ApplyModelSpec 的 draft 路径。
	}
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "tester", modelSpec(ds.Name, "dwd_draft", "dw", "dwd_draft"), false, false, true); err != nil {
		t.Fatalf("保存草稿失败: %v", err)
	}
	models, err := svc.PublishedModels(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("PublishedModels 失败: %v", err)
	}
	if _, ok := models["dwd_draft"]; ok {
		t.Fatalf("草稿模型不应出现在已发布集合: %+v", models)
	}
	if _, ok := models["dwd_ok"]; !ok {
		t.Fatalf("已发布模型应出现: %+v", models)
	}
}

// 数据集查询要把行级策略注入 WHERE、按基表列策略脱敏，并给出可复现的 SQL。
func TestDatasetQueryWithRLSAndMask(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_sem_t", Role: "viewer", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_sem_t", ColumnName: "amount", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}

	res, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_sem", Dimensions: []string{"dt"}, Measures: []string{"amount"},
	}, "alice", "viewer")
	if err != nil {
		t.Fatalf("DatasetQuery 失败: %v", err)
	}
	rows, _ := res["rows"].([][]any)
	if len(rows) != 2 {
		t.Fatalf("alice 应看到两天的数据: %+v", rows)
	}
	// 度量列被完全脱敏；SQL 里必须能看到行过滤条件。
	for _, r := range rows {
		if r[1] != "***" {
			t.Fatalf("amount 应脱敏: %+v", r)
		}
	}
	sqlText, _ := res["sql"].(string)
	if !containsSub(sqlText, "region =") {
		t.Fatalf("行级策略应注入 SQL: %s", sqlText)
	}
	masks, _ := res["masks"].(map[string]string)
	if masks["amount"] != "full" {
		t.Fatalf("应回显脱敏列: %+v", masks)
	}
	if _, ok := res["args"].([]any); !ok {
		t.Fatalf("应返回参数化实参: %#v", res["args"])
	}

	// bob 只能看到自己那一行（region=bob），不能看到 alice 的两行。
	res, err = svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_sem", Dimensions: []string{"region"}, Measures: []string{"amount"},
	}, "bob", "viewer")
	if err != nil {
		t.Fatalf("DatasetQuery 失败: %v", err)
	}
	bobRows := res["rows"].([][]any)
	if len(bobRows) != 1 || bobRows[0][0] != "bob" {
		t.Fatalf("bob 只应看到自己的数据: %+v", bobRows)
	}

	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{Dataset: "ghost"}, "", ""); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的应 NotFound，实际 %v", err)
	}
	// 未知维度是参数错误，不是 500。
	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_sem", Dimensions: []string{"nope"}, Measures: []string{"amount"},
	}, "", ""); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未知维度应报参数错误，实际 %v", err)
	}
}

func containsSub(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 指标：保存前必须校验且引用已发布模型；查询要带维度切片与行级权限。
func TestMetricLifecycle(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)

	bad := metricFor("bad_metric", "dwd_sem")
	bad.Agg = "median"
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, bad); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("非法聚合应被拒绝，实际 %v", err)
	}
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("ghost_metric", "ghost")); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未发布模型应被拒绝，实际 %v", err)
	}

	m, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_sem", "dwd_sem"))
	if err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if m.DisplayName != "gmv_sem" || m.TimeColumn != "dt" {
		t.Fatalf("应补默认展示名与时间列: %+v", m)
	}

	series, err := svc.QueryMetric(ctxBG(), ws.ID, "gmv_sem", semantic.Query{Granularity: "day"})
	if err != nil {
		t.Fatalf("QueryMetric 失败: %v", err)
	}
	if len(series.Points) != 2 {
		t.Fatalf("应有两天序列: %+v", series.Points)
	}
	if series.Points[0].Value+series.Points[1].Value != 60 {
		t.Fatalf("合计应为 60: %+v", series.Points)
	}

	// 行级权限：viewer 只看自己的 region，合计变成 40（alice 的 10+30）。
	// 指标链路按「模型名」匹配策略（见 QueryMetricFor），因此这里用模型名建策略。
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_sem", Role: "viewer", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}
	series, err = svc.QueryMetricFor(ctxBG(), ws.ID, "gmv_sem", semantic.Query{Granularity: "day"}, "alice", "viewer")
	if err != nil {
		t.Fatalf("QueryMetricFor 失败: %v", err)
	}
	var sum float64
	for _, p := range series.Points {
		sum += p.Value
	}
	if sum != 40 {
		t.Fatalf("行级权限应只统计 alice 的数据，实际 %v", sum)
	}

	if _, err := svc.QueryMetric(ctxBG(), ws.ID, "ghost", semantic.Query{}); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的指标应 NotFound，实际 %v", err)
	}
}

// 图表：编码槽必须落在已声明字段内；改图表要能告诉用户影响哪些看板。
func TestChartSaveQueryAndImpact(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)

	badChart := semantic.Chart{
		Name: "c_bad", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "not_declared"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, badChart); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("编码槽引用未声明字段应被拒绝，实际 %v", err)
	}
	missingDS := badChart
	missingDS.Name, missingDS.Dataset, missingDS.Encoding.Y = "c_missing", "ghost", "amount"
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, missingDS); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("数据集不存在应 NotFound，实际 %v", err)
	}

	chart := semantic.Chart{
		Name: "c_sem", Title: "GMV 趋势", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}
	saved, impact, err := svc.ApplyChart(ctxBG(), ws.ID, chart)
	if err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if saved.Version != 1 || len(impact) != 0 {
		t.Fatalf("首版无影响面: %+v %+v", saved, impact)
	}

	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_chart",
		Cards: []semantic.Card{{Title: "GMV", Chart: "c_sem"}},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	// 再存一次图表：影响面应包含引用它的看板。
	_, impact, err = svc.ApplyChart(ctxBG(), ws.ID, chart)
	if err != nil {
		t.Fatalf("更新图表失败: %v", err)
	}
	if !hasStr(impact, "dash_chart") {
		t.Fatalf("影响面应包含引用看板: %v", impact)
	}

	res, err := svc.ChartQuery(ctxBG(), ws.ID, "c_sem", "", "")
	if err != nil {
		t.Fatalf("ChartQuery 失败: %v", err)
	}
	if len(res.Points) != 2 || len(res.Series) != 1 {
		t.Fatalf("应得到两天单系列: %+v", res)
	}
	// 单系列时用标题代替度量名，points 的 series 必须同步改名。
	if res.Series[0].Name != "GMV 趋势" || res.Points[0].Series != "GMV 趋势" {
		t.Fatalf("单系列应改用标题: %+v %+v", res.Series, res.Points)
	}
	if res.SQL == "" || res.ElapsedMs < 0 {
		t.Fatalf("应带 SQL 与耗时: %+v", res)
	}

	// 追加筛选（看板全局时间窗口）要作用到查询上。
	extra := []semantic.Filter{{Field: "dt", Op: ">=", Value: time.Now().AddDate(0, 0, -1).Format("2006-01-02")}}
	res, err = svc.ChartQueryWith(ctxBG(), ws.ID, "c_sem", "", "", extra)
	if err != nil {
		t.Fatalf("ChartQueryWith 失败: %v", err)
	}
	if len(res.Points) != 1 {
		t.Fatalf("时间窗口应只剩最后一天: %+v", res.Points)
	}

	if n, err := svc.DatasetUsage(ctxBG(), ws.ID, "ds_sem"); err != nil || n["charts"] != 1 {
		t.Fatalf("数据集引用数应为 1: %+v %v", n, err)
	}
}

// 看板：卡片引用必须真实存在；查询时单卡失败不影响整页，错误挂在卡片上。
func TestDashboardSaveAndQuery(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_dashq", "dwd_sem")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	chart := semantic.Chart{
		Name: "c_dashq", Title: "趋势", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, chart); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}

	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_bad",
		Cards: []semantic.Card{{Title: "x", Metric: "ghost"}},
	}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("引用不存在指标应被拒绝，实际 %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_bad2",
		Cards: []semantic.Card{{Title: "x", Chart: "ghost"}},
	}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("引用不存在图表应被拒绝，实际 %v", err)
	}

	d := semantic.Dashboard{Name: "dash_q", Cards: []semantic.Card{
		{Title: "GMV", Metric: "gmv_dashq", Days: 7},
		{Title: "趋势", Chart: "c_dashq"},
	}}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, d); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	data, err := svc.QueryDashboard(ctxBG(), ws.ID, "dash_q")
	if err != nil {
		t.Fatalf("QueryDashboard 失败: %v", err)
	}
	if len(data.Cards) != 2 {
		t.Fatalf("应渲染两张卡: %+v", data.Cards)
	}
	for _, c := range data.Cards {
		if c.Error != "" {
			t.Fatalf("卡片不应报错: %+v", c)
		}
		// 指标卡用 Points，图表卡用 ChartPoints（两套渲染结果并存）。
		if c.Kind == "chart" {
			if len(c.ChartPoints) == 0 {
				t.Fatalf("图表卡应有数据: %+v", c)
			}
		} else if len(c.Points) == 0 {
			t.Fatalf("指标卡应有数据: %+v", c)
		}
	}
	// 图表卡走数据集查询，列级脱敏同样要生效（先加策略再查）。
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_sem_t", ColumnName: "amount", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}
	data, err = svc.QueryDashboardWith(ctxBG(), ws.ID, "dash_q", semantic.RunOptions{Days: 2})
	if err != nil {
		t.Fatalf("QueryDashboardWith 失败: %v", err)
	}
	if len(data.Cards) != 2 {
		t.Fatalf("全局筛选后仍应是两张卡: %+v", data.Cards)
	}

	if _, err := svc.QueryDashboard(ctxBG(), ws.ID, "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的看板应 NotFound，实际 %v", err)
	}
}
