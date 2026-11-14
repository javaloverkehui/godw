package service_test

import (
	"encoding/json"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 订阅的 webhook 渠道：成功/失败/连不上三种结局都要如实回写运行状态。
func TestRunSubscriptionWebhook(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_wh", "dwd_sem")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}

	var gotHeader, gotBody string
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-GoDW-Subscription")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		raw, _ := json.Marshal(payload["subscription"])
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()

	sub, err := svc.Repo.SaveSubscription(ctxBG(), ws.ID, model.Subscription{
		Name: "gmv_webhook", Kind: "metric", Target: "gmv_wh",
		Channel: "webhook", Endpoint: okSrv.URL, Enabled: true,
		Config: map[string]any{"days": float64(7)},
	})
	if err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	res, err := svc.RunSubscription(ctxBG(), ws.ID, *sub)
	if err != nil {
		t.Fatalf("RunSubscription 失败: %v", err)
	}
	if res["ok"] != true || res["status"] != http.StatusOK {
		t.Fatalf("成功推送应回 ok: %+v", res)
	}
	if gotHeader != "gmv_webhook" || !strings.Contains(gotBody, "gmv_webhook") {
		t.Fatalf("请求头 / 载荷不对: %q %q", gotHeader, gotBody)
	}

	// 5xx：不算成功，运行状态要标 failed。
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()
	badSub := *sub
	badSub.Endpoint = badSrv.URL
	res, err = svc.RunSubscription(ctxBG(), ws.ID, badSub)
	if err != nil {
		t.Fatalf("5xx 不应返回错误（状态由结果表达）: %v", err)
	}
	if res["ok"] != false {
		t.Fatalf("5xx 应判失败: %+v", res)
	}

	// 连不上：返回 CodeUnavailable，避免上层当成参数错误。
	dead := *sub
	dead.Endpoint = "http://127.0.0.1:1/hook"
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, dead); !errs.Is(err, errs.CodeUnavailable) {
		t.Fatalf("连不上应报不可用，实际 %v", err)
	}

	// 看板订阅走 QueryDashboard；看板不存在时任务标失败并返回错误。
	missing := model.Subscription{ID: "sub_missing", Name: "m", Kind: "dashboard",
		Target: "ghost_dash", Channel: "file"}
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, missing); err == nil {
		t.Fatal("看板不存在应报错")
	}
}

// 节点 SQL 回放的第二现场：上一轮 diff、版本快照丢失、渲染失败、无 SQL 节点。
func TestInstanceNodeSQLEdgeCases(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "sql_edge", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSource, Datasource: ds.Name, SQL: "SELECT '${biz_date}' AS d"},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	first, _, err := svc.Trigger(ctxBG(), ws.ID, "sql_edge", "2026-10-10", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	second, _, err := svc.Trigger(ctxBG(), ws.ID, "sql_edge", "2026-10-11", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	out, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, second.ID, "s")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	// 上一轮同节点一起返回，前端可以直接左右 diff。
	if out.Previous == nil || out.Previous.InstanceID != first.ID || !strings.Contains(out.Previous.SQL, "2026-10-10") {
		t.Fatalf("应返回上一轮 SQL: %+v", out.Previous)
	}

	// 版本快照被清理：退化为当前定义渲染，并显式告警。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM pipeline_version WHERE pipeline_id=$1 AND version=$2`, second.PipelineID, second.PipelineVer); err != nil {
		t.Fatalf("删除版本快照失败: %v", err)
	}
	out, err = svc.InstanceNodeSQL(ctxBG(), ws.ID, second.ID, "s")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.FromSnapshot || out.Warning == "" {
		t.Fatalf("快照丢失应降级并告警: %+v", out)
	}

	// 渲染失败：SQL 引用了未声明参数（直接落库、绕过发布校验）。
	bad := model.PipelineSpec{Kind: "Pipeline", Name: "sql_bad", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSource, Datasource: ds.Name, SQL: "SELECT ${nope}"},
	}}
	publishPipeline(t, svc, ws.ID, bad)
	badInst, _, err := svc.Trigger(ctxBG(), ws.ID, "sql_bad", "2026-10-12", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	out, err = svc.InstanceNodeSQL(ctxBG(), ws.ID, badInst.ID, "s")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("渲染失败应写进 Error: %+v", out)
	}

	// 只有质量节点：没有任何 SQL 可回放。
	noSQL := model.PipelineSpec{Kind: "Pipeline", Name: "sql_none", Nodes: []model.NodeSpec{
		{ID: "q", Type: model.NodeQuality, Model: "m"},
	}}
	publishPipeline(t, svc, ws.ID, noSQL)
	noneInst, _, err := svc.Trigger(ctxBG(), ws.ID, "sql_none", "2026-10-13", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	out, err = svc.InstanceNodeSQL(ctxBG(), ws.ID, noneInst.ID, "")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.Error == "" || !strings.Contains(out.Error, "没有包含 SQL") {
		t.Fatalf("无 SQL 节点应提示: %+v", out)
	}
}

