package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

func instSample(wsID, pipelineID, key, bizDate, status string) model.Instance {
	return model.Instance{
		WorkspaceID:    wsID,
		PipelineID:     pipelineID,
		PipelineVer:    1,
		BizDate:        bizDate,
		TriggerType:    "manual",
		Status:         status,
		IdempotencyKey: key,
		Params:         map[string]any{"bizDate": bizDate},
	}
}

// 幂等键是调度的安全网：同一业务窗口重复触发只能有一个实例。
// 第二次调用要返回首个实例（created=false），且不能覆盖它的参数。
func TestCreateInstanceIdempotent(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")

	first, created, err := r.CreateInstance(ctx, model.Instance{
		WorkspaceID: ws.ID, PipelineID: p.ID, PipelineVer: 1, BizDate: "2024-03-01",
		TriggerType: "schedule", IdempotencyKey: "2024-03-01:p1",
		Params: map[string]any{"retry": float64(0)},
	})
	if err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	if !created {
		t.Fatal("首次创建 created 应为 true")
	}
	if first.Status != "queued" {
		t.Fatalf("默认状态应是 queued: %q", first.Status)
	}
	if first.CreatedAt.IsZero() || first.Params["retry"] != float64(0) {
		t.Fatalf("实例字段不完整: %+v", first)
	}

	second, created, err := r.CreateInstance(ctx, model.Instance{
		WorkspaceID: ws.ID, PipelineID: p.ID, PipelineVer: 1, BizDate: "2024-03-01",
		TriggerType: "manual", IdempotencyKey: "2024-03-01:p1",
		Params: map[string]any{"retry": float64(3)},
	})
	if err != nil {
		t.Fatalf("重复触发不应报错: %v", err)
	}
	if created {
		t.Fatal("幂等命中时 created 应为 false")
	}
	if second.ID != first.ID {
		t.Fatalf("应复用首个实例: %s != %s", second.ID, first.ID)
	}
	if second.TriggerType != "schedule" || second.Params["retry"] != float64(0) {
		t.Fatalf("幂等命中不应覆盖首条记录: %+v", second)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM instance`); n != 1 {
		t.Fatalf("库里只应有 1 条实例: %d", n)
	}

	// 不同工作区可用相同幂等键（唯一约束是 workspace+key）。
	other := seedWorkspace(t, r, "other")
	otherP := seedPipeline(t, r, other.ID, "p1")
	if _, created, err := r.CreateInstance(ctx, instSample(other.ID, otherP.ID, "2024-03-01:p1", "2024-03-01", "")); err != nil || !created {
		t.Fatalf("跨工作区相同幂等键应可创建: %v %v", created, err)
	}

	// 管道外键必须存在。
	if _, _, err := r.CreateInstance(ctx, instSample(ws.ID, "pl_missing", "k2", "2024-03-02", "")); err == nil {
		t.Fatal("管道不存在应触发外键错误")
	}
	// trigger_type / status 都受 CHECK 约束。
	if _, _, err := r.CreateInstance(ctx, instSample(ws.ID, p.ID, "k3", "2024-03-02", "weird")); err == nil {
		t.Fatal("非法状态应被拒绝")
	}
	bad := instSample(ws.ID, p.ID, "k4", "2024-03-02", "")
	bad.TriggerType = "cron"
	if _, _, err := r.CreateInstance(ctx, bad); err == nil {
		t.Fatal("非法触发类型应被拒绝")
	}
}

// ClaimInstance 是「多 worker 不重复取任务」的关键：先来先领、租约超时可被回收。
func TestClaimHeartbeatFinish(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")

	// 没有可领取实例时返回 (nil, nil)，不是错误。
	got, err := r.ClaimInstance(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("空队列不应报错: %v", err)
	}
	if got != nil {
		t.Fatalf("空队列应返回 nil: %+v", got)
	}

	older := seedInstance(t, r, instSample(ws.ID, p.ID, "k-old", "2024-01-01", ""))
	newer := seedInstance(t, r, instSample(ws.ID, p.ID, "k-new", "2024-01-02", ""))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '2 hours' WHERE id=$1`, older.ID)
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '1 hour' WHERE id=$1`, newer.ID)

	claimed, err := r.ClaimInstance(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if claimed == nil || claimed.ID != older.ID {
		t.Fatalf("应按创建时间先领最早的一条: %+v", claimed)
	}
	if claimed.Status != "running" || claimed.WorkerOwner != "w1" {
		t.Fatalf("领取后应转 running 并记录 worker: %+v", claimed)
	}
	if claimed.StartedAt == nil || claimed.LeaseUntil == nil {
		t.Fatalf("领取后应有开始时间与租约: %+v", claimed)
	}
	if !claimed.LeaseUntil.After(time.Now()) {
		t.Fatalf("租约应在未来: %v", claimed.LeaseUntil)
	}

	// 已运行的实例不会被第二个 worker 领走。
	second, err := r.ClaimInstance(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatalf("第二次领取失败: %v", err)
	}
	if second == nil || second.ID != newer.ID {
		t.Fatalf("第二个 worker 应领到另一条: %+v", second)
	}

	// 续期：worker 正确才延长租约。
	before := *claimed.LeaseUntil
	execSQL(t, db, `UPDATE instance SET lease_until = now() + interval '5 seconds' WHERE id=$1`, claimed.ID)
	if err := r.Heartbeat(ctx, claimed.ID, "w1", time.Minute); err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	var after time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT lease_until FROM instance WHERE id=$1`, claimed.ID).Scan(&after); err != nil {
		t.Fatalf("读取租约失败: %v", err)
	}
	if !after.After(before) {
		t.Fatalf("续期应延长租约: %v -> %v", before, after)
	}

	// 换一个 worker 续期必须失败：租约已不属于它，继续写会污染状态。
	if err := r.Heartbeat(ctx, claimed.ID, "w2", time.Minute); err == nil {
		t.Fatal("错误的 worker 不应能续期")
	} else if errs.From(err).Code != errs.CodeLeaseLost {
		t.Fatalf("应返回 CodeLeaseLost，实际 %v", errs.From(err).Code)
	}

	// 结束后租约清空，再续期同样失败。
	if err := r.FinishInstance(ctx, claimed.ID, "success", ""); err != nil {
		t.Fatalf("结束实例失败: %v", err)
	}
	finished, err := r.GetInstance(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("读取实例失败: %v", err)
	}
	if finished.Status != "success" || finished.FinishedAt == nil || finished.LeaseUntil != nil {
		t.Fatalf("结束状态不对: %+v", finished)
	}
	if err := r.Heartbeat(ctx, claimed.ID, "w1", time.Minute); err == nil {
		t.Fatal("已结束的实例不应能续期")
	} else if errs.From(err).Code != errs.CodeLeaseLost {
		t.Fatalf("应返回 CodeLeaseLost，实际 %v", errs.From(err).Code)
	}

	// 租约过期的 running 实例会被重新领取（前一个 worker 已死）。
	execSQL(t, db, `UPDATE instance SET status='running', worker_owner='dead', lease_until=now() - interval '1 hour' WHERE id=$1`, newer.ID)
	reclaimed, err := r.ClaimInstance(ctx, "w3", time.Minute)
	if err != nil {
		t.Fatalf("回收过期租约失败: %v", err)
	}
	if reclaimed == nil || reclaimed.ID != newer.ID || reclaimed.WorkerOwner != "w3" {
		t.Fatalf("过期实例应被新 worker 接管: %+v", reclaimed)
	}

	// 失败实例记录错误信息。
	if err := r.FinishInstance(ctx, newer.ID, "failed", "SQL 语法错误"); err != nil {
		t.Fatalf("结束实例失败: %v", err)
	}
	if msg := queryStr(t, db, `SELECT error FROM instance WHERE id=$1`, newer.ID); msg != "SQL 语法错误" {
		t.Fatalf("错误信息应落库: %q", msg)
	}
}

