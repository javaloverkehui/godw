package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

// brokenDatasource 直接落一条连不上的数据源（绕过 CreateDatasource 的连通性探测）。
// 用途：验证「读取配置成功、真正取连接时失败」这一层错误传播——这类路径
// 用正常数据源永远走不到，而线上探测之后库被回收正是这个形态。
func brokenDatasource(t *testing.T, svc *service.Service, wsID, name string) {
	t.Helper()
	enc, err := svc.Cipher.Encrypt(map[string]string{"password": "x"})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	_, err = svc.Repo.CreateDatasource(ctxBG(), model.Datasource{
		WorkspaceID: wsID, Name: name, Type: "postgres",
		Config: map[string]any{
			"host": "127.0.0.1", "port": "1", "user": "nobody",
			"database": "none", "sslmode": "disable",
		},
	}, enc)
	if err != nil {
		t.Fatalf("写入损坏数据源失败: %v", err)
	}
}

// 指标预览是「草稿口径试算」的唯一入口：它不落库、不读写缓存，
// 且必须和正式查询一样注入行级权限，否则预览结果会是别人看到的数据。
func TestPreviewMetricDefaultsAndRLS(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)

	// TimeColumn / DisplayName 留空时由服务端补默认值：前端只想传「算哪个字段」。
	draft := semantic.Metric{
		Name: "gmv_draft", ModelName: "dwd_sem", Agg: "sum", Expr: "amount",
		Dimensions: []string{"region"},
	}
	series, err := svc.PreviewMetric(ctxBG(), ws.ID, draft, semantic.Query{Granularity: "day"}, "", "")
	if err != nil {
		t.Fatalf("PreviewMetric 失败: %v", err)
	}
	var sum float64
	for _, p := range series.Points {
		sum += p.Value
	}
	if sum != 60 {
		t.Fatalf("全量试算应为 60，实际 %v", sum)
	}
	if series.Cached {
		t.Fatal("预览不应带缓存标记")
	}
	// 试算不落库：指标列表里不应出现这个草稿。
	if _, err := svc.Repo.MetricByName(ctxBG(), ws.ID, "gmv_draft"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("草稿不应被保存，实际 %v", err)
	}

	// 行级权限：viewer 只应统计 alice 的两行（10+30）。
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_sem", Role: "viewer", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}
	scoped, err := svc.PreviewMetric(ctxBG(), ws.ID, draft, semantic.Query{Granularity: "day"}, "alice", "viewer")
	if err != nil {
		t.Fatalf("带身份的试算失败: %v", err)
	}
	sum = 0
	for _, p := range scoped.Points {
		sum += p.Value
	}
	if sum != 40 {
		t.Fatalf("行级权限应只统计 alice 的数据，实际 %v", sum)
	}

	// 非法定义与未发布模型都要在试算入口拒绝，不能等到执行 SQL 才报错。
	bad := draft
	bad.Agg = "median"
	if _, err := svc.PreviewMetric(ctxBG(), ws.ID, bad, semantic.Query{}, "", ""); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("非法聚合应被拒绝，实际 %v", err)
	}
	ghost := draft
	ghost.ModelName = "ghost_model"
	if _, err := svc.PreviewMetric(ctxBG(), ws.ID, ghost, semantic.Query{}, "", ""); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未发布模型应被拒绝，实际 %v", err)
	}
}

// 指标定义可能先于模型下线而存在（模型回滚/删除后仍留在库里），
// 此时查询必须报「模型未发布」而不是去连一个不存在的数据源。
func TestQueryMetricForUnpublishedModel(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.Repo.SaveMetric(ctxBG(), ws.ID, semantic.Metric{
		Name: "m_orphan", DisplayName: "m_orphan", ModelName: "dwd_gone",
		Agg: "sum", Expr: "amount", TimeColumn: "dt",
	}); err != nil {
		t.Fatalf("写入指标失败: %v", err)
	}
	if _, err := svc.QueryMetricFor(ctxBG(), ws.ID, "m_orphan", semantic.Query{}, "", ""); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("模型未发布应 NotFound，实际 %v", err)
	}
}

