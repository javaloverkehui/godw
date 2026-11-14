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

// 文档站的新鲜度有四种来源：质量规则、最近成功、最近失败、没有运行。
// 这四种在 HTML 里必须给出不同的文案与颜色，否则「数据新不新」就变成一句空话。
func TestCatalogDocsFreshnessMatrix(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	// runDoc 发布一个模型并跑一条写表管道，返回实例 id。
	runDoc := func(modelName, pipelineName, layer, status string, rules []any) string {
		spec := modelSpec(ds.Name, modelName, "dw", modelName)
		spec.Layer = layer
		publishModel(t, svc, ws.ID, spec)
		nodes := []model.NodeSpec{{ID: "w", Type: model.NodeSQL, Model: modelName, SQL: "SELECT 1 AS id"}}
		if rules != nil {
			nodes = append(nodes, model.NodeSpec{ID: "q", Type: model.NodeQuality, Model: modelName,
				DependsOn: []string{"w"}, Config: map[string]any{"rules": rules}})
		}
		publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: pipelineName, Nodes: nodes})
		inst, _, err := svc.Trigger(ctxBG(), ws.ID, pipelineName, "2026-06-01", "manual", nil)
		if err != nil {
			t.Fatalf("触发失败: %v", err)
		}
		if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "w", status, 10, 1, ""); err != nil {
			t.Fatalf("置任务状态失败: %v", err)
		}
		if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, status, ""); err != nil {
			t.Fatalf("置实例状态失败: %v", err)
		}
		return inst.ID
	}

	// MA：质量规则通过 + 30 分钟阈值 → 规则判定为新鲜（humanAge 分钟）。
	instA := runDoc("dwd_fresh_a", "doc_a", "dwd", "success",
		[]any{map[string]any{"name": "fresh_a", "kind": "freshness", "max_age_min": 30}})
	if err := svc.Repo.SaveQualityResults(ctxBG(), instA, "q", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{"fresh_a", "5 分钟", "30 分钟", "error", true}}); err != nil {
		t.Fatalf("写质量结果失败: %v", err)
	}

	// MB：运行失败 + 120 分钟阈值 → 过期（humanAge 小时）。
	runDoc("dwd_fresh_b", "doc_b", "dwd", "failed",
		[]any{map[string]any{"name": "fresh_b", "kind": "freshness", "max_age_min": 120}})

	// MC：运行成功 + 5000 分钟阈值，但没有质量结果 → 按运行时间判新鲜（humanAge 天）。
	runDoc("dwd_fresh_c", "doc_c", "dwd", "success",
		[]any{map[string]any{"name": "fresh_c", "kind": "freshness", "max_age_min": 5000}})

	// MD：成功运行且无规则 → unknown + 最近成功（humanAge 不到 1 分钟）。
	runDoc("dwd_fresh_d", "doc_d", "dwd", "success", nil)

	// ME：质量规则未通过 → 以规则为准判过期。
	instE := runDoc("dwd_fresh_e", "doc_e", "dwd", "success",
		[]any{map[string]any{"name": "fresh_e", "kind": "freshness", "max_age_min": 60}})
	if err := svc.Repo.SaveQualityResults(ctxBG(), instE, "q", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{"fresh_e", "2 小时", "1 小时", "error", false}}); err != nil {
		t.Fatalf("写质量结果失败: %v", err)
	}

	// ads 层用于验证排序：dwd 必须排在 ads 前面。
	ads := modelSpec(ds.Name, "ads_fresh_top", "dw", "ads_fresh_top")
	ads.Layer = "ads"
	publishModel(t, svc, ws.ID, ads)

	// 异常配置：质量节点没有 rules、rules 里混入非对象项，解析时都要跳过。
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "doc_nil_rules",
		Nodes: []model.NodeSpec{
			{ID: "w", Type: model.NodeSQL, Model: "dwd_fresh_d", SQL: "SELECT 1 AS id"},
			{ID: "q", Type: model.NodeQuality, Model: "dwd_fresh_d", DependsOn: []string{"w"}},
		}})
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "doc_bad_rules",
		Nodes: []model.NodeSpec{
			{ID: "w", Type: model.NodeSQL, Model: "dwd_fresh_c", SQL: "SELECT 1 AS id"},
			{ID: "q", Type: model.NodeQuality, Model: "dwd_fresh_c", DependsOn: []string{"w"},
				Config: map[string]any{"rules": []any{"not-a-map"}}},
		}})
	// 只含质量节点的运行：运行记录没有对应的写表模型，不应被当成「模型刷新」。
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "doc_quality_only",
		Nodes: []model.NodeSpec{{ID: "q", Type: model.NodeQuality, Model: "dwd_fresh_a",
			Config: map[string]any{"rules": []any{map[string]any{"name": "x", "kind": "not_null", "column": "id"}}}}}})
	qinst, _, err := svc.Trigger(ctxBG(), ws.ID, "doc_quality_only", "2026-06-02", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.Repo.FinishTask(ctxBG(), qinst.ID, "q", "success", 1, 1, ""); err != nil {
		t.Fatalf("置任务成功失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), qinst.ID, "success", ""); err != nil {
		t.Fatalf("置实例成功失败: %v", err)
	}

	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogDocs 失败: %v", err)
	}
	byName := map[string]service.CatalogDoc{}
	for _, d := range page.Entries {
		byName[d.Name] = d
	}
	if f := byName["dwd_fresh_a"].Freshness; f.Basis != "quality_rule" || f.Status != "fresh" || f.ThresholdMin != 30 {
		t.Fatalf("规则通过应为 fresh: %+v", f)
	}
	if f := byName["dwd_fresh_b"].Freshness; f.Basis != "last_failed" || f.Status != "stale" || f.ThresholdMin != 120 {
		t.Fatalf("失败运行应过期: %+v", f)
	}
	if f := byName["dwd_fresh_c"].Freshness; f.Basis != "last_success" || f.Status != "fresh" || f.ThresholdMin != 5000 {
		t.Fatalf("无质量结果时按运行时间判新鲜: %+v", f)
	}
	if f := byName["dwd_fresh_d"].Freshness; f.Basis != "last_success" || f.Status != "unknown" {
		t.Fatalf("无规则时应为 unknown: %+v", f)
	}
	if f := byName["dwd_fresh_e"].Freshness; f.Basis != "quality_rule" || f.Status != "stale" {
		t.Fatalf("规则未通过应过期: %+v", f)
	}
	// dwd 排在 ads 前面。
	if len(page.Layers) < 2 || page.Layers[0] != "dwd" || page.Layers[1] != "ads" {
		t.Fatalf("分层顺序不对: %v", page.Layers)
	}

	html, err := svc.CatalogHTML(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogHTML 失败: %v", err)
	}
	for _, want := range []string{
		"数据新鲜（质量规则判定，阈值 30 分钟）",
		"数据过期（质量规则未通过",
		"过期（",
		"小时）",
		"天）",
		"未知（最近成功 不到 1 分钟 前，未配新鲜度规则）",
		"成功 · ",
		"失败 · ",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML 缺少 %q", want)
		}
	}
}