// 文档站的剩余分支：失败运行、未跑过的规则、多管道写同一模型、血缘与物化方式。
func TestCatalogDocsRunStates(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_doc_state", "dw", "dwd_doc_state")
	spec.Materialization = "view"
	spec.Query = "SELECT 1"
	publishModel(t, svc, ws.ID, spec)
	if err := svc.Repo.ReplaceLineage(ctxBG(), ws.ID, "dwd_doc_state", []string{"ods_up"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	if err := svc.Repo.AddLineage(ctxBG(), ws.ID, "dws_down", []string{"dwd_doc_state"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}

	// 两条管道写同一模型，取最晚的一次运行。
	for _, name := range []string{"doc_pipe_a", "doc_pipe_b"} {
		publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: name, Nodes: []model.NodeSpec{
			{ID: "w", Type: model.NodeSQL, Model: "dwd_doc_state", SQL: "SELECT 1"},
			{ID: "q", Type: model.NodeQuality, Model: "dwd_doc_state", DependsOn: []string{"w"},
				Config: map[string]any{"rules": []any{
					map[string]any{"name": "never_run", "kind": "not_null", "column": "id"},
				}}},
		}})
	}
	// 第一条管道失败、第二条成功：最终展示成功那次。
	a, _, _ := svc.Trigger(ctxBG(), ws.ID, "doc_pipe_a", "2026-10-14", "manual", nil)
	if err := svc.Repo.FinishTask(ctxBG(), a.ID, "w", "failed", 0, 1, "boom"); err != nil {
		t.Fatalf("置失败失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), a.ID, "failed", "boom"); err != nil {
		t.Fatalf("置失败失败: %v", err)
	}
	b, _, _ := svc.Trigger(ctxBG(), ws.ID, "doc_pipe_b", "2026-10-15", "manual", nil)
	if err := svc.Repo.FinishTask(ctxBG(), b.ID, "w", "success", 5, 2, ""); err != nil {
		t.Fatalf("置成功失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), b.ID, "success", ""); err != nil {
		t.Fatalf("置成功失败: %v", err)
	}

	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogDocs 失败: %v", err)
	}
	doc := page.Entries[0]
	if doc.Materializ != "view" {
		t.Fatalf("应展示物化方式: %+v", doc.Materializ)
	}
	if !hasStr(doc.Upstream, "ods_up") || !hasStr(doc.Downstream, "dws_down") {
		t.Fatalf("血缘应带双向: %+v %+v", doc.Upstream, doc.Downstream)
	}
	if doc.LastRun == nil || doc.LastRun.Pipeline != "doc_pipe_b" || doc.LastRun.Status != "success" {
		t.Fatalf("应取最晚的一次运行: %+v", doc.LastRun)
	}
	// 没有新鲜度规则 → 只能按运行时间给出 unknown，而不是假装新鲜。
	if doc.Freshness.Status != "unknown" || doc.Freshness.Basis != "last_success" || doc.Freshness.ThresholdMin != 0 {
		t.Fatalf("无规则时状态应为 unknown: %+v", doc.Freshness)
	}
	// 规则未跑过时 LastPassed 必须留 nil，前端显示「未运行」。
	if len(doc.Quality) == 0 || doc.Quality[0].LastPassed != nil {
		t.Fatalf("未跑过的规则应留空结果: %+v", doc.Quality)
	}
}

// 阶段内失败不中止同阶段其它节点，但下游必须被标记 blocked（而不是成功或 pending）。
func TestExecuteBlocksDownstream(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint)`, p.table("dwd_blk_t"))
	p.exec(t, `INSERT INTO %s VALUES (1)`, p.table("dwd_blk_t"))

	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "blk_pipe", Nodes: []model.NodeSpec{
		{ID: "bad", Type: model.NodeSource, Datasource: ds.Name, SQL: "SELECT * FROM no_such_relation"},
		{ID: "after_bad", Type: model.NodeSource, Datasource: ds.Name, DependsOn: []string{"bad"},
			SQL: "SELECT id FROM " + p.table("dwd_blk_t")},
		{ID: "alone", Type: model.NodeSource, Datasource: ds.Name,
			SQL: "SELECT id FROM " + p.table("dwd_blk_t")},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "blk_pipe", "2026-10-16", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	// 预置一个已成功节点，执行时应跳过（模拟重跑）。
	if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "alone", "success", 1, 1, ""); err != nil {
		t.Fatalf("预置成功任务失败: %v", err)
	}

	if _, err := svc.RunOnce(ctxBG(), "w1"); err == nil {
		t.Fatal("存在失败节点时应返回错误")
	}
	tasks, err := svc.Repo.Tasks(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	status := map[string]string{}
	for _, tk := range tasks {
		status[tk.NodeID] = tk.Status
	}
	if status["bad"] != "failed" {
		t.Fatalf("坏节点应 failed: %+v", status)
	}
	if status["after_bad"] != "blocked" {
		t.Fatalf("下游应 blocked: %+v", status)
	}
	if status["alone"] != "success" {
		t.Fatalf("已成功节点应跳过保留: %+v", status)
	}
}

// 按表达式触发：解析结果与每个管道的触发结果必须一一对应，
// 单条失败只影响自己，不阻断整批。
func TestTriggerSelectReturnsPerPipelineResult(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "dwd_trig_sel", "dwd"))
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "trig_sel_pipe", Nodes: []model.NodeSpec{
		{ID: "w", Type: model.NodeSQL, Model: "dwd_trig_sel", SQL: "SELECT 1"},
	}})
	runs, res, err := svc.TriggerSelect(ctxBG(), ws.ID, "dwd_trig_sel", "2026-10-17", nil)
	if err != nil {
		t.Fatalf("TriggerSelect 失败: %v", err)
	}
	if len(runs) != len(res.Pipelines) || len(runs) != 1 {
		t.Fatalf("每个命中管道都应有一条结果: %+v", runs)
	}
	if runs[0].Pipeline != "trig_sel_pipe" {
		t.Fatalf("结果应对应命中管道: %+v", runs[0])
	}
	// 单条结果要么给出实例，要么带错误；不能两者都空。
	if runs[0].InstanceID == "" && runs[0].Error == "" {
		t.Fatalf("结果必须带实例或错误: %+v", runs[0])
	}
}

// 值采样要区分手机/邮箱/证件/IP 四类，且采样失败时安静返回 none（不误报）。
func TestSamplePIITypes(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint, phone_col text, mail_col text, card_col text, ip_col text, misc text, all_null text)`, p.table("pii_kinds"))
	p.exec(t, `INSERT INTO %s VALUES
		(1,'13800000001','a@b.com','110101199001011234','10.0.0.1','x',NULL),
		(2,'13800000002','c@d.com','110101199001011235','10.0.0.2','y',NULL),
		(3,'13800000003','e@f.com','110101199001011236','10.0.0.3','z',NULL)`, p.table("pii_kinds"))
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "pii_kinds",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"}, {Name: "phone_col", Type: "text"},
			{Name: "mail_col", Type: "text"}, {Name: "card_col", Type: "text"},
			{Name: "ip_col", Type: "text"}, {Name: "misc", Type: "text"}, {Name: "all_null", Type: "text"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}
	findings, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 0)
	if err != nil {
		t.Fatalf("DetectPII 失败: %v", err)
	}
	level := map[string]service.PIILevel{}
	for _, f := range findings {
		level[f.Column] = f.Level
	}
	if level["phone_col"] != service.PIIMedium || level["mail_col"] != service.PIIMedium {
		t.Fatalf("手机/邮箱应判 medium: %+v", level)
	}
	if level["card_col"] != service.PIIHigh {
		t.Fatalf("证件号应判 high: %+v", level)
	}
	if level["ip_col"] != service.PIILow {
		t.Fatalf("IP 应判 low: %+v", level)
	}
	if _, ok := level["misc"]; ok {
		t.Fatalf("普通文本不应命中: %+v", level)
	}
	if _, ok := level["all_null"]; ok {
		t.Fatalf("全 NULL 列不应命中: %+v", level)
	}

	// 采样表被删：采样失败安静返回，不影响其它列。
	p.exec(t, `DROP TABLE %s`, p.table("pii_kinds"))
	if _, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 999); err != nil {
		t.Fatalf("采样失败不应让整体报错: %v", err)
	}
}

