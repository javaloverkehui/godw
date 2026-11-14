package service_test

import (
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/semantic"
)

// dropTable 精确制造「某张元数据表不可用」。共享库不能 DROP，所以配合 faultSvc
// 用独占 schema；这样每条「仓储报错 → 服务如何降级/上抛」的分支都能被真实触发，
// 而不是靠 mock 猜返回值。
func dropTable(t *testing.T, db *repo.DB, name string) {
	t.Helper()
	if _, err := db.Pool.Exec(ctxBG(), "DROP TABLE IF EXISTS "+name+" CASCADE"); err != nil {
		t.Fatalf("删除表 %s 失败: %v", name, err)
	}
}

// model 表不可用：所有依赖「已发布模型集合」的入口都必须在第一跳就失败，
// 不能带着空映射继续算出一个看起来正常的空结果。
func TestFaultModelTableMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_fault_ds", "dw", "dwd_fault_ds"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_fault", "dwd_fault_ds", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	dropTable(t, db, "model")
	// 数据集存在但基础模型集合读不到：必须报错，不能返回空结果。
	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_fault", Dimensions: []string{"dt"}, Measures: []string{"amount"},
	}, "", ""); err == nil {
		t.Fatal("PublishedModels 失败时数据集查询应报错")
	}

	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err == nil {
		t.Fatal("CatalogDocs 在 model 表不可用时应失败")
	}
	if _, err := svc.ResolveSelect(ctxBG(), ws.ID, "any"); err == nil {
		t.Fatal("ResolveSelect 应失败")
	}
	// ColumnGraph 的模型补充信息失败时降级为空映射（不阻塞图本身）。
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, "some_t", "", "", 2); err != nil {
		t.Fatalf("ColumnGraph 应降级而不是报错: %v", err)
	}
	// 聚焦图的分层信息失败时降级为无色节点。
	if _, err := svc.LineageFocus(ctxBG(), ws.ID, "some_t", "up", 2); err != nil {
		t.Fatalf("LineageFocus 应降级而不是报错: %v", err)
	}
	// 列级血缘的「物理表 → 模型名」映射读不到时，仍按原表名落库。
	svc.RecordColumnLineage(ctxBG(), ws.ID, model.PipelineSpec{
		Name: "lineage_fault",
		Nodes: []model.NodeSpec{{
			ID: "w", Type: model.NodeSQL, Model: "dwd_x",
			SQL: "SELECT o.id AS id FROM dw.dwd_src_fault o",
		}},
	})
	// 模型发布落库失败要给可读错误，而不是半写版本。
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a",
		modelSpec(ds.Name, "dwd_fault_model", "dw", "dwd_fault_model"), true, false, true); err == nil {
		t.Fatal("SaveModel 失败时应报错")
	}
}

// model_version 表不可用：文档站的版本计数与回滚预览都要报错，
// 不能把「读不到版本」当成「只有一版」。
func TestFaultModelVersionMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_mv", "dw", "dwd_mv"))
	dropTable(t, db, "model_version")

	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err == nil {
		t.Fatal("CatalogDocs 应失败")
	}
	if _, err := svc.RollbackPreview(ctxBG(), ws.ID, "dwd_mv", 1); err == nil {
		t.Fatal("RollbackPreview 应失败")
	}
}

// pipeline 表不可用：文档站与选择器解析都要明确失败，避免「静默按空管道清单触发」。
func TestFaultPipelineTableMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "pipeline")
	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err == nil {
		t.Fatal("CatalogDocs 应失败")
	}
	if _, err := svc.ResolveSelect(ctxBG(), ws.ID, "any"); err == nil {
		t.Fatal("ResolveSelect 应失败")
	}
}