// 同一模型被两条管道写、且完成时间相同时，应稳定保留一条；
// 版本快照缺失的管道要被跳过而不是 panic。
func TestCatalogDocsDuplicateRunsAndMissingSpec(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dup_run", "dw", "dwd_dup_run"))
	var insts []string
	for _, name := range []string{"dup_pipe_a", "dup_pipe_b"} {
		publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: name,
			Nodes: []model.NodeSpec{{ID: "w", Type: model.NodeSQL, Model: "dwd_dup_run", SQL: "SELECT 1"}}})
		inst, _, err := svc.Trigger(ctxBG(), ws.ID, name, "2026-06-03", "manual", nil)
		if err != nil {
			t.Fatalf("触发失败: %v", err)
		}
		if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "w", "success", 1, 1, ""); err != nil {
			t.Fatalf("置任务成功失败: %v", err)
		}
		if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "success", ""); err != nil {
			t.Fatalf("置实例成功失败: %v", err)
		}
		insts = append(insts, inst.ID)
	}
	// 把两次完成时间改成完全一致，触发「不晚于上一次就跳过」。
	for _, id := range insts {
		if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
			`UPDATE task_instance SET finished_at=now() WHERE instance_id=$1`, id); err != nil {
			t.Fatalf("改完成时间失败: %v", err)
		}
	}
	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err != nil {
		t.Fatalf("CatalogDocs 失败: %v", err)
	}

	// 把一条管道版本的快照删掉：p.Spec 为 nil 时跳过它。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM pipeline_version WHERE pipeline_id=(SELECT id FROM pipeline WHERE name='dup_pipe_a')`); err != nil {
		t.Fatalf("删版本失败: %v", err)
	}
	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("快照缺失时不应失败: %v", err)
	}
	if len(page.Entries) == 0 || page.Entries[0].LastRun == nil {
		t.Fatalf("仍应保留另一条管道的运行: %+v", page.Entries)
	}
}

