package repo_test

import (
	"context"
	"math"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 本文件的用例集中在「目录/模型/管道」写入链路的失败分支。
// 这些分支的共同点是：只有前一条语句成功、后一条语句失败时才会走到，
// 关连接池覆盖不到（第一条就挂了），所以用改列类型 / 加触发器的方式精确构造。
//
// 扫描失败一律挑「Go 侧目标不是 string」的列来构造：
// pgx 会把任意类型格式化成文本喂给 *string，改类型不会失败；
// 改成可空 NULL 或把 int/time/bool 列改成 text 才会真正报错。

// 工作区插入失败（例如写入被约束拒绝）必须抛错，
// 不能返回一个内存里造出来的工作区——那会让后续写入挂在不存在的外键上。
func TestEnsureWorkspaceFailsWhenInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	failOn(t, db, "workspace", "INSERT", "")

	_, err := r.EnsureWorkspace(context.Background(), "w")
	wantErr(t, err, "注入故障: workspace insert")
}

// 插入成功后回查失败时，返回值必须是错误而不是空结构体：
// 否则调用方会拿到 ID 为空的工作区，所有写入都会落到错误的地方。
func TestEnsureWorkspaceFailsWhenReadBackBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	renameAfterInsert(t, db, "workspace", "created_at", "created_at_drift")

	ws, err := r.EnsureWorkspace(context.Background(), "w")
	wantErr(t, err, "created_at")
	if ws != nil {
		t.Fatalf("回查失败时不应返回工作区，实际: %+v", ws)
	}
}

// created_at 变成文本（结构漂移）后，ListDatasources 必须报错，
// 而不是跳过这一行——数据源列表少条目比直接报错更难排查。
func TestListDatasourcesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	seedDatasource(t, r, ws.ID, "ds")
	textColumn(t, db, "datasource", "created_at")

	_, err := r.ListDatasources(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 数据集标签列漂移同理：采集快照读不出来时必须报错。
func TestListDatasetsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	ds := seedDatasource(t, r, ws.ID, "ds")
	if err := r.UpsertDataset(context.Background(), model.Dataset{
		Name: "orders", Schema: "public", DatasourceID: ds.ID,
	}, ws.ID); err != nil {
		t.Fatalf("写入数据集失败: %v", err)
	}
	textColumn(t, db, "dataset", "tags")

	_, err := r.ListDatasets(context.Background(), ws.ID, "")
	wantErr(t, err, "scan")
}

// model 表不存在（迁移没跑 / 表被误删）时，SaveModel 必须报错退出，
// 不能把它当成「模型不存在」继续插入版本。
func TestSaveModelFailsWhenModelTableMissing(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	dropTable(t, db, "model")

	_, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", false)
	wantErr(t, err, "model")
}

// 版本号计算失败必须回滚整个保存：宁可这次不保存，也不能写入版本号错误的快照。
func TestSaveModelFailsWhenVersionQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	dropTable(t, db, "model_version")

	_, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", false)
	wantErr(t, err, "model_version")
}

// 版本快照写入失败时不能留下「模型存在但没有任何版本」的半成品。
func TestSaveModelFailsWhenVersionInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "model_version", "INSERT", "")

	_, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true)
	wantErr(t, err, "注入故障: model_version insert")

	if n := queryInt(t, db, `SELECT count(*) FROM model`); n != 0 {
		t.Fatalf("版本写入失败时 model 必须随事务回滚，实际残留 %d 行", n)
	}
}

// 发布新版本时要归档旧版本；归档失败必须中止，否则会出现两个 published 版本。
func TestSaveModelFailsWhenArchiveRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	// 先发布 v1，第二次发布才会有「旧 published 版本」需要归档。
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}
	failOn(t, db, "model_version", "UPDATE", "NEW.status = 'archived'")

	_, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true)
	wantErr(t, err, "注入故障: model_version update")

	if n := queryInt(t, db, `SELECT count(*) FROM model_version WHERE status='published'`); n != 1 {
		t.Fatalf("归档失败后生效版本数应仍为 1，实际 %d", n)
	}
}

// 切换 active_version 失败时不能只写进版本表：
// 否则执行器读到的还是旧版本，而界面上已经显示「已发布」。
func TestSaveModelFailsWhenActiveVersionUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "model", "UPDATE", "")

	_, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true)
	wantErr(t, err, "注入故障: model update")
}

// 模型 updated_at 列漂移时 ListModels 必须报错（列表页宁可直接失败，也不要展示半份数据）。
func TestListModelsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	textColumn(t, db, "model", "updated_at")

	_, err := r.ListModels(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 版本列表的发布时间列漂移时必须报错。
func TestModelVersionsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	textColumn(t, db, "model_version", "published_at")

	_, err := r.ModelVersions(context.Background(), ws.ID, "m")
	wantErr(t, err, "scan")
}