// 任务与实例的状态是两条独立的线：节点先 pending，跑起来转 running，结束带行数与耗时。
func TestTaskLifecycleAndOrdering(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	inst := seedInstance(t, r, instSample(ws.ID, p.ID, "k1", "2024-01-01", ""))

	nodes := []model.NodeSpec{
		{ID: "extract", Type: "sql"},
		{ID: "transform", Type: "sql"},
		{ID: "load", Type: "sql"},
	}
	if err := r.AddTasks(ctx, inst.ID, nodes); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	// 空列表是合法的 no-op（管道没有节点时不应报错）。
	if err := r.AddTasks(ctx, inst.ID, nil); err != nil {
		t.Fatalf("空任务列表应成功: %v", err)
	}
	// 重复写入同一节点不报错也不重复插入。
	if err := r.AddTasks(ctx, inst.ID, nodes[:1]); err != nil {
		t.Fatalf("重复写入任务应成功: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM task_instance WHERE instance_id=$1`, inst.ID); n != 3 {
		t.Fatalf("应只有 3 条任务，实际 %d", n)
	}

	tasks, err := r.Tasks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("列任务失败: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("应有 3 条任务，实际 %d", len(tasks))
	}
	// 都还没开始时按 node_id 排序。
	if tasks[0].NodeID != "extract" || tasks[1].NodeID != "load" || tasks[2].NodeID != "transform" {
		t.Fatalf("未开始的任务应按 node_id 排序: %+v", tasks)
	}
	if tasks[0].Status != "pending" || tasks[0].NodeType != "sql" || tasks[0].InstanceID != inst.ID {
		t.Fatalf("任务初始状态不对: %+v", tasks[0])
	}

	// 开始 load，再开始 extract：已开始的排在未开始的后面，且按 node_id 排序。
	if err := r.StartTask(ctx, inst.ID, "load", 1); err != nil {
		t.Fatalf("开始 load 失败: %v", err)
	}
	if err := r.StartTask(ctx, inst.ID, "extract", 2); err != nil {
		t.Fatalf("开始 extract 失败: %v", err)
	}
	tasks, err = r.Tasks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("复查任务失败: %v", err)
	}
	// 已开始的按 started_at 升序排在未开始的后面：load 先启动，故在 extract 之前。
	if tasks[0].NodeID != "transform" {
		t.Fatalf("未开始的任务应排最前: %+v", tasks)
	}
	if tasks[1].NodeID != "load" || tasks[1].Attempt != 1 || tasks[1].Status != "running" || tasks[1].StartedAt == nil {
		t.Fatalf("load 状态不对: %+v", tasks[1])
	}
	if tasks[2].NodeID != "extract" || tasks[2].Attempt != 2 {
		t.Fatalf("extract 状态不对: %+v", tasks[2])
	}

	if err := r.FinishTask(ctx, inst.ID, "extract", "success", 1200, 350, ""); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}
	if err := r.FinishTask(ctx, inst.ID, "load", "failed", 0, 10, "目标表不存在"); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}
	tasks, err = r.Tasks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("复查任务失败: %v", err)
	}
	byID := map[string]model.TaskInstance{}
	for _, tk := range tasks {
		byID[tk.NodeID] = tk
	}
	if byID["extract"].RowsAffected != 1200 || byID["extract"].DurationMS != 350 || byID["extract"].FinishedAt == nil {
		t.Fatalf("成功任务的计数不对: %+v", byID["extract"])
	}
	if byID["load"].Status != "failed" || byID["load"].Error != "目标表不存在" {
		t.Fatalf("失败任务的状态不对: %+v", byID["load"])
	}

	// 跳过下游只打 pending 的节点，已结束的不能被改写。
	if err := r.SkipDownstream(ctx, inst.ID, []string{"transform", "extract", "ghost"}, "上游失败"); err != nil {
		t.Fatalf("跳过下游失败: %v", err)
	}
	tasks, err = r.Tasks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("复查任务失败: %v", err)
	}
	for _, tk := range tasks {
		switch tk.NodeID {
		case "transform":
			if tk.Status != "blocked" || tk.Error != "上游失败" || tk.FinishedAt == nil {
				t.Fatalf("pending 节点应被 block: %+v", tk)
			}
		case "extract":
			if tk.Status != "success" {
				t.Fatalf("已成功节点不应被改写: %+v", tk)
			}
		}
	}
}

