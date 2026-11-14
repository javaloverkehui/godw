package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
)

// ExistingBizDates 判「已有」不看状态：失败实例也算已有，补数默认跳过它。
func TestExistingBizDates(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	other := seedPipeline(t, r, ws.ID, "p2")

	for _, c := range []struct {
		pipe   *model.Pipeline
		key    string
		biz    string
		status string
	}{
		{p, "k1", "2024-01-01", "success"},
		{p, "k2", "2024-01-02", "failed"},
		{p, "k3", "2024-01-05", "success"}, // 区间外
		{p, "k4", "2024-01-03", "queued"},
		{other, "k5", "2024-01-01", "success"}, // 另一条管道
	} {
		if _, _, err := r.CreateInstance(ctx, model.Instance{
			WorkspaceID: ws.ID, PipelineID: c.pipe.ID, PipelineVer: 1, BizDate: c.biz,
			TriggerType: "backfill", Status: c.status, IdempotencyKey: c.key,
		}); err != nil {
			t.Fatalf("准备实例 %s 失败: %v", c.key, err)
		}
	}

	dates, err := r.ExistingBizDates(ctx, ws.ID, "p1", "2024-01-01", "2024-01-03")
	if err != nil {
		t.Fatalf("查已有业务日期失败: %v", err)
	}
	if len(dates) != 3 {
		t.Fatalf("应有 3 个已有日期（含失败与排队），实际 %+v", dates)
	}
	for _, d := range []string{"2024-01-01", "2024-01-02", "2024-01-03"} {
		if !dates[d] {
			t.Fatalf("应包含 %s: %+v", d, dates)
		}
	}
	if dates["2024-01-05"] {
		t.Fatalf("区间外日期不应出现: %+v", dates)
	}

	// 单日区间；空结果返回空 map（不是 nil）。
	if one, err := r.ExistingBizDates(ctx, ws.ID, "p1", "2024-01-02", "2024-01-02"); err != nil || !one["2024-01-02"] {
		t.Fatalf("单日区间查询失败: %v %v", one, err)
	}
	empty, err := r.ExistingBizDates(ctx, ws.ID, "p1", "2023-01-01", "2023-12-31")
	if err != nil {
		t.Fatalf("空区间查询失败: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空区间应返回空 map: %#v", empty)
	}
	// 其他管道互不影响。
	if otherDates, err := r.ExistingBizDates(ctx, ws.ID, "p2", "2024-01-01", "2024-01-03"); err != nil || len(otherDates) != 1 {
		t.Fatalf("另一条管道应只有 1 天: %v %v", otherDates, err)
	}
}

// 补数批次是审计记录：提交时写一次，事后按工作区翻查。
func TestBackfillBatchRecord(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	base := time.Now().Truncate(time.Second)
	old := &model.BackfillBatch{
		ID: "bf_old", Pipeline: "p1", From: "2024-01-01", To: "2024-01-03",
		Total: 3, Created: 2, Skipped: 1, Reason: "历史漏跑", CreatedBy: "alice",
		CreatedAt: base.Add(-2 * time.Hour),
	}
	recent := &model.BackfillBatch{
		ID: "bf_new", Pipeline: "p1", From: "2024-01-04", To: "2024-01-05",
		Total: 2, Created: 2, Skipped: 0, Reason: "重算", CreatedBy: "bob",
		CreatedAt: base.Add(-time.Hour),
	}
	for _, b := range []*model.BackfillBatch{old, recent} {
		if err := r.SaveBackfillBatch(ctx, ws.ID, b); err != nil {
			t.Fatalf("保存批次 %s 失败: %v", b.ID, err)
		}
	}
	if err := r.SaveBackfillBatch(ctx, other.ID, old); err == nil {
		// 主键冲突：同一个批次 ID 不能落两个工作区。
		t.Fatal("重复主键应报错")
	}

	got, err := r.GetBackfillBatch(ctx, ws.ID, "bf_old")
	if err != nil {
		t.Fatalf("取批次失败: %v", err)
	}
	if got.Pipeline != "p1" || got.From != "2024-01-01" || got.To != "2024-01-03" {
		t.Fatalf("批次字段不对: %+v", got)
	}
	if got.Total != 3 || got.Created != 2 || got.Skipped != 1 {
		t.Fatalf("计数不对: %+v", got)
	}
	if got.Reason != "历史漏跑" || got.CreatedBy != "alice" || !got.CreatedAt.Equal(base.Add(-2*time.Hour)) {
		t.Fatalf("审计字段不对: %+v", got)
	}
	// 取不到时返回原始错误（服务层自己映射成 404）。
	if _, err := r.GetBackfillBatch(ctx, ws.ID, "bf_missing"); err == nil {
		t.Fatal("不存在的批次应报错")
	}
	// 跨工作区不可见。
	if _, err := r.GetBackfillBatch(ctx, other.ID, "bf_old"); err == nil {
		t.Fatal("跨工作区不应查到批次")
	}

	list, err := r.ListBackfillBatches(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列批次失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != "bf_new" || list[1].ID != "bf_old" {
		t.Fatalf("应按创建时间倒序: %+v", list)
	}
	// limit 生效；非法 limit 回落 20。
	if one, err := r.ListBackfillBatches(ctx, ws.ID, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 未生效: %v %v", one, err)
	}
	if two, err := r.ListBackfillBatches(ctx, ws.ID, 0); err != nil || len(two) != 2 {
		t.Fatalf("非法 limit 应回落默认值: %v %v", two, err)
	}
	if two, err := r.ListBackfillBatches(ctx, ws.ID, 500); err != nil || len(two) != 2 {
		t.Fatalf("超大 limit 应钳制: %v %v", two, err)
	}
	if got, err := r.ListBackfillBatches(ctx, other.ID, 10); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区应为空: %v %v", got, err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM backfill_batch`); n != 2 {
		t.Fatalf("只应有 2 条批次: %d", n)
	}
}