// 多环境解析的边界：环境不存在时绑定报错；绑定后数据源被删，解析要报错而不是
// 返回一个空名字。别名不存在且没有同名数据源时给 NotFound。
func TestEnvironmentEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	if err := svc.EnsureEnvironments(ctxBG(), ws.ID); err != nil {
		t.Fatalf("EnsureEnvironments 失败: %v", err)
	}
	if err := svc.BindEnv(ctxBG(), ws.ID, "prod", "pg_dw", ds.Name); err != nil {
		t.Fatalf("BindEnv 失败: %v", err)
	}
	// 绑定指向的数据源被删：解析时要报错，而不是返回空串。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(), `DELETE FROM datasource WHERE id=$1`, ds.ID); err != nil {
		t.Fatalf("删除数据源失败: %v", err)
	}
	if _, err := svc.ResolveDatasource(ctxBG(), ws.ID, "prod", "pg_dw"); err == nil {
		t.Fatal("绑定失效时应报错")
	}
}

// 结构变更的排序在「同表多列变更」时要按列名稳定：add 与 drop 混在一起，
// 顺序漂移会让复核者以为漏看了一条。
func TestCollectWithDiffSortsByColumn(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, zzz text)`, p.table("dwd_sort_t"))
	if _, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name); err != nil {
		t.Fatalf("首次采集失败: %v", err)
	}
	p.exec(t, `ALTER TABLE %s DROP COLUMN zzz`, p.table("dwd_sort_t"))
	p.exec(t, `ALTER TABLE %s ADD COLUMN aaa text`, p.table("dwd_sort_t"))
	res, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("二次采集失败: %v", err)
	}
	var cols []string
	for _, c := range res.Changes {
		if c.Table == "dwd_sort_t" {
			cols = append(cols, c.Column)
		}
	}
	if len(cols) < 2 || cols[0] > cols[1] {
		t.Fatalf("同表变更应按列名升序: %v", cols)
	}
}

// 敏感识别的采样分支：列名没有命中规则时按值判定手机/IP/邮箱/证件，
// 空字符串计入采样但不计命中；采样查询失败要安静返回 none。
func TestDetectPIISamplingBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (c1 text, c2 text, c3 text, c4 text, c5 text)`, p.table("pii_neutral"))
	p.exec(t, `INSERT INTO %s VALUES
		('13800000001','10.0.0.1','a@b.com','110101199001011234',''),
		('13800000002','10.0.0.2','c@d.com','110101199001011235','x'),
		('13800000003','10.0.0.3','e@f.com','110101199001011236','y')`, p.table("pii_neutral"))
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "pii_neutral",
		Columns: []model.ColumnSpec{
			{Name: "c1", Type: "text"}, {Name: "c2", Type: "text"}, {Name: "c3", Type: "text"},
			{Name: "c4", Type: "text"}, {Name: "c5", Type: "text"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写 dataset 失败: %v", err)
	}
	findings, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 200)
	if err != nil {
		t.Fatalf("DetectPII 失败: %v", err)
	}
	level := map[string]service.PIILevel{}
	for _, f := range findings {
		level[f.Column] = f.Level
	}
	if level["c1"] != service.PIIMedium {
		t.Fatalf("手机号列应判 medium: %+v", level)
	}
	if level["c2"] != service.PIILow {
		t.Fatalf("IP 列应判 low: %+v", level)
	}
	if level["c3"] != service.PIIMedium {
		t.Fatalf("邮箱列应判 medium: %+v", level)
	}
	if level["c4"] != service.PIIHigh {
		t.Fatalf("证件列应判 high: %+v", level)
	}

	// 采样查询本身失败（表被删）要安静返回 none，不影响其它列/其他表。
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "pii_missing",
		Columns: []model.ColumnSpec{{Name: "c1", Type: "text"}},
	}, ws.ID); err != nil {
		t.Fatalf("写 dataset 失败: %v", err)
	}
	if _, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 10); err != nil {
		t.Fatalf("采样失败不应让整体报错: %v", err)
	}
}

// 高亮片段要能处理长文本：关键词在中间时前后加省略号，且切片不能切在
// UTF-8 字节中间（中文会变成乱码）。
func TestSearchHighlightLongChinese(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_highlight", "dw", "dwd_highlight")
	spec.Description = strings.Repeat("前置描述内容", 10) + "订单" + strings.Repeat("后置描述内容", 10)
	publishModel(t, svc, ws.ID, spec)
	hits, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "订单"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	var hl string
	for _, h := range hits {
		if h.Name == "dwd_highlight" {
			hl = h.Highlight
		}
	}
	if hl == "" || !strings.Contains(hl, "<em>订单</em>") {
		t.Fatalf("应高亮关键词: %q", hl)
	}
	if !strings.HasPrefix(hl, "…") || !strings.HasSuffix(hl, "…") {
		t.Fatalf("长文本片段应前后省略: %q", hl)
	}
	if strings.ContainsRune(hl, '\uFFFD') {
		t.Fatalf("切片不应破坏 UTF-8: %q", hl)
	}
}