// ResetTasks 是「从某节点重跑」的基础：节点回 pending、质量结果清空、上游成功状态保留。
func TestResetTasksClearsResults(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	inst := seedInstance(t, r, instSample(ws.ID, p.ID, "k1", "2024-01-01", ""))
	if err := r.AddTasks(ctx, inst.ID, []model.NodeSpec{{ID: "a", Type: "sql"}, {ID: "b", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := r.StartTask(ctx, inst.ID, "a", 1); err != nil {
		t.Fatalf("开始任务失败: %v", err)
	}
	if err := r.FinishTask(ctx, inst.ID, "a", "failed", 5, 100, "boom"); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}
	// a 的质量结果应被重跑清掉，b 的保留。
	if err := r.SaveQualityResults(ctx, inst.ID, "a", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{RuleName: "not_null", Observed: "3", Expected: "0", Severity: "error", Passed: false}}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}
	if err := r.SaveQualityResults(ctx, inst.ID, "b", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{RuleName: "not_null", Observed: "0", Expected: "0", Severity: "error", Passed: true}}); err != nil {
		t.Fatalf("写入质量结果失败: %v", err)
	}

	// 空节点列表什么都不做。
	if err := r.ResetTasks(ctx, inst.ID, nil); err != nil {
		t.Fatalf("空列表应成功: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM quality_result WHERE instance_id=$1`, inst.ID); n != 2 {
		t.Fatalf("空列表不应清结果: %d", n)
	}

	if err := r.ResetTasks(ctx, inst.ID, []string{"a"}); err != nil {
		t.Fatalf("重置任务失败: %v", err)
	}
	var status, errMsg, worker string
	var rows, dur int64
	var started, finished *time.Time
	var lease *time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT status, error, rows_affected, duration_ms, started_at, finished_at, lease_owner, lease_until
		 FROM task_instance WHERE instance_id=$1 AND node_id='a'`, inst.ID).
		Scan(&status, &errMsg, &rows, &dur, &started, &finished, &worker, &lease); err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if status != "pending" || errMsg != "" || rows != 0 || dur != 0 || started != nil || finished != nil ||
		worker != "" || lease != nil {
		t.Fatalf("重置不彻底: %s %q %d %d %v %v %q %v", status, errMsg, rows, dur, started, finished, worker, lease)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM quality_result WHERE instance_id=$1`, inst.ID); n != 1 {
		t.Fatalf("只应清掉 a 的结果，实际剩 %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM quality_result WHERE node_id='b'`); n != 1 {
		t.Fatalf("b 的结果应保留: %d", n)
	}
}

// GetInstance 要一次带出管道名、资源组、优先级与全部任务，详情页不再二次请求。
func TestGetInstanceDetail(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p_detail")
	inst := seedInstance(t, r, instSample(ws.ID, p.ID, "k1", "2024-05-01", ""))
	if err := r.AddTasks(ctx, inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := r.SetInstanceResources(ctx, inst.ID, "etl", 2); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}

	got, err := r.GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("读实例详情失败: %v", err)
	}
	if got.PipelineName != "p_detail" || got.ResourceGroup != "etl" || got.Priority != 2 {
		t.Fatalf("详情字段不对: %+v", got)
	}
	if got.BizDate != "2024-05-01" || got.TriggerType != "manual" {
		t.Fatalf("业务字段不对: %+v", got)
	}
	if got.Params["bizDate"] != "2024-05-01" {
		t.Fatalf("参数应往返: %+v", got.Params)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].NodeID != "n1" {
		t.Fatalf("应带出任务: %+v", got.Tasks)
	}

	if _, err := r.GetInstance(ctx, "ins_missing"); err == nil {
		t.Fatal("不存在的实例应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// 用量字段没有读取方法，直接断言列值。
	if err := r.RecordInstanceUsage(ctx, inst.ID, 1500, 4096); err != nil {
		t.Fatalf("记录用量失败: %v", err)
	}
	if n := queryInt(t, r.DB, `SELECT cpu_ms FROM instance WHERE id=$1`, inst.ID); n != 1500 {
		t.Fatalf("cpu_ms 应写入: %d", n)
	}
	if n := queryInt(t, r.DB, `SELECT scanned_bytes FROM instance WHERE id=$1`, inst.ID); n != 4096 {
		t.Fatalf("scanned_bytes 应写入: %d", n)
	}

	if err := r.SetInstanceStatus(ctx, inst.ID, "waiting"); err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	if s := queryStr(t, r.DB, `SELECT status FROM instance WHERE id=$1`, inst.ID); s != "waiting" {
		t.Fatalf("状态应改为 waiting: %q", s)
	}
}

// 分页列表：总数不受 LIMIT 影响，四个筛选条件都要下推到 SQL。
func TestListInstancesPage(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	p1 := seedPipeline(t, r, ws.ID, "p1")
	p2 := seedPipeline(t, r, ws.ID, "p2")

	mk := func(p *model.Pipeline, key, bizDate, status string) *model.Instance {
		return seedInstance(t, r, instSample(ws.ID, p.ID, key, bizDate, status))
	}
	a := mk(p1, "a", "2024-01-01", "success")
	b := mk(p1, "b", "2024-01-02", "failed")
	c := mk(p2, "c", "2024-01-03", "success")
	// created_at 决定倒序，显式拉开避免同毫秒并列。
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '3 hours' WHERE id=$1`, a.ID)
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '2 hours' WHERE id=$1`, b.ID)
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '1 hour' WHERE id=$1`, c.ID)

	otherP := seedPipeline(t, r, other.ID, "p1")
	seedInstance(t, r, instSample(other.ID, otherP.ID, "x", "2024-01-01", "success"))

	rows, total, err := r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{})
	if err != nil {
		t.Fatalf("分页查询失败: %v", err)
	}
	if total != 3 {
		t.Fatalf("总数应为 3，实际 %d", total)
	}
	if len(rows) != 3 || rows[0].ID != c.ID || rows[2].ID != a.ID {
		t.Fatalf("应按创建时间倒序: %+v", rows)
	}
	if rows[0].PipelineName != "p2" {
		t.Fatalf("应带管道名: %+v", rows[0])
	}

	// limit 只影响返回行数，不影响 total。
	rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Limit: 1})
	if err != nil {
		t.Fatalf("limit 查询失败: %v", err)
	}
	if total != 3 || len(rows) != 1 || rows[0].ID != c.ID {
		t.Fatalf("limit=1 应返回最新一条且 total 不变: %d %+v", total, rows)
	}

	// 超过 500 会被钳制成默认 50，仍然返回全部 3 条。
	if rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Limit: 10000}); err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("超大 limit 应被钳制: %d %d %v", total, len(rows), err)
	}
	// 负数 offset 视作 0。
	if _, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Offset: -5}); err != nil || total != 3 {
		t.Fatalf("负数 offset 应归零: %d %v", total, err)
	}

	// offset 生效。
	if rows, _, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Limit: 1, Offset: 1}); err != nil || len(rows) != 1 || rows[0].ID != b.ID {
		t.Fatalf("offset 未生效: %+v %v", rows, err)
	}

	// 按管道名筛选。
	if rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Pipeline: "p1"}); err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("管道筛选失败: %d %+v %v", total, rows, err)
	}
	// 按状态筛选。
	if rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Status: "success"}); err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("状态筛选失败: %d %+v %v", total, rows, err)
	}
	// 按业务日期区间筛选（闭区间）。
	if rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{From: "2024-01-02", To: "2024-01-03"}); err != nil || total != 2 {
		t.Fatalf("日期区间筛选失败: %d %+v %v", total, rows, err)
	}
	if rows, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{From: "2024-02-01"}); err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("无匹配应返回空集合: %d %+v %v", total, rows, err)
	}

	// 组合筛选：状态 + 管道。
	if _, total, err = r.ListInstancesPage(ctx, ws.ID, model.InstanceQuery{Pipeline: "p1", Status: "failed"}); err != nil || total != 1 {
		t.Fatalf("组合筛选失败: %d %v", total, err)
	}
}

// 列表与「上一轮实例」：前者按创建时间倒序并受 limit 约束，后者取严格早于本次的那一条。
func TestListInstancesAndPrevious(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	otherP := seedPipeline(t, r, ws.ID, "p2")

	first := seedInstance(t, r, instSample(ws.ID, p.ID, "k1", "2024-01-01", "success"))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '3 hours' WHERE id=$1`, first.ID)
	second := seedInstance(t, r, instSample(ws.ID, p.ID, "k2", "2024-01-02", "success"))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '2 hours' WHERE id=$1`, second.ID)
	third := seedInstance(t, r, instSample(ws.ID, p.ID, "k3", "2024-01-03", "failed"))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '1 hour' WHERE id=$1`, third.ID)
	latest := seedInstance(t, r, instSample(ws.ID, otherP.ID, "k4", "2024-01-04", "success"))

	list, err := r.ListInstances(ctx, ws.ID, 2)
	if err != nil {
		t.Fatalf("列实例失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != latest.ID || list[1].ID != third.ID {
		t.Fatalf("应按创建时间倒序并受 limit 影响: %+v", list)
	}
	if list[1].PipelineName != "p1" || list[1].Status != "failed" || list[0].PipelineName != "p2" {
		t.Fatalf("列表字段不对: %+v", list)
	}
	// limit 非法时回落 50，返回全部 4 条。
	if all, err := r.ListInstances(ctx, ws.ID, 0); err != nil || len(all) != 4 {
		t.Fatalf("limit=0 应回落到默认值: %d %v", len(all), err)
	}
	if all, err := r.ListInstances(ctx, ws.ID, 999); err != nil || len(all) != 4 {
		t.Fatalf("超大 limit 应钳制但不丢数据: %d %v", len(all), err)
	}

	// 上一轮：third 之前同管道的最近一条是 second。created_at 被 SQL 改过，需重新读。
	thirdNow, err := r.GetInstance(ctx, third.ID)
	if err != nil {
		t.Fatalf("读取实例失败: %v", err)
	}
	prev, err := r.PreviousInstance(ctx, ws.ID, p.ID, third.ID, thirdNow.CreatedAt)
	if err != nil {
		t.Fatalf("查上一轮失败: %v", err)
	}
	if prev == nil || prev.ID != second.ID || prev.BizDate != "2024-01-02" {
		t.Fatalf("上一轮应是 second: %+v", prev)
	}
	// 最早那条没有上一轮，返回 nil 而不是错误。
	firstNow, err := r.GetInstance(ctx, first.ID)
	if err != nil {
		t.Fatalf("读取实例失败: %v", err)
	}
	prev, err = r.PreviousInstance(ctx, ws.ID, p.ID, first.ID, firstNow.CreatedAt)
	if err != nil {
		t.Fatalf("最早一条查上一轮不应报错: %v", err)
	}
	if prev != nil {
		t.Fatalf("最早一条不应有上一轮: %+v", prev)
	}
}