// task_instance 表不可用：文档站的最近运行、质量放行、执行推进都要报错，
// 而不是把「读不到任务」当成「没有任务」。
func TestFaultTaskTableMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("fault_task"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "fault_task", "2026-04-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "failed", "boom"); err != nil {
		t.Fatalf("置失败失败: %v", err)
	}
	// 第二个实例要在删表前建好：删表后连任务行都插不进去。
	inst2, _, err := svc.Trigger(ctxBG(), ws.ID, "fault_task", "2026-04-02", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	dropTable(t, db, "task_instance")

	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err == nil {
		t.Fatal("CatalogDocs 应失败")
	}
	if err := svc.WaiveQuality(ctxBG(), ws.ID, inst.ID, "", "r", "a"); err == nil {
		t.Fatal("WaiveQuality 读任务失败时应报错")
	}
	// 执行推进：读任务失败 → 实例落 failed。
	if _, err := svc.RunOnce(ctxBG(), "w1"); err == nil {
		t.Fatal("执行读任务失败时应返回错误")
	}
	// GetInstance 依赖 task_instance，这里直接查实例表确认收尾状态。
	var status string
	if err := db.Pool.QueryRow(ctxBG(), `SELECT status FROM instance WHERE id=$1`, inst2.ID).Scan(&status); err != nil {
		t.Fatalf("读实例状态失败: %v", err)
	}
	if status != "failed" {
		t.Fatalf("实例应收尾为 failed: %s", status)
	}
}

// quality_result 表不可用：文档站的质量回填失败要报错；执行侧保存失败只告警，
// 不能因为质量结果落不了库就把整个实例判失败。
func TestFaultQualityResultMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint)`, p.table("dwd_fault_q_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-01-01',1)`, p.table("dwd_fault_q_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_fault_q", p.schema, "dwd_fault_q_t"))
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "fault_quality",
		Nodes: []model.NodeSpec{{ID: "q", Type: model.NodeQuality, Model: "dwd_fault_q",
			Config: map[string]any{"rules": []any{
				map[string]any{"name": "id_nn", "kind": "not_null", "column": "id"},
			}}}}})
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "fault_quality", "2026-04-03", "manual", nil); err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	dropTable(t, db, "quality_result")

	if _, err := svc.CatalogDocs(ctxBG(), ws.ID); err == nil {
		t.Fatal("CatalogDocs 应失败")
	}
	// 执行仍应成功：质量结果写不进去只记日志。
	if _, err := svc.RunOnce(ctxBG(), "w1"); err != nil {
		t.Fatalf("质量结果落库失败不应让实例失败: %v", err)
	}
}

// column_policy 表不可用：策略命中测试、数据字典、导出分级都必须报错。
// 静默按「没有策略」处理等于把敏感数据放行。
func TestFaultColumnPolicyMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_cp", "dw", "dwd_cp"))
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_cp", "dwd_cp")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	dropTable(t, db, "column_policy")

	if _, err := svc.SimulatePolicies(ctxBG(), ws.ID, "t", "u", "viewer"); err == nil {
		t.Fatal("SimulatePolicies 应失败")
	}
	if _, err := svc.Dictionary(ctxBG(), ws.ID); err == nil {
		t.Fatal("Dictionary 应失败")
	}
	if _, _, err := svc.ExportSensitivity(ctxBG(), ws.ID, "metric", "gmv_cp"); err == nil {
		t.Fatal("ExportSensitivity 应失败")
	}
}

// row_policy 表不可用：策略命中测试与查询注入都要报错，避免「以为有行级隔离」。
func TestFaultRowPolicyMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "row_policy")
	if _, err := svc.SimulatePolicies(ctxBG(), ws.ID, "t", "u", "viewer"); err == nil {
		t.Fatal("SimulatePolicies 应失败")
	}
	if _, err := svc.ApplyRowFilters(ctxBG(), ws.ID, "t", "u", "viewer"); err == nil {
		t.Fatal("ApplyRowFilters 应失败")
	}
}