// 血缘归一化：带 schema 前缀但未登记的物理表要回退到裸表名映射；
// 两个模型同表名时裸名映射让位（置空），避免把上游错误归到某一张表。
func TestLineageTableNormalization(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_norm_src", "dw", "dwd_norm_src"))
	// 两个模型共用表名 dwd_shared，但 schema 不同：裸表名映射应被置空。
	a := modelSpec(ds.Name, "dwd_shared_a", "dw_a", "dwd_shared")
	b := modelSpec(ds.Name, "dwd_shared_b", "dw_b", "dwd_shared")
	publishModel(t, svc, ws.ID, a)
	publishModel(t, svc, ws.ID, b)

	svc.RecordColumnLineage(ctxBG(), ws.ID, model.PipelineSpec{
		Name: "norm_pipe",
		Nodes: []model.NodeSpec{{
			ID: "w", Type: model.NodeSQL, Model: "dwd_norm_dst",
			SQL: "SELECT o.id AS id, s.id AS sid FROM schema_not_registered.dwd_norm_src o, schema_not_registered.unknown_tbl s",
		}},
	})
	edges, err := svc.Repo.ColumnLineage(ctxBG(), ws.ID, "dwd_norm_dst", "")
	if err != nil {
		t.Fatalf("读列级血缘失败: %v", err)
	}
	var sawModel bool
	for _, e := range edges {
		if e.UpstreamTable == "dwd_norm_src" {
			sawModel = true
		}
	}
	if !sawModel {
		t.Fatalf("带 schema 前缀的已知表应归一化到模型名: %+v", edges)
	}
}

// 治理侧的零散分支：空过滤项跳过、空表名策略不命中、角色默认 viewer、
// 角色不匹配时不注入行条件。
func TestGovernanceEdgeBranches(t *testing.T) {
	if got := service.ApplyRowFilters([]string{"", "region = '${user}'"}, "alice", "viewer"); got != "(region = 'alice')" {
		t.Fatalf("空过滤项应跳过: %q", got)
	}

	svc, ws := newSvc(t)
	// 空表名的列策略：命中测试里不能匹配任何表。
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "", ColumnName: "x", Classification: "secret", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存空表名策略失败: %v", err)
	}
	// 空白过滤表达式：读取时要跳过而不是拼出 "()" 这种非法条件。
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "t_edge", Role: "viewer", FilterExpr: "   ",
	}); err != nil {
		t.Fatalf("保存空行策略失败: %v", err)
	}
	conds, err := svc.ApplyRowFilters(ctxBG(), ws.ID, "t_edge", "alice", "viewer")
	if err != nil || len(conds) != 0 {
		t.Fatalf("空白表达式应跳过: %v %v", conds, err)
	}
	// 角色留空 → 默认 viewer；没有命中行策略时给出提示。
	sim, err := svc.SimulatePolicies(ctxBG(), ws.ID, "t_edge", "alice", "")
	if err != nil {
		t.Fatalf("SimulatePolicies 失败: %v", err)
	}
	if sim.Role != "viewer" {
		t.Fatalf("空角色应默认 viewer: %+v", sim)
	}
	// admin 不需要「能读整表」的提示。
	sim, err = svc.SimulatePolicies(ctxBG(), ws.ID, "t_edge", "root", "admin")
	if err != nil {
		t.Fatalf("SimulatePolicies 失败: %v", err)
	}
	for _, n := range sim.Notes {
		if strings.Contains(n, "能读到整张表") {
			t.Fatalf("admin 不应出现整表提示: %+v", sim.Notes)
		}
	}

	// 预览二进制列：[]byte 要转成字符串再脱敏/返回。
	ds := newPgDatasource(t, svc, ws.ID)
	ph := newPhys(t)
	ph.exec(t, `CREATE TABLE %s (id bigint, raw bytea)`, ph.table("dwd_bytea_t"))
	ph.exec(t, `INSERT INTO %s VALUES (1, '\x6869')`, ph.table("dwd_bytea_t"))
	res, err := svc.PreviewTable(ctxBG(), ws.ID, ds.Name, ph.schema, "dwd_bytea_t", "", "", 10)
	if err != nil {
		t.Fatalf("PreviewTable 失败: %v", err)
	}
	rows, _ := res["rows"].([][]any)
	if len(rows) != 1 || rows[0][1] != "hi" {
		t.Fatalf("bytea 应转成字符串: %+v", rows)
	}
	// 数据源不存在：预览要报错。
	if _, err := svc.PreviewTable(ctxBG(), ws.ID, "ghost_ds", "s", "t", "", "", 10); err == nil {
		t.Fatal("数据源不存在应报错")
	}
}

