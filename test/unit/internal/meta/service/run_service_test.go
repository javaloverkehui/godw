package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 触发要守住两条幂等语义：schedule/backfill 同日期唯一，manual 允许重复。
// 否则要么补数跑重，要么手工重跑被静默吞掉。
func TestTriggerIdempotency(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.Repo.SavePipeline(ctxBG(), ws.ID, simplePipeline("trig_unpub"), "tester", false); err != nil {
		t.Fatalf("保存未发布管道失败: %v", err)
	}
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "trig_unpub", "2026-10-01", "schedule", nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("未发布管道应拒绝触发，实际 %v", err)
	}
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "ghost", "2026-10-01", "schedule", nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的管道应 NotFound，实际 %v", err)
	}

	publishPipeline(t, svc, ws.ID, simplePipeline("trig_pipe"))
	a, createdA, err := svc.Trigger(ctxBG(), ws.ID, "trig_pipe", "2026-10-01", "schedule", nil)
	if err != nil || !createdA {
		t.Fatalf("首次触发应创建: %+v %v", a, err)
	}
	b, createdB, err := svc.Trigger(ctxBG(), ws.ID, "trig_pipe", "2026-10-01", "schedule", nil)
	if err != nil || createdB || b.ID != a.ID {
		t.Fatalf("同日期 schedule 应复用实例: created=%v a=%s b=%s err=%v", createdB, a.ID, b.ID, err)
	}

	m1, createdM1, err := svc.Trigger(ctxBG(), ws.ID, "trig_pipe", "2026-10-01", "manual", nil)
	if err != nil || !createdM1 {
		t.Fatalf("手工触发应创建: %v %v", createdM1, err)
	}
	m2, createdM2, _ := svc.Trigger(ctxBG(), ws.ID, "trig_pipe", "2026-10-01", "manual", nil)
	if !createdM2 || m1.ID == m2.ID {
		t.Fatalf("手工触发应允许重复并产生新实例: %s %s", m1.ID, m2.ID)
	}

	// 业务日期为空时按今天补齐，不能写空串进库（会造成幂等键撞车）。
	today, _, err := svc.Trigger(ctxBG(), ws.ID, "trig_pipe", "", "schedule", nil)
	if err != nil {
		t.Fatalf("缺省日期触发失败: %v", err)
	}
	if today.BizDate != time.Now().Format("2006-01-02") {
		t.Fatalf("应补今天: %s", today.BizDate)
	}

	// 按 ID 触发用于「重跑」，管道不存在要明确报错。
	if _, _, err := svc.TriggerByPipelineID(ctxBG(), ws.ID, a.PipelineID, "2026-10-02", "retry", nil); err != nil {
		t.Fatalf("按 ID 触发失败: %v", err)
	}
	if _, _, err := svc.TriggerByPipelineID(ctxBG(), ws.ID, "pl_missing", "2026-10-02", "retry", nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("管道不存在应 NotFound，实际 %v", err)
	}
}