// ApplyModelSpec 的拒绝分支直接决定「能不能上线」：有损类型、破坏性变更、
// DDL 执行失败都必须停在这里，而不是留下一个半成品版本。
func TestApplyModelSpecEdgeRejections(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	// 有损类型（decimal 精度超过引擎上限）必须要求显式 allow_lossy。
	lossy := modelSpec(ds.Name, "dwd_lossy", "dw", "dwd_lossy")
	lossy.Columns[2].Type = "decimal(50,10)"
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", lossy, true, false, true); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("有损类型应被拒绝，实际 %v", err)
	}

	// 数据源可达但建连失败：模型不能落库，避免出现「定义了但永远跑不了」的模型。
	brokenDatasource(t, svc, ws.ID, "pg_broken")
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", modelSpec("pg_broken", "dwd_brk", "dw", "dwd_brk"), true, false, true); err == nil {
		t.Fatal("连不上的数据源应拒绝建模")
	}
	if _, err := svc.PreviewModelDDL(ctxBG(), ws.ID, modelSpec("pg_broken", "dwd_brk", "dw", "dwd_brk")); err == nil {
		t.Fatal("连不上的数据源应拒绝预览 DDL")
	}
}

// 凭证解密失败是「密钥更换/密文损坏」这类运维事故的形态：
// 必须报内部错误，而不是把空密码当成有效连接去连库。
func TestDatasourceSpecDecryptFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE datasource SET secret_enc=$2 WHERE id=$1`, ds.ID, []byte("not-a-valid-ciphertext")); err != nil {
		t.Fatalf("破坏密文失败: %v", err)
	}
	if _, err := svc.DatasourceSpec(ctxBG(), ws.ID, ds.Name); !errs.Is(err, errs.CodeInternal) {
		t.Fatalf("解密失败应报 Internal，实际 %v", err)
	}
	// 模型入口同样要停在这里，而不是拿空凭证去建连。
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", modelSpec(ds.Name, "dwd_cipher", "dw", "dwd_cipher"), true, false, true); err == nil {
		t.Fatal("凭证损坏时建模应失败")
	}
}

// ApplyPipeline 的结构校验与发布副作用：重复节点 ID 直接拒绝；
// 质量节点没有模型名不代表不写表，血缘记录要跳过它而不是写一条空边。
func TestApplyPipelineValidateAndLineageWithQualityNode(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_pl_src", "dw", "dwd_pl_src"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_pl_dst", "dw", "dwd_pl_dst"))

	dup := `kind: Pipeline
name: pl_dup
nodes:
  - id: same
    type: source
    datasource: ` + ds.Name + `
    sql: SELECT 1
  - id: same
    type: source
    datasource: ` + ds.Name + `
    sql: SELECT 2
`
	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "a", []byte(dup), true); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("重复节点 ID 应被拒绝，实际 %v", err)
	}

	withQuality := `kind: Pipeline
name: pl_quality_line
nodes:
  - id: raw
    type: source
    datasource: ` + ds.Name + `
    sql: SELECT 1 AS id
  - id: src
    type: sql
    model: dwd_pl_src
    depends_on: [raw]
    datasource: ` + ds.Name + `
    sql: SELECT id, amount FROM dw.dwd_pl_src
  - id: q
    type: quality
    model: dwd_pl_src
    depends_on: [src]
    config:
      rules:
        - name: r1
          kind: not_null
          column: id
  - id: w
    type: sql
    model: dwd_pl_dst
    depends_on: [src, q]
    sql: SELECT s.id AS id, s.amount AS amount FROM dw.dwd_pl_src s