// 回滚时版本存在性查询失败必须中止，不能默认「版本不存在」或「存在」。
func TestRollbackModelFailsWhenVersionLookupBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	renameColumn(t, db, "model_version", "version", "version_drift")

	err := r.RollbackModel(context.Background(), ws.ID, "m", 1)
	wantErr(t, err, "version")
}

// 回滚第一步（归档当前 published 版本）失败必须中止。
func TestRollbackModelFailsWhenArchiveRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	failOn(t, db, "model_version", "UPDATE", "")

	err := r.RollbackModel(context.Background(), ws.ID, "m", 1)
	wantErr(t, err, "注入故障: model_version update")
}

// 回滚第二步（把目标版本改成 published）失败必须中止：
// 只归档不发布会让模型处于「没有任何生效版本」的状态。
func TestRollbackModelFailsWhenPublishRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	failOn(t, db, "model_version", "UPDATE", "NEW.status = 'published'")

	err := r.RollbackModel(context.Background(), ws.ID, "m", 1)
	wantErr(t, err, "注入故障: model_version update")

	if v := queryStr(t, db, `SELECT status FROM model_version WHERE version=1`); v != "published" {
		t.Fatalf("回滚失败后不应改动版本状态，实际 %q", v)
	}
}

// 管道参数默认值可以是任意 YAML 值；不可序列化的值（NaN）必须报错，
// 否则会写出一个无法反序列化的 JSONB 快照。
func TestSavePipelineFailsOnUnmarshalableSpec(t *testing.T) {
	// 序列化失败发生在任何 SQL 之前，用共享库即可。
	r, _ := newRepoDB(t)
	ws := seedWorkspace(t, r, "w")
	spec := model.PipelineSpec{
		Name:   "p",
		Params: []model.ParamSpec{{Name: "rate", Type: "float", Default: math.NaN()}},
	}

	_, err := r.SavePipeline(context.Background(), ws.ID, spec, "tester", false)
	wantErr(t, err, "NaN")
}

// pipeline 表缺失时 SavePipeline 必须报错，不能把它当成新建管道。
func TestSavePipelineFailsWhenTableMissing(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	dropTable(t, db, "pipeline")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p"}, "tester", false)
	wantErr(t, err, "pipeline")
}

// 更新已有管道失败必须中止，不能继续插入新版本（否则版本指向一个没更新成功的定义）。
func TestSavePipelineFailsWhenPipelineUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "p"}, false)
	failOn(t, db, "pipeline", "UPDATE", "")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p", Description: "改"}, "tester", false)
	wantErr(t, err, "注入故障: pipeline update")
}

// 版本号计算失败必须回滚（新建管道时 pipeline 行也不能留下）。
func TestSavePipelineFailsWhenVersionQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	dropTable(t, db, "pipeline_version")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p"}, "tester", false)
	wantErr(t, err, "pipeline_version")

	if n := queryInt(t, db, `SELECT count(*) FROM pipeline`); n != 0 {
		t.Fatalf("版本写入失败时管道行必须回滚，实际残留 %d 行", n)
	}
}

// 版本快照写入失败必须中止。
func TestSavePipelineFailsWhenVersionInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "pipeline_version", "INSERT", "")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p"}, "tester", true)
	wantErr(t, err, "注入故障: pipeline_version insert")
}

// 发布时归档旧版本失败必须中止——否则两个版本同时 published，
// 回滚与重跑都找不到唯一快照。
func TestSavePipelineFailsWhenArchiveRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "p"}, true)
	failOn(t, db, "pipeline_version", "UPDATE", "NEW.status = 'archived'")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p"}, "tester", true)
	wantErr(t, err, "注入故障: pipeline_version update")

	if n := queryInt(t, db, `SELECT count(*) FROM pipeline_version WHERE status='published'`); n != 1 {
		t.Fatalf("归档失败后生效版本数应仍为 1，实际 %d", n)
	}
}

// 切换 active_version 失败必须中止：否则新版本已入库但调度仍认旧版本。
func TestSavePipelineFailsWhenActiveVersionUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "pipeline", "UPDATE", "")

	_, err := r.SavePipeline(context.Background(), ws.ID, model.PipelineSpec{Name: "p"}, "tester", true)
	wantErr(t, err, "注入故障: pipeline update")
}