// BackfillStatus 是补数进度条的数据源：成功/失败/运行中三类计数，
// 并把失败原因去重（min(error)）带给前端。
func TestBackfillStatusAndGantt(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	p := seedPipeline(t, r, ws.ID, "p1")
	other := seedPipeline(t, r, ws.ID, "p2")

	mk := func(pipe *model.Pipeline, key, biz, status, errMsg string) *model.Instance {
		t.Helper()
		inst, _, err := r.CreateInstance(ctx, model.Instance{
			WorkspaceID: ws.ID, PipelineID: pipe.ID, PipelineVer: 1, BizDate: biz,
			TriggerType: "backfill", Status: status, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatalf("准备实例 %s 失败: %v", key, err)
		}
		if errMsg != "" {
			execSQL(t, db, `UPDATE instance SET error=$2 WHERE id=$1`, inst.ID, errMsg)
		}
		return inst
	}
	ok := mk(p, "k1", "2024-02-01", "success", "")
	mk(p, "k2", "2024-02-02", "failed", "目标表不存在")
	mk(p, "k3", "2024-02-02", "failed", "目标表不存在") // 同一天再失败一次
	run := mk(p, "k4", "2024-02-03", "running", "")
	mk(p, "k5", "2024-02-03", "queued", "")
	mk(p, "k6", "2024-02-04", "canceled", "") // 区间外/不计入任何桶
	mk(other, "k7", "2024-02-01", "success", "")
	_ = ok

	status, fails, err := r.BackfillStatus(ctx, ws.ID, "p1", "2024-02-01", "2024-02-03")
	if err != nil {
		t.Fatalf("统计补数状态失败: %v", err)
	}
	if status["success"] != 1 || status["failed"] != 2 || status["running"] != 2 {
		t.Fatalf("状态计数不对: %+v", status)
	}
	// running 桶同时包含 running 与 queued；canceled 不落任何桶。
	if fails == nil || len(fails) != 1 || fails[0] != "目标表不存在" {
		t.Fatalf("失败原因应去重后返回: %+v", fails)
	}
	// 区间外无数据时计数全 0，fails 是空切片（不是 nil，前端按数组消费）。
	empty, emptyFails, err := r.BackfillStatus(ctx, ws.ID, "p1", "2023-01-01", "2023-01-31")
	if err != nil {
		t.Fatalf("空区间统计失败: %v", err)
	}
	if empty["success"] != 0 || empty["failed"] != 0 || empty["running"] != 0 {
		t.Fatalf("空区间计数应为 0: %+v", empty)
	}
	if emptyFails == nil || len(emptyFails) != 0 {
		t.Fatalf("空区间应返回空切片: %#v", emptyFails)
	}

	// 甘特图：起止时间与耗时；未开始的用 created_at 兜底，未结束的用 now() 兜底。
	execSQL(t, db, `UPDATE instance SET started_at = now() - interval '10 minutes',
		finished_at = now() - interval '5 minutes' WHERE id=$1`, ok.ID)
	execSQL(t, db, `UPDATE instance SET started_at = now() - interval '2 minutes', finished_at = NULL WHERE id=$1`, run.ID)

	gantt, err := r.BackfillGantt(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("取甘特数据失败: %v", err)
	}
	if len(gantt) != 7 {
		t.Fatalf("应返回本工作区全部 7 次运行，实际 %d", len(gantt))
	}
	// 按创建时间倒序：最后创建的是另一条管道（p2）的实例。
	if gantt[0]["pipeline"] != "p2" {
		t.Fatalf("应按创建时间倒序: %+v", gantt[0])
	}
	byID := map[string]map[string]any{}
	for _, g := range gantt {
		byID[g["id"].(string)] = g
	}
	done := byID[ok.ID]
	if done["pipeline"] != "p1" || done["bizDate"] != "2024-02-01" {
		t.Fatalf("甘特行字段不对: %+v", done)
	}
	if ms := done["durationMs"].(int64); ms < 4*60*1000 || ms > 6*60*1000 {
		t.Fatalf("耗时应约 5 分钟，实际 %d ms", ms)
	}
	runningRow := byID[run.ID]
	if runningRow["durationMs"].(int64) <= 0 {
		t.Fatalf("未结束的实例耗时应为正（now() 兜底）: %+v", runningRow)
	}
	// 未开始的实例用 created_at 兜底，结束时间为 now，耗时应非负。
	for _, g := range gantt {
		if g["durationMs"].(int64) < 0 {
			t.Fatalf("耗时应非负: %+v", g)
		}
	}

	// limit 生效；非法 limit 回落 120。
	if one, err := r.BackfillGantt(ctx, ws.ID, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 未生效: %v %v", one, err)
	}
	if all, err := r.BackfillGantt(ctx, ws.ID, 0); err != nil || len(all) != 7 {
		t.Fatalf("非法 limit 应回落默认值: %d %v", len(all), err)
	}
	if got, err := r.BackfillGantt(ctx, "ws_none", 10); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空集合: %v %v", got, err)
	}
}
