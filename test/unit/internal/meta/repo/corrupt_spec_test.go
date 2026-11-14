package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 快照列是 JSONB，历史数据或人工改库可能留下「不是对象」的值。
// 这里钉住两条口径：需要精确口径的地方（执行器/详情页）必须报错；
// 列表页则跳过坏行继续返回其他记录，不能因为一条脏数据整页 500。
func TestCorruptSnapshotSpecs(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	// ---- 模型：生效版本的 spec 不是对象 ----
	execSQL(t, db, `INSERT INTO model(id,workspace_id,name,layer,active_version)
		VALUES('mdl_corrupt',$1,'corrupt_model','ods',1)`, ws.ID)
	execSQL(t, db, `INSERT INTO model_version(id,model_id,version,spec,status)
		VALUES('mv_corrupt','mdl_corrupt',1,'"oops"'::jsonb,'published')`)
	// 再放一个正常模型，验证列表不会因为脏行整体失败。
	if _, err := r.SaveModel(ctx, ws.ID, model.ModelSpec{Kind: "model", Name: "good_model", Layer: "dwd"}, "", "DDL", "t", true); err != nil {
		t.Fatalf("准备正常模型失败: %v", err)
	}

	if _, err := r.PublishedModelSpec(ctx, ws.ID, "corrupt_model"); err == nil {
		t.Fatal("生效版本快照损坏时应报错")
	}
	models, err := r.ListModels(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列表不应因单条脏数据失败: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("应返回 2 个模型，实际 %d", len(models))
	}
	for _, m := range models {
		if m.Name == "corrupt_model" && m.Spec != nil {
			t.Fatalf("脏快照不应解出 spec: %+v", m.Spec)
		}
	}
	// ModelVersions 同样会跳过坏 spec，但仍返回版本行本身。
	versions, err := r.ModelVersions(ctx, ws.ID, "corrupt_model")
	if err != nil {
		t.Fatalf("列版本不应报错: %v", err)
	}
	if len(versions) != 1 || versions[0].Version != 1 || versions[0].Status != "published" {
		t.Fatalf("版本行应照常返回: %+v", versions)
	}

	// ---- 管道：生效版本 spec 损坏 ----
	execSQL(t, db, `INSERT INTO pipeline(id,workspace_id,name,active_version)
		VALUES('pl_corrupt',$1,'bad_pipe',1)`, ws.ID)
	execSQL(t, db, `INSERT INTO pipeline_version(id,pipeline_id,version,spec,status)
		VALUES('pv_corrupt','pl_corrupt',1,'"oops"'::jsonb,'published')`)
	if _, _, _, err := r.PipelineByName(ctx, ws.ID, "bad_pipe"); err == nil {
		t.Fatal("管道快照损坏时应报错")
	}
	if _, err := r.PipelineVersionSpec(ctx, "pl_corrupt", 1); err == nil {
		t.Fatal("按版本取快照损坏时应报错")
	}
	pipelines, err := r.ListPipelines(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列管道不应因脏数据失败: %v", err)
	}
	found := false
	for _, p := range pipelines {
		if p.Name == "bad_pipe" {
			found = true
			if p.Spec != nil {
				t.Fatalf("脏快照不应解出 spec: %+v", p.Spec)
			}
		}
	}
	if !found {
		t.Fatal("坏数据的管道仍应出现在列表里（便于人工修复）")
	}

	// ---- 语义数据集 / 图表 ----
	execSQL(t, db, `INSERT INTO semantic_dataset(id,workspace_id,name,spec)
		VALUES('dsx_corrupt',$1,'bad_dataset','"oops"'::jsonb)`, ws.ID)
	if _, err := r.GetDataset(ctx, ws.ID, "bad_dataset"); err == nil {
		t.Fatal("数据集 spec 损坏时应报错")
	}
	datasets, err := r.ListDatasetsScoped(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列语义数据集不应报错: %v", err)
	}
	if len(datasets) != 0 {
		t.Fatalf("脏行应被跳过: %+v", datasets)
	}

	execSQL(t, db, `INSERT INTO chart(id,workspace_id,name,spec)
		VALUES('cht_corrupt',$1,'bad_chart','"oops"'::jsonb)`, ws.ID)
	if _, err := r.GetChart(ctx, ws.ID, "bad_chart"); err == nil {
		t.Fatal("图表 spec 损坏时应报错")
	}
	charts, err := r.ListCharts(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列图表不应报错: %v", err)
	}
	if len(charts) != 0 {
		t.Fatalf("脏行应被跳过: %+v", charts)
	}
}

// 质量结果与表级血缘的写入同样受外键约束；失败必须显式返回，不能静默丢行。
func TestQualityAndLineageWriteErrors(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	// 实例不存在：质量结果无法落库。
	err := r.SaveQualityResults(ctx, "ins_missing", "n1", []qualityRow{
		{RuleName: "r", Passed: true},
	})
	if err == nil {
		t.Fatal("实例不存在时写质量结果应失败")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM quality_result`); n != 0 {
		t.Fatalf("失败后不应残留质量结果: %d", n)
	}
	// 空结果集不碰库，因此实例不存在也不报错。
	if err := r.SaveQualityResults(ctx, "ins_missing", "n1", nil); err != nil {
		t.Fatalf("空结果集应成功: %v", err)
	}

	// 工作区不存在：表级血缘插入失败。
	if err := r.ReplaceLineage(ctx, "ws_missing", "dwd", []string{"ods"}); err == nil {
		t.Fatal("工作区不存在时写血缘应失败")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge`); n != 0 {
		t.Fatalf("失败后不应残留边: %d", n)
	}
	// 下游为空串是合法的（上游表没有名字），此时只插空 upstream 会被跳过。
	if err := r.ReplaceLineage(ctx, ws.ID, "", []string{""}); err != nil {
		t.Fatalf("空上游应被跳过: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM lineage_edge`); n != 0 {
		t.Fatalf("空上游不应落库: %d", n)
	}
}