// 管道创建时间列漂移时管道列表必须报错。
func TestListPipelinesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	seedPipeline(t, r, ws.ID, "p")
	textColumn(t, db, "pipeline", "created_at")

	_, err := r.ListPipelines(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 调度器扫描「可调度管道」时并发度列漂移必须报错，
// 静默跳过会让任务永远不触发。
func TestListEnabledPipelinesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	savePipeline(t, r, ws.ID, model.PipelineSpec{Name: "p", Schedule: "0 2 * * *"}, true)
	textColumn(t, db, "pipeline", "concurrency")

	_, err := r.ListEnabledPipelines(context.Background())
	wantErr(t, err, "scan")
}

// 指标单位列被写成 NULL（上游漏填）时，ListMetrics 必须报错。
func TestListMetricsFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveMetric(context.Background(), ws.ID, metricSample("gmv")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	nullify(t, db, "metric", "unit")

	_, err := r.ListMetrics(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}

// 看板描述为 NULL 时看板列表必须报错，不能把 NULL 当成空串悄悄放过。
func TestListDashboardsFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveDashboard(context.Background(), ws.ID, dashboardSample("d", "x")); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	nullify(t, db, "dashboard", "description")

	_, err := r.ListDashboards(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}

// 采集回来的列定义不是数组（例如上游写成了对象）时必须报错，
// 而不是返回空列让建模脚手架生成一张没有字段的表。
func TestDatasetColumnsByNameFailsOnMalformedJSON(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	ds := seedDatasource(t, r, ws.ID, "ds")
	if err := r.UpsertDataset(context.Background(), model.Dataset{
		Name: "orders", Schema: "public", DatasourceID: ds.ID,
	}, ws.ID); err != nil {
		t.Fatalf("写入数据集失败: %v", err)
	}
	execSQL(t, db, `UPDATE dataset SET columns='{"oops":1}'::jsonb WHERE table_name='orders'`)

	_, err := r.DatasetColumnsByName(context.Background(), ws.ID, "orders")
	wantErr(t, err, "unmarshal")
}

// 血缘边的节点名为 NULL 时 ListLineage 必须报错（历史数据缺 node_id 是常见脏数据）。
func TestListLineageFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.AddLineage(context.Background(), ws.ID, "dwd_order", []string{"ods_order"}); err != nil {
		t.Fatalf("写入血缘失败: %v", err)
	}
	nullifyKeyColumn(t, db, "lineage_edge", "node_id")

	_, err := r.ListLineage(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}

// 环境创建时间列漂移时环境列表必须报错。
func TestListEnvironmentsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.EnsureEnvironment(context.Background(), ws.ID, "prod", "生产"); err != nil {
		t.Fatalf("写入环境失败: %v", err)
	}
	textColumn(t, db, "environment", "created_at")

	_, err := r.ListEnvironments(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 数据源名称为 NULL 时环境绑定列表必须报错——
// 否则「别名→数据源」映射会缺项，任务会连到默认库上。
func TestListEnvBindingsFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	ds := seedDatasource(t, r, ws.ID, "ds")
	if err := r.EnsureEnvironment(context.Background(), ws.ID, "prod", ""); err != nil {
		t.Fatalf("写入环境失败: %v", err)
	}
	if err := r.BindEnvDatasource(context.Background(), ws.ID, "prod", "src", ds.ID); err != nil {
		t.Fatalf("绑定数据源失败: %v", err)
	}
	nullify(t, db, "datasource", "name")

	_, err := r.ListEnvBindings(context.Background(), ws.ID, "prod")
	wantErr(t, err, "NULL")
}

// 结构变更的 breaking 标记被写成文本（结构漂移）时必须报错。
func TestListSchemaChangesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.SaveSchemaChange(context.Background(), ws.ID, "ds",
		model.SchemaChange{Table: "orders", Kind: "add_column", Column: "c"}); err != nil {
		t.Fatalf("写入结构变更失败: %v", err)
	}
	textColumn(t, db, "schema_change", "breaking")

	_, err := r.ListSchemaChanges(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 人工覆盖的敏感级别列表：表名为 NULL 时必须报错，
// 否则治理页会显示一条没有表名的覆盖记录。
func TestListPIIOverridesFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	ds := seedDatasource(t, r, ws.ID, "ds")
	if err := r.UpsertDataset(context.Background(), model.Dataset{
		Name: "orders", Schema: "public", DatasourceID: ds.ID,
	}, ws.ID); err != nil {
		t.Fatalf("写入数据集失败: %v", err)
	}
	execSQL(t, db, `UPDATE dataset SET pii_override=true, pii='high', pii_reason='人工判定'`)
	nullify(t, db, "dataset", "table_name")

	_, err := r.ListPIIOverrides(context.Background(), ws.ID, "")
	wantErr(t, err, "NULL")
}