// 模型 Spec 缺失时（版本被清理），血缘映射要跳过它而不是 panic。
func TestRecordColumnLineageSkipsNilSpec(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_nil_spec", "dw", "dwd_nil_spec"))
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE model SET active_version=99 WHERE workspace_id=$1 AND name='dwd_nil_spec'`, ws.ID); err != nil {
		t.Fatalf("改版本失败: %v", err)
	}
	svc.RecordColumnLineage(ctxBG(), ws.ID, model.PipelineSpec{
		Name: "nil_spec_pipe",
		Nodes: []model.NodeSpec{{ID: "w", Type: model.NodeSQL, Model: "dwd_dst",
			SQL: "SELECT o.id AS id FROM dw.dwd_nil_spec o"}},
	})
}

// 导出的其余异常：驳回已批准的任务、看板无数据、订阅默认目录等。
func TestExportEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	pending := func(kind, target string) *model.ExportJob {
		job, err := svc.Repo.CreateExport(ctxBG(), model.ExportJob{
			WorkspaceID: ws.ID, Kind: kind, Target: target,
			Params: map[string]any{"_classification": "high"}, Status: "pending_approval", CreatedBy: "a",
		})
		if err != nil {
			t.Fatalf("建待审批任务失败: %v", err)
		}
		return job
	}
	job := pending("metric", "m")
	if _, err := svc.ApproveExport(ctxBG(), ws.ID, job.ID, "admin"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	// 已批准 → 驳回必须冲突。
	if _, err := svc.RejectExport(ctxBG(), ws.ID, job.ID, "太晚", "admin"); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("批准后驳回应冲突，实际 %v", err)
	}
	_ = waitStatus(t, svc, ws.ID, job.ID)

	// 默认导出目录：ExportDir 置空时用 ./data/exports（目标不存在 → 任务失败）。
	svc.Cfg.Semantic.ExportDir = ""
	bad, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "nope_metric", nil, "a")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	if done := waitStatus(t, svc, ws.ID, bad.ID); done.Status != "failed" {
		t.Fatalf("目标不存在应失败: %+v", done)
	}

	// 看板不存在 → exportDashboard 直接报错。
	dashJob, err := svc.CreateExport(ctxBG(), ws.ID, "dashboard", "ghost_dash", nil, "a")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	if done := waitStatus(t, svc, ws.ID, dashJob.ID); done.Status != "failed" || done.Error == "" {
		t.Fatalf("看板不存在应失败: %+v", done)
	}

	// 看板有卡片但窗口内没有数据 → 导出一个空文件（0 行，成功）。
	ds := newPgDatasource(t, svc, ws.ID)
	ph := newPhys(t)
	ph.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, ph.table("dwd_empty_t"))
	ph.exec(t, `INSERT INTO %s VALUES ('2020-01-01',1,10,'a')`, ph.table("dwd_empty_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_empty", ph.schema, "dwd_empty_t"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_empty", "dwd_empty")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, dashboardFor("dash_empty", "gmv_empty")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	empty, err := svc.CreateExport(ctxBG(), ws.ID, "dashboard", "dash_empty", nil, "a")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	if done := waitStatus(t, svc, ws.ID, empty.ID); done.Status != "success" || done.RowCount != 0 {
		t.Fatalf("空看板应成功且 0 行: %+v", done)
	}
}

// 订阅渠道的分支：dashboard 成功落盘、file 默认目录、目录不可写、非法 endpoint。
func TestSubscriptionEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	ph := newPhys(t)
	ph.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, ph.table("dwd_sub2_t"))
	d := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	ph.exec(t, `INSERT INTO %s VALUES ('%s',1,10,'a')`, ph.table("dwd_sub2_t"), d)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_sub2", ph.schema, "dwd_sub2_t"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_sub2", "dwd_sub2")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, dashboardFor("dash_sub2", "gmv_sub2")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}

	// dashboard 订阅成功。
	sub, err := svc.Repo.SaveSubscription(ctxBG(), ws.ID, model.Subscription{
		Name: "dash_file", Kind: "dashboard", Target: "dash_sub2", Channel: "file", Enabled: true,
	})
	if err != nil {
		t.Fatalf("保存订阅失败: %v", err)
	}
	res, err := svc.RunSubscription(ctxBG(), ws.ID, *sub)
	if err != nil || res["path"] == "" {
		t.Fatalf("dashboard 订阅应落盘: %+v %v", res, err)
	}

	// ExportDir 为空 → 默认目录。
	svc.Cfg.Semantic.ExportDir = ""
	res, err = svc.RunSubscription(ctxBG(), ws.ID, *sub)
	if err != nil || res["path"] == "" {
		t.Fatalf("默认目录应可用: %+v %v", res, err)
	}

	// 目录不可写 → 报错。
	svc.Cfg.Semantic.ExportDir = "not/a/real/dir\x00bad"
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, *sub); err == nil {
		t.Fatal("非法目录应报错")
	}
	svc.Cfg.Semantic.ExportDir = t.TempDir()

	// 非法 endpoint → 构造请求失败。
	bad := model.Subscription{Name: "bad_hook", Kind: "metric", Target: "gmv_sub2",
		Channel: "webhook", Endpoint: "://bad"}
	if _, err := svc.RunSubscription(ctxBG(), ws.ID, bad); err == nil {
		t.Fatal("非法 endpoint 应报错")
	}
}

// 保留策略的剩余分支：未发布模型、数据源缺失、归档语句跳过、执行失败。
func TestRetentionEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "ghost_model", RetainDays: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}
	if _, err := svc.PlanRetention(ctxBG(), ws.ID, "ghost_model"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("未发布模型应 NotFound，实际 %v", err)
	}

	ds := newPgDatasource(t, svc, ws.ID)
	ph := newPhys(t)
	ph.exec(t, `CREATE TABLE %s (dt date, id bigint)`, ph.table("dwd_ret_arch_t"))
	old := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	ph.exec(t, `INSERT INTO %s VALUES ('%s',1)`, ph.table("dwd_ret_arch_t"), old)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_ret_arch", ph.schema, "dwd_ret_arch_t"))
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_arch", RetainDays: 7, ArchiveTo: "s3://b", Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}
	// 归档模式：注释与 COPY 跳过，只执行 DELETE。
	n, err := svc.RunRetention(ctxBG(), ws.ID, "dwd_ret_arch", "a")
	if err != nil || n != 1 {
		t.Fatalf("归档模式应只删过期行: %d %v", n, err)
	}

	// 数据源被改名：计划阶段就要报错。
	if err := svc.Repo.SaveRetentionPolicy(ctxBG(), ws.ID, model.RetentionPolicy{
		ModelName: "dwd_ret_arch", RetainDays: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}
	name := ds.Name
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE datasource SET name=$2 WHERE workspace_id=$1 AND name=$3`, ws.ID, name+"_gone", name); err != nil {
		t.Fatalf("改名数据源失败: %v", err)
	}
	if _, err := svc.PlanRetention(ctxBG(), ws.ID, "dwd_ret_arch"); err == nil {
		t.Fatal("数据源不存在时计划应报错")
	}
}