`
	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "a", []byte(withQuality), true); err != nil {
		t.Fatalf("发布带质量节点的管道失败: %v", err)
	}
	edges, err := svc.Repo.ListLineage(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读血缘失败: %v", err)
	}
	up := map[string][]string{}
	for _, e := range edges {
		up[e.Downstream] = append(up[e.Downstream], e.Upstream)
	}
	if !hasStr(up["dwd_pl_dst"], "dwd_pl_src") {
		t.Fatalf("写表节点应记录上游模型: %+v", up)
	}
	if _, ok := up[""]; ok {
		t.Fatalf("不应产生空节点名的血缘: %+v", up)
	}
}

// 图表的系列拆分、坐标轴格式化、取值归一化都是「看起来正常但数据全丢」的高发区：
// numeric 被扫成结构体、时间被格式化成带时区的长串都会让前端静默画成 0。
func TestChartSeriesAxisAndValueBranches(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)

	seriesChart := semantic.Chart{
		Name: "c_series", Title: "分区域", Type: "stack_bar", Dataset: "ds_sem",
		Dimensions: []string{"dt", "region"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount", Series: "region"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, seriesChart); err != nil {
		t.Fatalf("保存多系列图表失败: %v", err)
	}
	res, err := svc.ChartQuery(ctxBG(), ws.ID, "c_series", "", "")
	if err != nil {
		t.Fatalf("多系列查询失败: %v", err)
	}
	names := map[string]bool{}
	for _, s := range res.Series {
		names[s.Name] = true
	}
	if !names["alice"] || !names["bob"] {
		t.Fatalf("系列应按维度值拆分: %+v", res.Series)
	}
	for _, p := range res.Points {
		if p.Series == "" || p.Series == "amount" {
			t.Fatalf("每个点都要带系列名: %+v", p)
		}
	}

	// Y 槽指向文本列：toFloatSafe 必须走字符串分支并解析失败为 0，而不是把
	// "{1 -3 false}" 这类结构体字符串当成数字。
	textChart := semantic.Chart{
		Name: "c_text_y", Type: "bar", Dataset: "ds_sem",
		Dimensions: []string{"dt", "region"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "region"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, textChart); err != nil {
		t.Fatalf("保存文本轴图表失败: %v", err)
	}
	if _, err := svc.ChartQuery(ctxBG(), ws.ID, "c_text_y", "", ""); err != nil {
		t.Fatalf("文本轴查询失败: %v", err)
	}

	// 时间轴带时分：不能输出 "2026-08-01 13:45:00 +0000 UTC"。
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (ts timestamp not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_ts_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01 13:45:00',1,10,'a'),('2026-08-02 09:10:00',2,20,'b')`, p.table("dwd_ts_t"))
	publishModel(t, svc, ws.ID, modelSpec(dsNameOf(t, svc, ws.ID), "dwd_ts", p.schema, "dwd_ts_t"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, semantic.Dataset{
		Name: "ds_ts", BaseModel: "dwd_ts",
		Dimensions: []semantic.Dimension{
			{Name: "ts", Field: "ts", Type: "time", Filterable: true, TimeGrains: []string{"day"}},
		},
		Measures: []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}); err != nil {
		t.Fatalf("保存时间数据集失败: %v", err)
	}
	tsChart := semantic.Chart{
		Name: "c_ts", Type: "line", Dataset: "ds_ts",
		Dimensions: []string{"ts"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "ts", Y: "amount"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, tsChart); err != nil {
		t.Fatalf("保存时间轴图表失败: %v", err)
	}
	tsRes, err := svc.ChartQuery(ctxBG(), ws.ID, "c_ts", "", "")
	if err != nil {
		t.Fatalf("时间轴查询失败: %v", err)
	}
	for _, pt := range tsRes.Points {
		if strings.Contains(pt.X, "UTC") || strings.Contains(pt.X, "+0000") {
			t.Fatalf("横轴不应带时区: %q", pt.X)
		}
	}

	// 标题为空时单系列保留度量名（firstNonEmptyStr 的兜底方向）。
	noTitle := semantic.Chart{
		Name: "c_notitle", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, noTitle); err != nil {
		t.Fatalf("保存无标题图表失败: %v", err)
	}
	nt, err := svc.ChartQuery(ctxBG(), ws.ID, "c_notitle", "", "")
	if err != nil || len(nt.Series) != 1 || nt.Series[0].Name == "" {
		t.Fatalf("无标题时系列名应为度量名: %+v %v", nt, err)
	}
}

// dsNameOf 取工作空间里唯一的 pg 数据源名（seedSemantic 内部创建，测试无法直接拿到）。
func dsNameOf(t *testing.T, svc *service.Service, wsID string) string {
	t.Helper()
	list, err := svc.Repo.ListDatasources(ctxBG(), wsID)
	if err != nil || len(list) == 0 {
		t.Fatalf("读取数据源失败: %+v %v", list, err)
	}
	return list[0].Name
}

// 看板查询把「卡片错误」挂在卡片上而不是整页失败，但每条解析链路的错误都必须
// 被接住：指标被删、模型下线、数据源消失、图表/数据集被删，界面都要能定位到具体卡片。
func TestDashboardCardResolveFailures(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_card", "dwd_sem")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	chart := semantic.Chart{
		Name: "c_card", Title: "趋势", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, chart); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name: "dash_cards",
		Cards: []semantic.Card{
			{Title: "GMV", Metric: "gmv_card", Days: 7},
			{Title: "趋势", Chart: "c_card"},
		},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	// 指标被删（历史看板仍引用）：错误必须挂在指标卡上。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM metric WHERE workspace_id=$1 AND name='gmv_card'`, ws.ID); err != nil {
		t.Fatalf("删除指标失败: %v", err)
	}
	data, err := svc.QueryDashboard(ctxBG(), ws.ID, "dash_cards")
	if err != nil {
		t.Fatalf("单卡失败不应让整页失败: %v", err)
	}
	if len(data.Cards) != 2 || data.Cards[0].Error == "" {
		t.Fatalf("指标卡应带错误: %+v", data.Cards)
	}

	// 图表卡：Days>0 时会先取图表/数据集算时间窗口，两者被删都要能安全降级。
	if err := svc.Repo.DeleteChart(ctxBG(), ws.ID, "c_card"); err != nil {
		t.Fatalf("删除图表失败: %v", err)
	}
	data, err = svc.QueryDashboardWith(ctxBG(), ws.ID, "dash_cards", semantic.RunOptions{Days: 3})
	if err != nil {
		t.Fatalf("图表被删不应让整页失败: %v", err)
	}
	if data.Cards[1].Error == "" {
		t.Fatalf("图表卡应带错误: %+v", data.Cards[1])
	}
}