// lineage_edge 表不可用：模型/管道发布时血缘写入失败只记日志（发布不能被血缘拖住），
// 但选择器与数据字典读血缘失败必须报错，否则会按「没有上下游」给出错误结论。
func TestFaultLineageEdgeMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_le_src", "dw", "dwd_le_src"))
	if err := svc.Repo.ReplaceLineage(ctxBG(), ws.ID, "dwd_le_src", []string{"ods_x"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	dropTable(t, db, "lineage_edge")

	// 发布模型：AddLineage 失败 → 告警但发布成功。
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "a",
		modelSpec(ds.Name, "dwd_le_new", "dw", "dwd_le_new"), true, false, true); err != nil {
		t.Fatalf("血缘写入失败不应阻断发布: %v", err)
	}
	// 发布管道：ReplaceLineage 失败 → 告警但发布成功。
	pl := `kind: Pipeline
name: le_pipe
nodes:
  - id: s
    type: sql
    model: dwd_le_src
    datasource: ` + ds.Name + `
    sql: SELECT 1 AS id
`
	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "a", []byte(pl), true); err != nil {
		t.Fatalf("血缘写入失败不应阻断管道发布: %v", err)
	}
	if _, err := svc.ResolveSelect(ctxBG(), ws.ID, "dwd_le_src"); err == nil {
		t.Fatal("ResolveSelect 读血缘失败应报错")
	}
	if _, err := svc.Dictionary(ctxBG(), ws.ID); err == nil {
		t.Fatal("Dictionary 读血缘失败应报错")
	}
}

// lineage_column 表不可用：列级血缘写入失败必须降级为表级，不阻断发布。
func TestFaultLineageColumnMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "lineage_column")
	svc.RecordColumnLineage(ctxBG(), ws.ID, model.PipelineSpec{
		Name: "col_fault",
		Nodes: []model.NodeSpec{{
			ID: "w", Type: model.NodeSQL, Model: "dwd_c",
			SQL: "SELECT o.id AS id FROM dw.dwd_c_src o",
		}},
	})
}

// dataset 表不可用：画像/敏感识别的列来源缺失、采集回写失败都要报错或落失败样本。
func TestFaultDatasetTableMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	dropTable(t, db, "dataset")

	if _, err := svc.Profile(ctxBG(), ws.ID, ds.Name, "public", "t", 10); err == nil {
		t.Fatal("Profile 应失败")
	}
	if _, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 10); err == nil {
		t.Fatal("DetectPII 应失败")
	}
	// 采集仍返回表数，但回写失败会计入 failures；整体不报错（单表失败不阻断）。
	if _, err := svc.CollectMetadata(ctxBG(), ws.ID, ds.Name); err != nil {
		t.Fatalf("单表回写失败不应让采集整体失败: %v", err)
	}
}

// chart 表不可用：保存图表必须报错，而不是返回一个没落库的对象。
func TestFaultChartTableMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	seedSemantic(t, svc, ws.ID)
	dropTable(t, db, "chart")
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
		Name: "c_fault", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}); err == nil {
		t.Fatal("SaveChart 失败时应报错")
	}
}