// 队列：RequeueStale 回收租约超时实例，RunningCount/Promote/PendingInstances 支撑资源组调度。
func TestQueueControl(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")

	stale := seedInstance(t, r, instSample(ws.ID, p.ID, "stale", "2024-01-01", "running"))
	fresh := seedInstance(t, r, instSample(ws.ID, p.ID, "fresh", "2024-01-02", "running"))
	noLease := seedInstance(t, r, instSample(ws.ID, p.ID, "nolease", "2024-01-03", "running"))
	execSQL(t, db, `UPDATE instance SET lease_until = now() - interval '1 hour' WHERE id=$1`, stale.ID)
	execSQL(t, db, `UPDATE instance SET lease_until = now() + interval '1 hour' WHERE id=$1`, fresh.ID)

	n, err := r.RequeueStale(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("回收超时实例失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("只应回收 1 条，实际 %d", n)
	}
	if s := queryStr(t, db, `SELECT status FROM instance WHERE id=$1`, stale.ID); s != "queued" {
		t.Fatalf("超时实例应回到 queued: %q", s)
	}
	if msg := queryStr(t, db, `SELECT error FROM instance WHERE id=$1`, stale.ID); msg != "租约超时，自动重排队" {
		t.Fatalf("应写入默认错误信息: %q", msg)
	}
	// 已有错误信息时保留原文，便于定位。
	execSQL(t, db, `UPDATE instance SET status='running', lease_until=now() - interval '1 hour', error='已知失败' WHERE id=$1`, fresh.ID)
	if n, err = r.RequeueStale(ctx, time.Minute); err != nil || n != 1 {
		t.Fatalf("第二次回收失败: %d %v", n, err)
	}
	if msg := queryStr(t, db, `SELECT error FROM instance WHERE id=$1`, fresh.ID); msg != "已知失败" {
		t.Fatalf("原有错误应保留: %q", msg)
	}
	if s := queryStr(t, db, `SELECT status FROM instance WHERE id=$1`, noLease.ID); s != "running" {
		t.Fatalf("没有租约的实例不应被回收: %q", s)
	}

	// 按资源组统计运行数。
	if err := r.SetInstanceResources(ctx, stale.ID, "g1", 1); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}
	if err := r.SetInstanceResources(ctx, noLease.ID, "g1", 3); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}
	if err := r.SetInstanceResources(ctx, fresh.ID, "g2", 2); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}
	if err := r.SetInstanceStatus(ctx, fresh.ID, "running"); err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	if c, err := r.RunningCount(ctx, ws.ID, "g1"); err != nil || c != 1 {
		t.Fatalf("g1 运行数应为 1: %d %v", c, err)
	}
	if c, err := r.RunningCount(ctx, ws.ID, "g2"); err != nil || c != 1 {
		t.Fatalf("g2 运行数应为 1: %d %v", c, err)
	}

	// waiting → queued 的提升按优先级排序，且受 limit 约束。
	w1 := seedInstance(t, r, instSample(ws.ID, p.ID, "w1", "2024-01-04", ""))
	w2 := seedInstance(t, r, instSample(ws.ID, p.ID, "w2", "2024-01-05", ""))
	if err := r.SetInstanceResources(ctx, w1.ID, "g1", 9); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}
	if err := r.SetInstanceResources(ctx, w2.ID, "g1", 1); err != nil {
		t.Fatalf("设置资源组失败: %v", err)
	}
	if err := r.SetInstanceStatus(ctx, w1.ID, "waiting"); err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	if err := r.SetInstanceStatus(ctx, w2.ID, "waiting"); err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	// limit<=0 是 no-op。
	if n, err := r.Promote(ctx, ws.ID, "g1", 0); err != nil || n != 0 {
		t.Fatalf("limit=0 不应提升: %d %v", n, err)
	}
	if n, err := r.Promote(ctx, ws.ID, "g1", 1); err != nil || n != 1 {
		t.Fatalf("应只提升 1 条: %d %v", n, err)
	}
	// 优先级小的先提升。
	if s := queryStr(t, db, `SELECT status FROM instance WHERE id=$1`, w2.ID); s != "queued" {
		t.Fatalf("优先级 1 的应先提升: %q", s)
	}
	if s := queryStr(t, db, `SELECT status FROM instance WHERE id=$1`, w1.ID); s != "waiting" {
		t.Fatalf("优先级 9 的应仍等待: %q", s)
	}
	if q := queryInt(t, db, `SELECT count(*) FROM instance WHERE id=$1 AND queued_at IS NOT NULL`, w2.ID); q != 1 {
		t.Fatalf("提升后应记录 queued_at")
	}

	// 排队视图包含 queued 与 waiting，按优先级再按入队时间排序。
	// stale 被 RequeueStale 退回队列（优先级 1），w2 刚被提升（优先级 1），w1 仍在等待（优先级 9）。
	pending, err := r.PendingInstances(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列排队失败: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("应有 3 条排队，实际 %d: %+v", len(pending), pending)
	}
	if pending[0].InstanceID != stale.ID || pending[0].Priority != 1 || pending[0].Status != "queued" {
		t.Fatalf("排队顺序不对: %+v", pending)
	}
	if pending[1].InstanceID != w2.ID || pending[1].QueuedAt == "" {
		t.Fatalf("被提升的实例应带 queued_at: %+v", pending[1])
	}
	if pending[1].PipelineName != "p1" || pending[1].ResourceGroup != "g1" {
		t.Fatalf("排队行字段不对: %+v", pending[1])
	}
	if pending[2].InstanceID != w1.ID || pending[2].Status != "waiting" {
		t.Fatalf("优先级 9 的应排在最后: %+v", pending[2])
	}
	if got, err := r.PendingInstances(ctx, ws.ID, -1); err != nil || len(got) != 3 {
		t.Fatalf("非法 limit 应回落默认值: %d %v", len(got), err)
	}
}

