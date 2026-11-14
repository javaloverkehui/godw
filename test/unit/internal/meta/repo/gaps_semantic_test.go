package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/semantic"
)

// 本文件覆盖语义层（数据集/图表）版本写入与补数批次、文档站摘要的失败分支。

// 新建数据集失败时必须报错，不能返回一个没有入库的对象：
// 调用方会拿着它的 ID 继续建图表，最终指向一个不存在的数据集。
func TestSaveDatasetFailsWhenInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "semantic_dataset", "INSERT", "")

	_, err := r.SaveDataset(context.Background(), ws.ID, semantic.Dataset{Name: "d"})
	wantErr(t, err, "注入故障: semantic_dataset insert")
}

// 覆盖已有数据集时更新失败必须报错：版本号已经 +1，
// 但 spec 没写进去的话，前端会看到一个「新版本」里装着旧定义。
func TestSaveDatasetFailsWhenUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveDataset(context.Background(), ws.ID, semantic.Dataset{Name: "d", Description: "v1"}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	failOn(t, db, "semantic_dataset", "UPDATE", "")

	_, err := r.SaveDataset(context.Background(), ws.ID, semantic.Dataset{Name: "d", Description: "v2"})
	wantErr(t, err, "注入故障: semantic_dataset update")

	if v := queryInt(t, db, `SELECT version FROM semantic_dataset WHERE name='d'`); v != 1 {
		t.Fatalf("更新失败后版本号不应前进，实际 %d", v)
	}
}

// 数据集版本号列漂移时 ListDatasetsScoped 必须报错（文档站按版本号去重）。
func TestListDatasetsScopedFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveDataset(context.Background(), ws.ID, semantic.Dataset{Name: "d"}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	textColumn(t, db, "semantic_dataset", "version")

	_, err := r.ListDatasetsScoped(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 新建图表失败时必须报错（图表挂在数据集上，半成品会让看板渲染出空图）。
func TestSaveChartFailsWhenInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	failOn(t, db, "chart", "INSERT", "")

	_, err := r.SaveChart(context.Background(), ws.ID, semantic.Chart{Name: "c"})
	wantErr(t, err, "注入故障: chart insert")
}

// 覆盖已有图表时更新失败必须报错，理由同数据集。
func TestSaveChartFailsWhenUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveChart(context.Background(), ws.ID, semantic.Chart{Name: "c", Title: "v1"}); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	failOn(t, db, "chart", "UPDATE", "")

	_, err := r.SaveChart(context.Background(), ws.ID, semantic.Chart{Name: "c", Title: "v2"})
	wantErr(t, err, "注入故障: chart update")

	if v := queryInt(t, db, `SELECT version FROM chart WHERE name='c'`); v != 1 {
		t.Fatalf("更新失败后版本号不应前进，实际 %d", v)
	}
}

// 图表版本号列漂移时 ListCharts 必须报错。
func TestListChartsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveChart(context.Background(), ws.ID, semantic.Chart{Name: "c"}); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	textColumn(t, db, "chart", "version")

	_, err := r.ListCharts(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}

// 补数批次的总数被写成文本（结构漂移）时批次列表必须报错：
// 「总数」是审计补数范围的关键字段，读错等于审计失真。
func TestListBackfillBatchesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.SaveBackfillBatch(context.Background(), ws.ID, &model.BackfillBatch{
		ID: "b1", Pipeline: "p", From: "2024-01-01", To: "2024-01-03", Total: 3,
	}); err != nil {
		t.Fatalf("保存补数批次失败: %v", err)
	}
	textColumn(t, db, "backfill_batch", "total")

	_, err := r.ListBackfillBatches(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 补数进度统计：实例状态为 NULL（历史脏数据）时必须报错，
// 否则统计会漏掉这些实例，让人以为补数已完成。
func TestBackfillStatusFailsOnNullStatus(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, instanceSample(ws.ID, pl.ID))
	nullify(t, db, "instance", "status")

	_, _, err := r.BackfillStatus(context.Background(), ws.ID, "p", "2024-01-01", "2024-01-31")
	wantErr(t, err, "NULL")
}

// 甘特图同理：状态为 NULL 时必须报错，而不是画出一条没有状态的条。
func TestBackfillGanttFailsOnNullStatus(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, instanceSample(ws.ID, pl.ID))
	nullify(t, db, "instance", "status")

	_, err := r.BackfillGantt(context.Background(), ws.ID, 10)
	wantErr(t, err, "NULL")
}

// 文档站「最近一次质量结果」的 passed 列漂移时必须报错：
// 判定结果读错会把失败显示成通过。
func TestLatestQualityResultsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, instanceSample(ws.ID, pl.ID))
	if err := r.SaveQualityResults(context.Background(), inst.ID, "n1", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{RuleName: "not_null", Severity: "error", Passed: true}}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	textColumn(t, db, "quality_result", "passed")

	_, err := r.LatestQualityResults(context.Background(), ws.ID)
	wantErr(t, err, "scan")
}