// 删除权：模型在、数据源被删时异步任务必须落 failed 并留原因。
func TestErasureConnectorFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	ph := newPhys(t)
	ph.exec(t, `CREATE TABLE %s (dt date, id bigint)`, ph.table("dwd_er_conn_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_er_conn", ph.schema, "dwd_er_conn_t"))
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(), `DELETE FROM datasource WHERE id=$1`, ds.ID); err != nil {
		t.Fatalf("删除数据源失败: %v", err)
	}
	job, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_er_conn", "id", "1", "r", "a")
	if err != nil {
		t.Fatalf("提交删除请求失败: %v", err)
	}
	done := waitErasure(t, svc, ws.ID, job.ID)
	if done.Status != "failed" || done.Error == "" {
		t.Fatalf("数据源缺失应落 failed: %+v", done)
	}
}

// 导出涉及的表解析：看板/数据集/未知类型的错误分支。
func TestExportTablesEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.ExportTables(ctxBG(), ws.ID, "dashboard", "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的看板应 NotFound，实际 %v", err)
	}
	if _, err := svc.ExportTables(ctxBG(), ws.ID, "dataset", "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的数据集应 NotFound，实际 %v", err)
	}
	// 未知类型：不涉及任何表（空列表），不报错。
	tables, err := svc.ExportTables(ctxBG(), ws.ID, "unknown", "x")
	if err != nil || len(tables) != 0 {
		t.Fatalf("未知类型应返回空表: %v %v", tables, err)
	}

	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_exp_edge", "dw", "dwd_exp_edge"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_exp_edge", "dwd_exp_edge", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	// 数据集基础模型下线 → 解析物理表失败。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE model SET active_version=0 WHERE workspace_id=$1 AND name='dwd_exp_edge'`, ws.ID); err != nil {
		t.Fatalf("下线模型失败: %v", err)
	}
	if _, err := svc.ExportTables(ctxBG(), ws.ID, "dataset", "ds_exp_edge"); err == nil {
		t.Fatal("基础模型下线时应报错")
	}
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE model SET active_version=1 WHERE workspace_id=$1 AND name='dwd_exp_edge'`, ws.ID); err != nil {
		t.Fatalf("恢复模型失败: %v", err)
	}
	// 看板卡片的指标被删 → 解析指标失败。
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_exp_edge", "dwd_exp_edge")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, dashboardFor("dash_exp_edge", "gmv_exp_edge")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM metric WHERE workspace_id=$1 AND name='gmv_exp_edge'`, ws.ID); err != nil {
		t.Fatalf("删除指标失败: %v", err)
	}
	if _, err := svc.ExportTables(ctxBG(), ws.ID, "dashboard", "dash_exp_edge"); err == nil {
		t.Fatal("卡片指标缺失时应报错")
	}
}