// 取消只对「还没被领取」的实例生效；运行中的要等当前节点结束，这是能力边界。
func TestCancelQueuedInstance(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")

	queued := seedInstance(t, r, instSample(ws.ID, p.ID, "q", "2024-01-01", ""))
	if err := r.AddTasks(ctx, queued.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}, {ID: "n2", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	old, err := r.CancelQueuedInstance(ctx, queued.ID)
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if old != "queued" {
		t.Fatalf("应返回取消前的状态: %q", old)
	}
	inst, err := r.GetInstance(ctx, queued.ID)
	if err != nil {
		t.Fatalf("读实例失败: %v", err)
	}
	if inst.Status != "canceled" || inst.FinishedAt == nil || inst.Error != "用户取消" {
		t.Fatalf("取消状态不对: %+v", inst)
	}
	// pending 任务连带标 skipped。
	if n := queryInt(t, db, `SELECT count(*) FROM task_instance WHERE instance_id=$1 AND status='skipped'`, queued.ID); n != 2 {
		t.Fatalf("pending 任务应全部 skipped: %d", n)
	}

	running := seedInstance(t, r, instSample(ws.ID, p.ID, "r", "2024-01-02", "running"))
	if _, err := r.CancelQueuedInstance(ctx, running.ID); err == nil {
		t.Fatal("运行中的实例不应能取消")
	} else if errs.From(err).Code != errs.CodeConflict {
		t.Fatalf("应返回 CodeConflict，实际 %v", errs.From(err).Code)
	}

	done := seedInstance(t, r, instSample(ws.ID, p.ID, "s", "2024-01-03", "success"))
	if _, err := r.CancelQueuedInstance(ctx, done.ID); err == nil {
		t.Fatal("已结束的实例不应能取消")
	} else if errs.From(err).Code != errs.CodeConflict {
		t.Fatalf("应返回 CodeConflict，实际 %v", errs.From(err).Code)
	}

	if _, err := r.CancelQueuedInstance(ctx, "ins_missing"); err == nil {
		t.Fatal("不存在的实例应报错")
	}
}

// 原地重跑：实例回队列并清空执行痕迹，不新建实例。
func TestRequeueInstance(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	inst := seedInstance(t, r, instSample(ws.ID, p.ID, "k", "2024-01-01", "running"))
	execSQL(t, db, `UPDATE instance SET worker_owner='w1', lease_until=now()+interval '1 hour',
		started_at=now(), finished_at=now(), cpu_ms=99 WHERE id=$1`, inst.ID)

	if err := r.RequeueInstance(ctx, inst.ID, "手动重跑"); err != nil {
		t.Fatalf("重排队失败: %v", err)
	}
	var status, worker, errMsg string
	var started, finished, lease, queued *time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT status, worker_owner, error, started_at, finished_at, lease_until, queued_at
		 FROM instance WHERE id=$1`, inst.ID).
		Scan(&status, &worker, &errMsg, &started, &finished, &lease, &queued); err != nil {
		t.Fatalf("读取实例失败: %v", err)
	}
	if status != "queued" || worker != "" || errMsg != "手动重跑" {
		t.Fatalf("重排队状态不对: %s %q %q", status, worker, errMsg)
	}
	if started != nil || finished != nil || lease != nil {
		t.Fatalf("执行痕迹应被清空: %v %v %v", started, finished, lease)
	}
	if queued == nil {
		t.Fatal("应记录 queued_at")
	}
	// 用量是历史事实，不应被清空。
	if n := queryInt(t, db, `SELECT cpu_ms FROM instance WHERE id=$1`, inst.ID); n != 99 {
		t.Fatalf("用量不应被重置: %d", n)
	}
}

// 运行矩阵：列是最近 N 次运行，行是节点；缺节点的格子必须是 nil，
// 前端据此画「本次没跑这个节点」。
func TestInstanceMatrix(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	other := seedPipeline(t, r, ws.ID, "p2")

	older := seedInstance(t, r, instSample(ws.ID, p.ID, "old", "2024-01-01", "success"))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '2 hours' WHERE id=$1`, older.ID)
	newer := seedInstance(t, r, instSample(ws.ID, p.ID, "new", "2024-01-02", "failed"))
	execSQL(t, db, `UPDATE instance SET created_at = now() - interval '1 hour' WHERE id=$1`, newer.ID)
	otherInst := seedInstance(t, r, instSample(ws.ID, other.ID, "x", "2024-01-03", "success"))

	if err := r.AddTasks(ctx, older.ID, []model.NodeSpec{{ID: "a", Type: "sql"}, {ID: "b", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := r.AddTasks(ctx, newer.ID, []model.NodeSpec{{ID: "a", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := r.AddTasks(ctx, otherInst.ID, []model.NodeSpec{{ID: "z", Type: "sql"}}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := r.FinishTask(ctx, older.ID, "a", "success", 10, 100, ""); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}
	if err := r.FinishTask(ctx, newer.ID, "a", "failed", 0, 20, "boom"); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}

	m, err := r.InstanceMatrix(ctx, ws.ID, "p1", 10)
	if err != nil {
		t.Fatalf("取矩阵失败: %v", err)
	}
	if m.Pipeline != "p1" {
		t.Fatalf("应回填管道名: %q", m.Pipeline)
	}
	if len(m.Runs) != 2 || m.Runs[0].ID != newer.ID || m.Runs[1].ID != older.ID {
		t.Fatalf("运行应按创建时间倒序: %+v", m.Runs)
	}
	if m.Runs[0].BizDate != "2024-01-02" || m.Runs[0].Status != "failed" {
		t.Fatalf("运行字段不对: %+v", m.Runs[0])
	}
	if len(m.Nodes) != 2 || m.Nodes[0] != "a" || m.Nodes[1] != "b" {
		t.Fatalf("节点应排序: %+v", m.Nodes)
	}
	// 每次运行都要有一整列，长度与 Runs 相等。
	if len(m.Cells["a"]) != 2 || len(m.Cells["b"]) != 2 {
		t.Fatalf("每个节点都应有与运行等长的列: %+v", m.Cells)
	}
	if m.Cells["a"][0] == nil || m.Cells["a"][0].Status != "failed" || m.Cells["a"][0].Error != "boom" {
		t.Fatalf("最新一次 a 的状态不对: %+v", m.Cells["a"][0])
	}
	if m.Cells["a"][1] == nil || m.Cells["a"][1].DurationMS != 100 {
		t.Fatalf("上一轮 a 的耗时不对: %+v", m.Cells["a"][1])
	}
	// 新一轮没有 b 节点，格子必须是 nil。
	if m.Cells["b"][0] != nil {
		t.Fatalf("缺失节点应为 nil: %+v", m.Cells["b"][0])
	}

	// 不带管道过滤时包含 p2 的运行。
	all, err := r.InstanceMatrix(ctx, ws.ID, "", 10)
	if err != nil {
		t.Fatalf("全量矩阵失败: %v", err)
	}
	if len(all.Runs) != 3 {
		t.Fatalf("不过滤时应包含 3 次运行，实际 %d", len(all.Runs))
	}
	if len(all.Nodes) != 3 {
		t.Fatalf("节点并集应有 3 个，实际 %+v", all.Nodes)
	}

	// 空工作区返回空矩阵而不是 nil。
	empty, err := r.InstanceMatrix(ctx, "ws_none", "", 5)
	if err != nil {
		t.Fatalf("空工作区矩阵失败: %v", err)
	}
	if len(empty.Runs) != 0 || len(empty.Nodes) != 0 || empty.Cells == nil {
		t.Fatalf("空矩阵字段不对: %+v", empty)
	}
}

// 环比告警依赖「上一个有数据的业务日」的写入行数之和。
func TestPreviousInstanceRows(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")

	day1 := seedInstance(t, r, instSample(ws.ID, p.ID, "d1", "2024-01-01", "success"))
	day2 := seedInstance(t, r, instSample(ws.ID, p.ID, "d2", "2024-01-02", "success"))
	// 失败的那天不算：它的行数不可信。
	day3 := seedInstance(t, r, instSample(ws.ID, p.ID, "d3", "2024-01-03", "failed"))
	current := seedInstance(t, r, instSample(ws.ID, p.ID, "d4", "2024-01-04", ""))

	for _, tc := range []struct {
		inst *model.Instance
		rows int64
	}{{day1, 100}, {day2, 250}, {day3, 999}} {
		if err := r.AddTasks(ctx, tc.inst.ID, []model.NodeSpec{{ID: "n1", Type: "sql"}}); err != nil {
			t.Fatalf("写入任务失败: %v", err)
		}
		if err := r.FinishTask(ctx, tc.inst.ID, "n1", "success", tc.rows, 1, ""); err != nil {
			t.Fatalf("结束任务失败: %v", err)
		}
	}

	rows, err := r.PreviousInstanceRows(ctx, ws.ID, p.ID, current.BizDate)
	if err != nil {
		t.Fatalf("统计上期行数失败: %v", err)
	}
	if rows != 250 {
		t.Fatalf("应取最近一个成功业务日（2024-01-02）的 250 行，实际 %d", rows)
	}

	// 没有任何历史时返回 0，不报错。
	if rows, err := r.PreviousInstanceRows(ctx, ws.ID, p.ID, "2024-01-01"); err != nil || rows != 0 {
		t.Fatalf("无历史时应返回 0: %d %v", rows, err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM instance WHERE workspace_id=$1`, ws.ID); n != 4 {
		t.Fatalf("实例应全部保留: %d", n)
	}
}