// 模型下线与数据源消失同样发生在看板指标卡上：resolve 的每一步都要返回
// 明确的错误，而不是带着 nil 连接器继续往下算。
func TestDashboardMetricModelAndDatasourceFailures(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_res", "dwd_sem")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_res",
		Cards: []semantic.Card{{Title: "GMV", Metric: "gmv_res", Days: 7}},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	// 模型下线（active_version=0）→ PublishedModelSpec 报 NotFound。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE model SET active_version=0 WHERE workspace_id=$1 AND name='dwd_sem'`, ws.ID); err != nil {
		t.Fatalf("下线模型失败: %v", err)
	}
	data, err := svc.QueryDashboard(ctxBG(), ws.ID, "dash_res")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if data.Cards[0].Error == "" {
		t.Fatalf("模型下线应挂在卡片错误上: %+v", data.Cards[0])
	}

	// 模型恢复但数据源被改名：DatasourceSpec 找不到 → 仍是卡片错误。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE model SET active_version=1 WHERE workspace_id=$1 AND name='dwd_sem'`, ws.ID); err != nil {
		t.Fatalf("恢复模型失败: %v", err)
	}
	ds := dsNameOf(t, svc, ws.ID)
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE datasource SET name=$2 WHERE workspace_id=$1 AND name=$3`, ws.ID, ds+"_gone", ds); err != nil {
		t.Fatalf("改名数据源失败: %v", err)
	}
	data, err = svc.QueryDashboard(ctxBG(), ws.ID, "dash_res")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if data.Cards[0].Error == "" {
		t.Fatalf("数据源消失应挂在卡片错误上: %+v", data.Cards[0])
	}
}

// 时间窗口只在数据集存在可筛选的时间维度时注入：宁可不筛，也不要因为一条
// 时间条件让整张图报错。这里验证「没有 filterable 时间维度就不加条件」。
func TestChartCardFiltersWithoutFilterableTime(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text)`, p.table("dwd_nofilt_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,10,'a')`, p.table("dwd_nofilt_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_nofilt", p.schema, "dwd_nofilt_t"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, semantic.Dataset{
		Name: "ds_nofilt", BaseModel: "dwd_nofilt",
		Dimensions: []semantic.Dimension{
			// time 但不可筛选：全局时间窗口不能强加给它。
			{Name: "dt", Field: "dt", Type: "time", Filterable: false, TimeGrains: []string{"day"}},
			{Name: "region", Field: "region", Type: "categorical", Filterable: true},
		},
		Measures: []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
		Name: "c_nofilt", Title: "无筛选", Type: "line", Dataset: "ds_nofilt",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_nofilt",
		Cards: []semantic.Card{{Title: "趋势", Chart: "c_nofilt"}},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	data, err := svc.QueryDashboardWith(ctxBG(), ws.ID, "dash_nofilt", semantic.RunOptions{Days: 3})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if data.Cards[0].Error != "" {
		t.Fatalf("不可筛选的时间维度不应导致报错: %+v", data.Cards[0])
	}
}