// DryRun / SampleRun 的剩余分支：参数默认值、显式 where、数据源缺失与不可达。
func TestDryRunAndSampleRunEdgeBranches(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	// DryRun：未发布模型 + 无 SQL 的 sink 节点 → firstNonEmpty 走第二参数。
	res, err := svc.DryRun(ctxBG(), ws.ID, model.PipelineSpec{
		Name: "dry_nosql",
		Nodes: []model.NodeSpec{{ID: "s", Type: model.NodeSink, Model: "ghost_model"}},
	}, "2026-07-01", nil)
	if err != nil {
		t.Fatalf("DryRun 失败: %v", err)
	}
	if !strings.Contains(res.Nodes[0].Error, "模型未发布") {
		t.Fatalf("未发布模型应写进节点错误: %+v", res.Nodes[0])
	}

	// SampleRun：参数默认值参与渲染；显式 where 进入增量条件。
	spec := model.PipelineSpec{
		Name:   "sample_defaults",
		Params: []model.ParamSpec{{Name: "n", Type: "int", Default: 3}},
		Nodes: []model.NodeSpec{{ID: "s", Type: model.NodeSQL, Datasource: ds.Name,
			Config: map[string]any{"where": "id > 0"},
			SQL:    "SELECT 1 AS id WHERE ${n} > 0"}},
	}
	out, err := svc.SampleRun(ctxBG(), ws.ID, spec, "s", "2026-07-01", 10)
	if err != nil {
		t.Fatalf("SampleRun 失败: %v", err)
	}
	if !strings.Contains(out.SQL, "3 > 0") {
		t.Fatalf("参数默认值应参与渲染: %s", out.SQL)
	}

	// 节点指定的数据源不存在 → 参数错误前先报数据源缺失。
	ghost := model.PipelineSpec{Name: "ghost_ds", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSQL, Datasource: "ghost_ds", SQL: "SELECT 1"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, ghost, "s", "2026-07-01", 10); err == nil {
		t.Fatal("数据源不存在应报错")
	}
	// 数据源存在但连不上 → 取连接失败。
	brokenDatasource(t, svc, ws.ID, "pg_broken3")
	broken := model.PipelineSpec{Name: "broken_ds", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSQL, Datasource: "pg_broken3", SQL: "SELECT 1"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, broken, "s", "2026-07-01", 10); err == nil {
		t.Fatal("数据源不可达应报错")
	}
}

// 选择器的边界：有效边但下游模型不存在、环、quality 节点不算写出模型。
func TestSelectorGraphEdgeBranches(t *testing.T) {
	models := []service.SelectModel{{Name: "a"}, {Name: "b"}}
	edges := []service.SelectEdge{
		{Up: "a", Down: "z"}, // 下游不存在
		{Up: "a", Down: "b"},
		{Up: "b", Down: "a"}, // 形成环：闭包必须终止
	}
	terms, _ := service.ParseSelect("a+")
	got, err := service.ExpandSelect(terms, models, edges, nil)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	wantSet(t, got, "a", "b")

	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_sel_q", "dw", "dwd_sel_q"))
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "sel_quality_only",
		Nodes: []model.NodeSpec{{ID: "q", Type: model.NodeQuality, Model: "dwd_sel_q"}}})
	// quality 节点不写出模型：解析仍成功，但不会命中该管道；未覆盖的模型要列出来。
	res2, err := svc.ResolveSelect(ctxBG(), ws.ID, "dwd_sel_q")
	if err != nil {
		t.Fatalf("ResolveSelect 失败: %v", err)
	}
	if len(res2.Pipelines) != 0 || !hasStr(res2.Uncovered, "dwd_sel_q") {
		t.Fatalf("质量节点不应算作写出模型: %+v", res2)
	}
}