// schema_change 表不可用：结构变更识别成功但落库失败只告警，采集结果仍要返回。
func TestFaultSchemaChangeMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint)`, p.table("fault_sc_t"))
	if _, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name); err != nil {
		t.Fatalf("首次采集失败: %v", err)
	}
	dropTable(t, db, "schema_change")
	p.exec(t, `ALTER TABLE %s ADD COLUMN note text`, p.table("fault_sc_t"))
	res, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("落库失败不应让采集失败: %v", err)
	}
	if len(res.Changes) == 0 {
		t.Fatalf("变更识别应仍然返回: %+v", res)
	}
}

// alert_rule 表不可用：告警判定加载规则失败只记录日志，绝不影响实例收尾。
func TestFaultAlertRuleMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "alert_rule")
	svc.EvaluateAlerts(ctxBG(), ws.ID, &model.Instance{
		ID: "i", WorkspaceID: ws.ID, PipelineName: "p", Status: "failed", BizDate: "2026-01-01",
	})
}

// alert_event 表不可用：事件落库失败只告警，不 panic、不中断后续规则。
func TestFaultAlertEventMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	if _, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "fail", Kind: alert.KindFailure, Target: "*", Severity: "warning", Enabled: true,
		Channels: []string{"webhook"},
	}); err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	dropTable(t, db, "alert_event")
	svc.EvaluateAlerts(ctxBG(), ws.ID, &model.Instance{
		ID: "i", WorkspaceID: ws.ID, PipelineName: "p", Status: "failed", BizDate: "2026-01-01",
	})
}

// resource_group 表不可用：环境初始化必须报错（没有默认资源组调度会失控）。
func TestFaultResourceGroupMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "resource_group")
	if err := svc.EnsureEnvironments(ctxBG(), ws.ID); err == nil {
		t.Fatal("EnsureEnvironments 应失败")
	}
}

// env_binding 表不可用：别名解析必须报错，不能退化成「按别名当数据源名」。
func TestFaultEnvBindingMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	dropTable(t, db, "env_binding")
	if _, err := svc.ResolveDatasource(ctxBG(), ws.ID, "prod", "pg_dw"); err == nil {
		t.Fatal("ResolveDatasource 应失败")
	}
}

// backfill_batch 表不可用：补数批次落库失败要报错，否则用户拿不到可追踪的批次。
func TestFaultBackfillBatchMissing(t *testing.T) {
	svc, ws, db := faultSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("fault_bf"))
	dropTable(t, db, "backfill_batch")
	if _, err := svc.Backfill(ctxBG(), ws.ID, "fault_bf", "2026-05-01", "2026-05-02", "r", "a", false); err == nil {
		t.Fatal("SaveBackfillBatch 失败时应报错")
	}
}

// service 层自己的兜底分支：指标预览/查询在「模型在、数据源不在」时的错误传播。
func TestMetricDatasourceRenamed(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	ds := dsNameOf(t, svc, ws.ID)
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE datasource SET name=$2 WHERE workspace_id=$1 AND name=$3`, ws.ID, ds+"_gone", ds); err != nil {
		t.Fatalf("改名数据源失败: %v", err)
	}
	if _, err := svc.PreviewMetric(ctxBG(), ws.ID, metricFor("gmv_m", "dwd_sem"), semantic.Query{}, "", ""); err == nil {
		t.Fatal("数据源不存在时预览应失败")
	}
	if _, err := svc.Repo.SaveMetric(ctxBG(), ws.ID, semantic.Metric{
		Name: "gmv_orphan", DisplayName: "g", ModelName: "dwd_sem",
		Agg: "sum", Expr: "amount", TimeColumn: "dt",
	}); err != nil {
		t.Fatalf("写入指标失败: %v", err)
	}
	if _, err := svc.QueryMetricFor(ctxBG(), ws.ID, "gmv_orphan", semantic.Query{}, "", ""); err == nil {
		t.Fatal("数据源不存在时查询应失败")
	}
}

// 指标表达式引用不存在的列：执行期要报错，而不是返回空序列让人以为「没有数据」。
func TestQueryMetricBadExpr(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, semantic.Metric{
		Name: "gmv_badexpr", ModelName: "dwd_sem", Agg: "sum", Expr: "no_such_col",
	}); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.QueryMetric(ctxBG(), ws.ID, "gmv_badexpr", semantic.Query{}); err == nil {
		t.Fatal("引用不存在列时应报错")
	}
}

// 指标默认值：时间列与展示名留空时补默认，前端只需要传口径。
func TestApplyMetricDefaults(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	saved, err := svc.ApplyMetric(ctxBG(), ws.ID, semantic.Metric{
		Name: "gmv_defaults", ModelName: "dwd_sem", Agg: "sum", Expr: "amount",
	})
	if err != nil {
		t.Fatalf("ApplyMetric 失败: %v", err)
	}
	if saved.TimeColumn != "dt" || saved.DisplayName != "gmv_defaults" {
		t.Fatalf("应补默认时间列与展示名: %+v", saved)
	}
}