// 选择器的错误分支决定了「表达式写错」能不能被立刻发现：
// 空项、缺取值、未知前缀、tag/layer/pipeline 空命中都要给出可读错误。
func TestSelectorParseAndExpandEdgeBranches(t *testing.T) {
	models := []service.SelectModel{
		{Name: "a", Layer: "dwd", Tags: []string{"core"}},
		{Name: "b", Layer: "dwd"},
	}
	edges := []service.SelectEdge{
		{Up: "", Down: "a"},  // 空上游
		{Up: "x", Down: "y"}, // 两端都不在模型集合
		{Up: "a", Down: "a"}, // 自环
		{Up: "a", Down: "b"}, // 有效
		{Up: "a", Down: "b"}, // 重复边：appendUnique 必须去重
	}
	pipelines := map[string][]string{
		"empty_pipe":       {},    // 管道存在但没写模型
		"ghost_model_pipe": {"z"}, // 写的模型不在已发布集合
	}

	// 多余分隔符产生空项：跳过而不是报错。
	terms, err := service.ParseSelect("a, ,b")
	if err != nil || len(terms) != 2 {
		t.Fatalf("空项应被跳过: %+v %v", terms, err)
	}
	if _, err := service.ParseSelect("tag:"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺取值应报参数错误，实际 %v", err)
	}

	terms, _ = service.ParseSelect("a+")
	got, err := service.ExpandSelect(terms, models, edges, pipelines)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	wantSet(t, got, "a", "b")

	for _, expr := range []string{"tag:none", "layer:ads", "pipeline:nope", "pipeline:empty_pipe", "pipeline:ghost_model_pipe"} {
		terms, perr := service.ParseSelect(expr)
		if perr != nil {
			t.Fatalf("解析 %q 失败: %v", expr, perr)
		}
		if _, err := service.ExpandSelect(terms, models, edges, pipelines); !errs.Is(err, errs.CodeNotFound) {
			t.Fatalf("%q 应报 NotFound，实际 %v", expr, err)
		}
	}
}

// 建模侧的几个边界：数据源没写 type 时按 postgres 处理；未知引擎退回 PG 方言；
// 脚手架在缺上游列时自动从已采集的 catalog 里补，省掉用户手填。
func TestModelingEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	// 直接落一条没有 type 的数据源：dialectFor 必须给出 postgres，而不是空串。
	enc, _ := svc.Cipher.Encrypt(map[string]string{"password": "godw"})
	if _, err := svc.Repo.CreateDatasource(ctxBG(), model.Datasource{
		WorkspaceID: ws.ID, Name: "pg_notype", Type: "",
		Config: map[string]any{"host": "127.0.0.1", "port": "5432", "user": "godw",
			"database": "godw", "sslmode": "disable"},
	}, enc); err != nil {
		t.Fatalf("写入无类型数据源失败: %v", err)
	}
	// CheckModel 走 dialectFor：没有 type 时按 postgres 校验类型，不能判为不合法。
	chk, err := svc.CheckModel(ctxBG(), ws.ID, modelSpec("pg_notype", "dwd_notype", "dw", "dwd_notype"))
	if err != nil || !chk.Valid {
		t.Fatalf("无 type 的数据源应按 postgres 校验: %+v %v", chk, err)
	}

	// 数据源不存在时 dialectFor 报错 → ModelDiff 仍需给出一个可用的引擎。
	if _, err := svc.Repo.SaveModel(ctxBG(), ws.ID, modelSpec(ds.Name, "dwd_ghost_ds", "dw", "dwd_ghost_ds"), "", "", "t", true); err != nil {
		t.Fatalf("直接落模型失败: %v", err)
	}
	spec := modelSpec("no_such_ds", "dwd_ghost_ds", "dw", "dwd_ghost_ds")
	out, err := svc.ModelDiff(ctxBG(), ws.ID, spec)
	if err != nil || out["engine"] != "postgres" {
		t.Fatalf("数据源缺失应退回 postgres: %+v %v", out, err)
	}

	// 脚手架：只给 source_name，列从 catalog 里取。
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: "dw", Name: "src_tbl",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint", PrimaryKey: true},
			{Name: "dt", Type: "date"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}
	res, err := svc.ScaffoldModel(ctxBG(), ws.ID, modelc.ScaffoldOptions{
		Layer: "dwd", Name: "from_catalog", Datasource: ds.Name, Schema: "dw",
		SourceName: "src_tbl",
	})
	if err != nil {
		t.Fatalf("从 catalog 生成脚手架失败: %v", err)
	}
	if !hasStr(res.KeyCandidates, "id") {
		t.Fatalf("应识别出主键候选: %+v", res)
	}

	// 规范为空时 LayerRules 返回内置默认值，而不是空 map。
	saved := svc.Rules
	svc.Rules = modelc.Rules{}
	rules, err := svc.LayerRules(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读取分层规范失败: %v", err)
	}
	svc.Rules = saved
	layers, _ := rules["layers"].(map[string]any)
	if len(layers) == 0 {
		t.Fatalf("空规范应回退默认值: %+v", rules)
	}
}