// 模型入口的其它败例：非法命名、类型不支持、DDL 执行失败。
func TestApplyModelSpecRejections(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	badName := modelSpec(ds.Name, "Bad-Name", "dw", "dwd_bad")
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", badName, true, false, true); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("非法命名应报模型不合法，实际 %v", err)
	}
	badType := modelSpec(ds.Name, "dwd_bad_type", "dw", "dwd_bad_type")
	badType.Columns[1].Type = "varchar(10)"
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", badType, true, false, true); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("不支持的类型应被拒绝，实际 %v", err)
	}
	// 规范违规：dwd 表名没有 dwd_ 前缀。
	badLayer := modelSpec(ds.Name, "wrong_prefix", "dw", "wrong_prefix")
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a", badLayer, true, false, true); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("命名规范违规应被拒绝，实际 %v", err)
	}
}

// 数据集查询失败：表被删要报执行失败；基础模型下线要报 NotFound。
func TestDatasetQueryFailures(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text)`, p.table("dwd_dqf_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,1,'a')`, p.table("dwd_dqf_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dqf", p.schema, "dwd_dqf_t"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_dqf", "dwd_dqf", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	req := semantic.QueryRequest{Dataset: "ds_dqf", Dimensions: []string{"dt"}, Measures: []string{"amount"}}

	p.exec(t, `DROP TABLE %s`, p.table("dwd_dqf_t"))
	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, req, "", ""); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("表被删应报执行失败，实际 %v", err)
	}

	// 基础模型下线（active_version=0）后数据集不可查询。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(), `UPDATE model SET active_version=0 WHERE workspace_id=$1 AND name='dwd_dqf'`, ws.ID); err != nil {
		t.Fatalf("下线模型失败: %v", err)
	}
	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, req, "", ""); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("基础模型未发布应 NotFound，实际 %v", err)
	}
}