// 建模：未知引擎退回 PG 方言；血缘去重要稳定。
func TestModelingUnknownEngineAndDedupe(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	enc, _ := svc.Cipher.Encrypt(map[string]string{"password": "x"})
	if _, err := svc.Repo.CreateDatasource(ctxBG(), model.Datasource{
		WorkspaceID: ws.ID, Name: "ora", Type: "oracle",
		Config: map[string]any{},
	}, enc); err != nil {
		t.Fatalf("写入 oracle 数据源失败: %v", err)
	}
	spec := modelSpec("ora", "dwd_ora", "dw", "dwd_ora")
	if _, err := svc.Repo.SaveModel(ctxBG(), ws.ID, spec, "", "", "t", true); err != nil {
		t.Fatalf("落模型失败: %v", err)
	}
	out, err := svc.ModelDiff(ctxBG(), ws.ID, spec)
	if err != nil {
		t.Fatalf("ModelDiff 失败: %v", err)
	}
	if out["engine"] != "oracle" {
		t.Fatalf("引擎应来自数据源: %+v", out)
	}
	if _, ok := out["plan"].(modelc.MigrationPlan); !ok {
		t.Fatalf("未知引擎也应给出迁移方案: %T", out["plan"])
	}

	// 重复的上游关系：字典里只应出现一次。
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dedupe", "dw", "dwd_dedupe"))
	for i := 0; i < 2; i++ {
		if err := svc.Repo.AddLineage(ctxBG(), ws.ID, "dwd_dedupe", []string{"ods_dup"}); err != nil {
			t.Fatalf("写血缘失败: %v", err)
		}
	}
	entries, err := svc.Dictionary(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("Dictionary 失败: %v", err)
	}
	for _, e := range entries {
		if e.Name == "dwd_dedupe" {
			n := 0
			for _, u := range e.Upstream {
				if u == "ods_dup" {
					n++
				}
			}
			if n > 1 {
				t.Fatalf("血缘应去重: %+v", e.Upstream)
			}
		}
	}
}

// 自助查询的默认 limit 与 numeric 特殊值：NULL/NaN 不能变成结构体字符串。
func TestRawSQLDefaultLimitAndNumeric(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	res, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name,
		`SELECT 'NaN'::numeric AS n, NULL::numeric AS z`, "", "", 0)
	if err != nil {
		t.Fatalf("RawSQLQuery 失败: %v", err)
	}
	if res.Limit != 500 {
		t.Fatalf("limit=0 应回到默认 500: %d", res.Limit)
	}
	for _, v := range res.Rows[0] {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "{") {
			t.Fatalf("numeric 不应输出结构体字符串: %q", s)
		}
	}
}

// 非数值类型的度量槽：bytea 应转成字符串再解析，布尔走 fmt.Sprint 兜底。
// 任何一路 panic 都会让整张图 500，而这只需要一列类型不对就会触发。
func TestChartByteaAndBoolMeasures(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), raw bytea, flag boolean)`, p.table("dwd_blob_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,1,'\x6869',true),('2026-08-01',2,2,'\x6f6b',false)`, p.table("dwd_blob_t"))
	spec := modelSpec(ds.Name, "dwd_blob", p.schema, "dwd_blob_t")
	spec.Columns = append(spec.Columns,
		model.ColumnSpec{Name: "raw", Type: "string", Nullable: true},
		model.ColumnSpec{Name: "flag", Type: "string", Nullable: true})
	publishModel(t, svc, ws.ID, spec)
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, semantic.Dataset{
		Name: "ds_blob", BaseModel: "dwd_blob",
		Dimensions: []semantic.Dimension{
			{Name: "dt", Field: "dt", Type: "time", Filterable: true, TimeGrains: []string{"day"}},
			{Name: "raw", Field: "raw", Type: "categorical", Filterable: true},
			{Name: "flag", Field: "flag", Type: "categorical", Filterable: true},
		},
		Measures: []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	for _, field := range []string{"raw", "flag"} {
		name := "c_blob_" + field
		if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
			Name: name, Type: "bar", Dataset: "ds_blob",
			Dimensions: []string{"dt", field}, Measures: []string{"amount"},
			Encoding: semantic.Encoding{X: "dt", Y: field},
		}); err != nil {
			t.Fatalf("保存图表 %s 失败: %v", name, err)
		}
		if _, err := svc.ChartQuery(ctxBG(), ws.ID, name, "", ""); err != nil {
			t.Fatalf("查询图表 %s 失败: %v", name, err)
		}
	}
}