// 自助查询要把驱动原始类型转成人可读形式：bytea→字符串、timestamp→固定格式、
// numeric→float。不转的话前端拿到的 base64 / RFC3339 / 结构体会直接显示成乱码。
func TestRawSQLNormalizeRawValues(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	res, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name,
		`SELECT now() AS ts, 1.25::numeric AS n, 'ok'::text AS t, '\x6869'::bytea AS b`,
		"", "", 10)
	if err != nil {
		t.Fatalf("RawSQLQuery 失败: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("应返回一行: %+v", res)
	}
	row := res.Rows[0]
	if s, ok := row[0].(string); !ok || !strings.Contains(s, ":") || strings.Contains(s, "UTC") {
		t.Fatalf("时间应格式化为固定串: %#v", row[0])
	}
	if f, ok := row[1].(float64); !ok || f != 1.25 {
		t.Fatalf("numeric 应转成 float64: %#v", row[1])
	}
	if row[2] != "ok" {
		t.Fatalf("text 应原样返回: %#v", row[2])
	}
	if row[3] != "hi" {
		t.Fatalf("bytea 应转成字符串: %#v", row[3])
	}
	_ = time.Now
}

// DryRun 的失败分支：单节点渲染失败不能影响其它节点；模型没有分区时增量
// 推导失败要落到节点错误里，让用户看到「这次会全量扫」而不是静默通过。
func TestDryRunNodeErrorBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	// 无分区键、也没有 updated_at 这类时间列：增量条件推导必然失败。
	noPart := modelSpec(ds.Name, "dwd_nopart", "dw", "dwd_nopart")
	noPart.PartitionBy = nil
	publishModel(t, svc, ws.ID, noPart)

	// 参数声明了但没给默认值：DryRun 只渲染不执行，必须在节点上报「缺值」，
	// 否则用户会以为这条 SQL 能直接跑。
	spec := model.PipelineSpec{
		Name:   "dry_err",
		Params: []model.ParamSpec{{Name: "nope", Type: "int"}},
		Nodes: []model.NodeSpec{
			{ID: "badvar", Type: model.NodeSQL, SQL: "SELECT ${nope}"},
			{ID: "w", Type: model.NodeSQL, Model: "dwd_nopart", SQL: "SELECT id FROM dw.raw"},
			{ID: "wh", Type: model.NodeSQL, Model: "dwd_nopart", Config: map[string]any{"where": "id > 0"},
				SQL: "SELECT id FROM dw.raw"},
		},
	}
	res, err := svc.DryRun(ctxBG(), ws.ID, spec, "2026-08-01", nil)
	if err != nil {
		t.Fatalf("DryRun 不应整体失败: %v", err)
	}
	byID := map[string]service.DryRunNode{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	if byID["badvar"].Error == "" {
		t.Fatalf("未声明参数应写进节点错误: %+v", byID["badvar"])
	}
	if byID["w"].Error == "" {
		t.Fatalf("无分区模型的 SQL 节点应提示增量推导失败: %+v", byID["w"])
	}
	// 显式 where 的节点应能推导成功（不报错、不重复提示全量扫描）。
	if byID["wh"].Error != "" || byID["wh"].Inference == nil || byID["wh"].Inference.Where != "id > 0" {
		t.Fatalf("显式 where 应原样进入推导结果: %+v", byID["wh"])
	}

	// 采样时 NULL 值要转成空串，而不是 "<nil>"；超长 SQL 报错要截断。
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint, note text)`, p.table("dwd_null_t"))
	p.exec(t, `INSERT INTO %s VALUES (1, NULL)`, p.table("dwd_null_t"))
	sampleSpec := model.PipelineSpec{Name: "sm", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSQL, Datasource: ds.Name,
			SQL: "SELECT id, note FROM " + p.table("dwd_null_t")},
	}}
	out, err := svc.SampleRun(ctxBG(), ws.ID, sampleSpec, "", "2026-08-01", 10)
	if err != nil {
		t.Fatalf("SampleRun 失败: %v", err)
	}
	if out.Rows[0][1] != "" {
		t.Fatalf("NULL 应转成空串: %#v", out.Rows[0])
	}
	longSQL := "SELECT 1 " + strings.Repeat("-- 填充\n", 100) + " FROM nowhere"
	bad := model.PipelineSpec{Name: "sm_bad", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSQL, Datasource: ds.Name, SQL: longSQL},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, bad, "s", "2026-08-01", 10); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("坏 SQL 应报执行失败，实际 %v", err)
	}
}

// 补数的强制模式：已有实例的日期也要重新排进去，否则「补过一次就不能再补」
// 会让排障时无法重跑。没有可补日期时要给出明确冲突错误。
func TestBackfillForceAndRangeErrors(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("bf_force"))

	if _, err := svc.Backfill(ctxBG(), ws.ID, "", "2026-01-01", "2026-01-02", "", "a", false); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺管道应报参数错误，实际 %v", err)
	}
	if _, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-13-01", "2026-01-02", "", "a", false); err == nil {
		t.Fatal("非法 from 应报错")
	}
	if _, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-01-01", "2026-13-02", "", "a", false); err == nil {
		t.Fatal("非法 to 应报错")
	}
	if _, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-02-01", "2026-01-01", "", "a", false); err == nil {
		t.Fatal("to 早于 from 应报错")
	}

	first, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-01-01", "2026-01-02", "首补", "a", false)
	if err != nil || first.Created != 2 || first.Skipped != 0 {
		t.Fatalf("首次补数应创建 2 天: %+v %v", first, err)
	}
	// 不勾强制：两天都已存在 → 冲突。
	if _, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-01-01", "2026-01-02", "重复", "a", false); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("无新日期应报冲突，实际 %v", err)
	}
	// 强制：把两天重新排进去，Skipped 归零。
	forced, err := svc.Backfill(ctxBG(), ws.ID, "bf_force", "2026-01-01", "2026-01-02", "强制", "a", true)
	if err != nil {
		t.Fatalf("强制补数失败: %v", err)
	}
	if forced.Total != 2 || forced.Skipped != 0 || forced.Reason != "强制" {
		t.Fatalf("强制补数应包含已存在日期: %+v", forced)
	}
}

// 采集后列被删/改类型要能产出 drop_column 与 modify_column，
// 并按表名+列名稳定排序——结构变更列表是人工复核的依据，顺序漂移会误导。
func TestCollectWithDiffDropAndModify(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, note text)`, p.table("diff2_t"))
	if _, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name); err != nil {
		t.Fatalf("首次采集失败: %v", err)
	}
	p.exec(t, `ALTER TABLE %s DROP COLUMN note`, p.table("diff2_t"))
	p.exec(t, `ALTER TABLE %s ALTER COLUMN id TYPE numeric(20,0)`, p.table("diff2_t"))

	res, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("二次采集失败: %v", err)
	}
	kinds := map[string]string{}
	for _, c := range res.Changes {
		if c.Table == "diff2_t" {
			kinds[c.Kind+":"+c.Column] = c.Kind
		}
	}
	if kinds["drop_column:note"] != "drop_column" {
		t.Fatalf("应识别删列: %+v", res.Changes)
	}
	if kinds["modify_column:id"] != "modify_column" {
		t.Fatalf("应识别改类型: %+v", res.Changes)
	}
	for i := 1; i < len(res.Changes); i++ {
		if res.Changes[i-1].Table > res.Changes[i].Table {
			t.Fatalf("结果应按表名升序: %+v", res.Changes)
		}
	}
}