// 画像查询失败（表在采集后被删）要报执行失败，并带可定位的 SQL。
func TestProfileQueryFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2))`, p.table("dwd_pf_t"))
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "dwd_pf_t",
		Columns: []model.ColumnSpec{{Name: "dt", Type: "date"}, {Name: "id", Type: "bigint"}, {Name: "amount", Type: "decimal(18,2)"}},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}
	p.exec(t, `DROP TABLE %s`, p.table("dwd_pf_t"))
	if _, err := svc.Profile(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_pf_t", 10); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("表被删应报执行失败，实际 %v", err)
	}
}

// 采样失败：模型指向不存在的表时是执行失败，而不是返回空结果。
func TestSampleRunModelExecutionFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_sm_bad", "dw", "dwd_sm_bad"))
	spec := model.PipelineSpec{Name: "sm_bad", Nodes: []model.NodeSpec{
		{ID: "w", Type: model.NodeSQL, Model: "dwd_sm_bad", SQL: "SELECT 1 AS id"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, spec, "w", "2026-08-01", 10); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("表不存在应报执行失败，实际 %v", err)
	}
}

// 列级血缘：SELECT * 与非 SQL 文本要降级处理，且不能留下自环。
func TestRecordColumnLineageDegrades(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_line_src", "dw", "dwd_line_src"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_line_dst", "dw", "dwd_line_dst"))

	spec := model.PipelineSpec{Kind: "Pipeline", Name: "line_bad", Nodes: []model.NodeSpec{
		{ID: "star", Type: model.NodeSQL, Model: "dwd_line_dst",
			SQL: "SELECT * FROM dw.dwd_line_src"},
		{ID: "bad", Type: model.NodeSQL, Model: "dwd_line_dst", SQL: "this is not sql"},
	}}
	svc.RecordColumnLineage(ctxBG(), ws.ID, spec)

	// SELECT * 不应写出错误列映射；非 SQL 文本被跳过而不是 panic。
	edges, err := svc.Repo.ColumnLineage(ctxBG(), ws.ID, "dwd_line_dst", "")
	if err != nil {
		t.Fatalf("ColumnLineage 失败: %v", err)
	}
	for _, e := range edges {
		if e.UpstreamTable == e.DownstreamTable && e.UpstreamColumn == e.DownstreamColumn {
			t.Fatalf("不应写入自环: %+v", e)
		}
	}
}

// 放行时不指定节点：所有失败任务都应被置为 skipped。
func TestWaiveQualityAllNodes(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "waive_all", Nodes: []model.NodeSpec{
		{ID: "a", Type: model.NodeQuality, Model: "m"},
		{ID: "b", Type: model.NodeQuality, Model: "m"},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "waive_all", "2026-10-18", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := svc.Repo.FinishTask(ctxBG(), inst.ID, id, "failed", 0, 0, "bad"); err != nil {
			t.Fatalf("置失败失败: %v", err)
		}
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "failed", "bad"); err != nil {
		t.Fatalf("置失败失败: %v", err)
	}
	if err := svc.WaiveQuality(ctxBG(), ws.ID, inst.ID, "", "整体放行", "alice"); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	tasks, _ := svc.Repo.Tasks(ctxBG(), inst.ID)
	for _, tk := range tasks {
		if tk.Status != "skipped" {
			t.Fatalf("全部失败节点都应 skipped: %+v", tk)
		}
	}
}

// 同名数据源必须拒绝，否则「模型指向哪个库」会变得不确定。
func TestCreateDatasourceDuplicate(t *testing.T) {
	svc, ws := newSvc(t)
	newPgDatasource(t, svc, ws.ID)
	_, err := svc.CreateDatasource(ctxBG(), ws.ID, "pg", "postgres", map[string]any{
		"host": "127.0.0.1", "port": "5432", "user": "godw", "database": "godw", "sslmode": "disable",
	}, map[string]string{"password": "godw"})
	if err == nil {
		t.Fatal("重复数据源应被拒绝")
	}
}

// 数据源不可达时 TestDatasource 必须落 status=error 并保留原因。
func TestTestDatasourceFailureRecordsStatus(t *testing.T) {
	svc, ws := newSvc(t)
	enc, err := svc.Cipher.Encrypt(map[string]string{"password": "definitely-wrong"})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	ds, err := svc.Repo.CreateDatasource(ctxBG(), model.Datasource{
		WorkspaceID: ws.ID, Name: "bad_auth", Type: "postgres",
		Config: map[string]any{"host": "127.0.0.1", "port": "5432", "user": "godw",
			"database": "godw", "sslmode": "disable"},
	}, enc)
	if err != nil {
		t.Fatalf("写入数据源失败: %v", err)
	}
	if err := svc.TestDatasource(ctxBG(), ws.ID, "bad_auth"); err == nil {
		t.Fatal("错误密码应测试失败")
	}
	stored, _, err := svc.Repo.DatasourceByName(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("读数据源失败: %v", err)
	}
	if stored.Status != "error" || stored.LastError == "" {
		t.Fatalf("失败应落库并保留原因: %+v", stored)
	}
}
