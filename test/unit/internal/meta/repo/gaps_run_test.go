package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
)

// 本文件覆盖执行面（实例/任务/质量结果/血缘/审计/队列）的失败与边界分支。
// 重点是「第二条语句失败」「重复调用幂等」「limit 钳制」这三类——
// 它们在真实库上才会暴露，且一旦出错往往是数据不一致而不是报错。

// 实例读到了、任务列表读失败时，GetInstance 必须整体失败：
// 详情页缺任务列表会让人误判「节点一个都没跑」。
func TestGetInstanceFailsWhenTaskQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	renameColumn(t, db, "task_instance", "node_id", "node_id_drift")

	_, err := r.GetInstance(context.Background(), inst.ID)
	wantErr(t, err, "node_id")
}

// attempt 列漂移时任务列表必须报错（前端靠它显示重试次数）。
func TestTasksFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.AddTasks(context.Background(), inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	textColumn(t, db, "task_instance", "attempt")

	_, err := r.Tasks(context.Background(), inst.ID)
	wantErr(t, err, "scan")
}

// 分页第一页取到总数、第二页失败时必须报错：
// 返回「有总数但没有数据」会让前端显示空白列表。
func TestListInstancesPageFailsWhenRowQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	renameColumn(t, db, "pipeline", "name", "name_drift")

	_, _, err := r.ListInstancesPage(context.Background(), ws.ID, model.InstanceQuery{})
	wantErr(t, err, "name")
}

// pipeline_ver 列漂移时实例分页必须报错。
func TestListInstancesPageFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	textColumn(t, db, "instance", "pipeline_ver")

	_, _, err := r.ListInstancesPage(context.Background(), ws.ID, model.InstanceQuery{})
	wantErr(t, err, "scan")
}

// 实例列表的创建时间列漂移时必须报错。
func TestListInstancesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	textColumn(t, db, "instance", "created_at")

	_, err := r.ListInstances(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 质量结果表结构漂移时查询必须报错：门禁页显示「没有结果」会让人误以为全部通过。
func TestQualityResultsFailsWhenQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	renameColumn(t, db, "quality_result", "rule_name", "rule_name_drift")

	_, err := r.QualityResults(context.Background(), "ins")
	wantErr(t, err, "rule_name")
}

// 质量结果的创建时间列漂移时必须报错。
func TestQualityResultsFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.SaveQualityResults(context.Background(), inst.ID, "n1", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{RuleName: "not_null", Observed: "0", Expected: "0", Severity: "error", Passed: true}}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	textColumn(t, db, "quality_result", "created_at")

	_, err := r.QualityResults(context.Background(), inst.ID)
	wantErr(t, err, "scan")
}

// 血缘图的两个端点在主键里，线上出现 NULL 说明约束被摘掉了；
// 这种脏数据必须让画图接口报错，而不是画出一条没有源头的边。
func TestLineageGraphFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.AddLineage(context.Background(), ws.ID, "dwd_order", []string{"ods_order"}); err != nil {
		t.Fatalf("写入血缘失败: %v", err)
	}
	nullifyKeyColumn(t, db, "lineage_edge", "upstream")

	_, err := r.LineageGraph(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}

// 审计日志时间列漂移时必须报错：审计记录错序等于审计失效。
func TestListAuditFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if err := r.Audit(context.Background(), model.AuditLog{WorkspaceID: ws.ID, Action: "create"}); err != nil {
		t.Fatalf("写审计失败: %v", err)
	}
	textColumn(t, db, "audit_log", "created_at")

	_, err := r.ListAudit(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 质量看板的「最近一次」时间列漂移时必须报错。
func TestQualityOverviewFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.SaveQualityResults(context.Background(), inst.ID, "n1", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{RuleName: "not_null", Severity: "error", Passed: false}}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	textColumn(t, db, "quality_result", "created_at")

	_, err := r.QualityOverview(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 队列视图的优先级列漂移时必须报错：顺序错了会执行错误的实例。
func TestPendingInstancesFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	textColumn(t, db, "instance", "priority")

	_, err := r.PendingInstances(context.Background(), ws.ID, 10)
	wantErr(t, err, "scan")
}

// 取消排队实例时第一步（改状态）失败必须停下，不能继续去改任务表：
// 否则任务显示 skipped 而实例还排在队列里，worker 会把它当正常任务跑。
func TestCancelQueuedInstanceFailsWhenStatusUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	failOn(t, db, "instance", "UPDATE", "NEW.status = 'canceled'")

	status, err := r.CancelQueuedInstance(context.Background(), inst.ID)
	wantErr(t, err, "注入故障: instance update")
	if status != "queued" {
		t.Fatalf("应把取消前的状态回给调用方，实际 %q", status)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM task_instance WHERE status='skipped'`); n != 0 {
		t.Fatalf("实例改状态失败时不应跳过任务，实际 %d 条", n)
	}
}

// 取消排队实例时第二步（跳过任务）失败也必须报错：
// 实例已 canceled 但任务还 pending，重跑时会出现「实例结束、节点没跑」。
func TestCancelQueuedInstanceFailsWhenTaskUpdateRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.AddTasks(context.Background(), inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	failOn(t, db, "task_instance", "UPDATE", "NEW.status = 'skipped'")

	_, err := r.CancelQueuedInstance(context.Background(), inst.ID)
	wantErr(t, err, "注入故障: task_instance update")
}

// InstanceMatrix 的 limit 是外部传入的：0 / 负数 / 超大值都必须钳到默认值，
// 否则一次手滑就能把整张实例表拉出来。
func TestInstanceMatrixClampsLimit(t *testing.T) {
	// 只读接口，无需独占 schema。
	r, _ := newRepoDB(t)
	ws := seedWorkspace(t, r, "w")

	for _, limit := range []int{0, -7, 1000} {
		m, err := r.InstanceMatrix(context.Background(), ws.ID, "", limit)
		if err != nil {
			t.Fatalf("limit=%d 时查询失败: %v", limit, err)
		}
		if m == nil || m.Runs == nil || m.Nodes == nil || m.Cells == nil {
			t.Fatalf("limit=%d 时矩阵结构必须初始化，实际 %+v", limit, m)
		}
	}
}

// 实例开始时间为文本（结构漂移）时矩阵必须报错。
func TestInstanceMatrixFailsOnBrokenColumnType(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	// 领取后才会写 started_at；矩阵扫的就是这一列。
	if _, err := r.ClaimInstance(context.Background(), "worker", time.Minute); err != nil {
		t.Fatalf("领取实例失败: %v", err)
	}
	textColumn(t, db, "instance", "started_at")

	_, err := r.InstanceMatrix(context.Background(), ws.ID, "", 10)
	wantErr(t, err, "scan")
}

// 矩阵第二段查询（任务）失败时必须报错，不能返回只有实例行的半份矩阵。
func TestInstanceMatrixFailsWhenTaskQueryBroken(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.AddTasks(context.Background(), inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	renameColumn(t, db, "task_instance", "node_id", "node_id_drift")

	_, err := r.InstanceMatrix(context.Background(), ws.ID, "", 10)
	wantErr(t, err, "node_id")
}

// 矩阵单元格的耗时列漂移时必须报错。
func TestInstanceMatrixFailsOnBrokenCellColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	pl := seedPipeline(t, r, ws.ID, "p")
	inst := seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: pl.ID, BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	})
	if err := r.AddTasks(context.Background(), inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	textColumn(t, db, "task_instance", "duration_ms")

	_, err := r.InstanceMatrix(context.Background(), ws.ID, "", 10)
	wantErr(t, err, "scan")
}