// 重跑必须把下游一起打回 pending：只重跑当前节点会让下游读到上一次的产物，
// 这种「看起来刷新了」比直接失败更难排查。
func TestRetryNodeResetsDownstream(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "retry_pipe", Nodes: []model.NodeSpec{
		{ID: "a", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 1"},
		{ID: "b", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 2", DependsOn: []string{"a"}},
		{ID: "c", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 3", DependsOn: []string{"b"}},
	}}
	publishPipeline(t, svc, ws.ID, pipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "retry_pipe", "2026-10-03", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := svc.Repo.FinishTask(ctxBG(), inst.ID, id, "success", 1, 1, ""); err != nil {
			t.Fatalf("置任务成功失败: %v", err)
		}
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "failed", "人工标记"); err != nil {
		t.Fatalf("置实例失败失败: %v", err)
	}

	if _, err := svc.RetryNode(ctxBG(), inst.ID, "nope"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("节点不存在应 NotFound，实际 %v", err)
	}
	got, err := svc.RetryNode(ctxBG(), inst.ID, "b")
	if err != nil {
		t.Fatalf("RetryNode 失败: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("重跑后实例应回到 queued: %s", got.Status)
	}
	tasks, err := svc.Repo.Tasks(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	status := map[string]string{}
	for _, tk := range tasks {
		status[tk.NodeID] = tk.Status
	}
	if status["a"] != "success" {
		t.Fatalf("上游成功节点应保留: %+v", status)
	}
	if status["b"] != "pending" || status["c"] != "pending" {
		t.Fatalf("当前节点与下游应重置为 pending: %+v", status)
	}

	// 运行中的实例不允许重跑（会与执行器双写）。
	if _, err := svc.Repo.ClaimInstance(ctxBG(), "w1", 0); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if _, err := svc.RetryNode(ctxBG(), inst.ID, "a"); !errs.Is(err, errs.CodeInstanceConflict) {
		t.Fatalf("运行中应拒绝重跑，实际 %v", err)
	}
}

// RunOnce 覆盖「领取 → 执行 → 落状态 → 告警/日志收尾」整条链路。
func TestRunOnceSuccessAndFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint)`, p.table("dwd_run_t"))
	p.exec(t, `INSERT INTO %s VALUES (1),(2)`, p.table("dwd_run_t"))

	okPipe := model.PipelineSpec{Kind: "Pipeline", Name: "run_ok", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSource, Datasource: ds.Name,
			SQL: "SELECT id FROM " + p.table("dwd_run_t") + " WHERE id > 0"},
	}}
	publishPipeline(t, svc, ws.ID, okPipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "run_ok", "2026-10-04", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}

	worked, err := svc.RunOnce(ctxBG(), "worker-1")
	if err != nil || !worked {
		t.Fatalf("应领取并执行一个实例: %v %v", worked, err)
	}
	done, err := svc.Repo.GetInstance(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读实例失败: %v", err)
	}
	if done.Status != "success" {
		t.Fatalf("实例应成功: %+v", done)
	}
	tasks, _ := svc.Repo.Tasks(ctxBG(), inst.ID)
	if len(tasks) != 1 || tasks[0].Status != "success" {
		t.Fatalf("任务应成功: %+v", tasks)
	}
	// 没有排队实例时返回 false 而不是报错。
	if worked, err := svc.RunOnce(ctxBG(), "worker-1"); err != nil || worked {
		t.Fatalf("空队列应返回 false: %v %v", worked, err)
	}

	// 失败节点：错误要落库，RunOnce 返回错误但实例仍被正确收尾。
	badPipe := model.PipelineSpec{Kind: "Pipeline", Name: "run_bad", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSource, Datasource: ds.Name, SQL: "SELECT * FROM no_such_table_xyz"},
	}}
	publishPipeline(t, svc, ws.ID, badPipe)
	badInst, _, err := svc.Trigger(ctxBG(), ws.ID, "run_bad", "2026-10-05", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "worker-1"); err == nil {
		t.Fatal("坏节点应返回错误")
	}
	badDone, _ := svc.Repo.GetInstance(ctxBG(), badInst.ID)
	if badDone.Status != "failed" || badDone.Error == "" {
		t.Fatalf("失败实例应记录原因: %+v", badDone)
	}

	// Recover 回收僵尸实例：把租约过期的 running 重新排队。
	if _, err := svc.Repo.ClaimInstance(ctxBG(), "dead-worker", 1*time.Nanosecond); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	n, err := svc.Recover(ctxBG())
	if err != nil {
		t.Fatalf("Recover 失败: %v", err)
	}
	if n < 0 {
		t.Fatalf("回收数量不应为负: %d", n)
	}
}

// 节点 SQL 回放必须用实例创建时的版本快照与实例参数，而不是当前定义。
func TestInstanceNodeSQL(t *testing.T) {
	svc, ws := newSvc(t)
	pipe := model.PipelineSpec{
		Kind: "Pipeline", Name: "sql_view",
		Params: []model.ParamSpec{{Name: "lookback", Type: "int", Default: 7}},
		Nodes: []model.NodeSpec{
			{ID: "q", Type: model.NodeQuality, Model: "m", Config: map[string]any{
				"rules": []any{map[string]any{"name": "r", "kind": "not_null", "column": "id"}},
			}},
			{ID: "s", Type: model.NodeSource, Datasource: "pg", DependsOn: []string{"q"},
				SQL: "SELECT * FROM t WHERE dt = '${biz_date}' AND n = ${lookback}"},
		},
	}
	publishPipeline(t, svc, ws.ID, pipe)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "sql_view", "2026-10-06", "manual",
		map[string]any{"lookback": 3})
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}

	out, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, inst.ID, "s")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.NodeID != "s" || !out.FromSnapshot || out.Version != inst.PipelineVer {
		t.Fatalf("应使用版本快照: %+v", out)
	}
	if !strings.Contains(out.SQL, "n = 3") || !strings.Contains(out.SQL, "2026-10-06") {
		t.Fatalf("SQL 应渲染实例参数与 biz_date: %s", out.SQL)
	}
	if !hasStr(out.Refs, "lookback") || !hasStr(out.Refs, "biz_date") {
		t.Fatalf("引用列表不对: %v", out.Refs)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("节点清单应包含全部节点: %+v", out.Nodes)
	}
	// 不指定节点时跳过没有 SQL 的质量节点，选中真正有 SQL 的那个。
	empty, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, inst.ID, "")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if empty.NodeID != "s" {
		t.Fatalf("应自动选中 SQL 节点: %+v", empty)
	}
	// 只指定质量节点：它没有 SQL，应给出可读提示而不是空结果。
	qOnly, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, inst.ID, "q")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if !strings.Contains(qOnly.Error, "没有 SQL") {
		t.Fatalf("质量节点应提示没有 SQL: %+v", qOnly)
	}
	if _, err := svc.InstanceNodeSQL(ctxBG(), "other-ws", inst.ID, "s"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("跨工作区应 NotFound，实际 %v", err)
	}

}
