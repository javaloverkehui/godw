package repo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 保留策略：默认 365 天，同名 upsert 只更新。
func TestRetentionPolicy(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	if err := r.SaveRetentionPolicy(ctx, ws.ID, model.RetentionPolicy{
		ModelName: "dwd_order", RetainDays: 0, ArchiveTo: "s3://cold", Enabled: true,
	}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}
	// RetainDays<=0 回落到 365（0 天等于删光，绝不能落库）。
	if n := queryInt(t, db, `SELECT retain_days FROM retention_policy WHERE model_name='dwd_order'`); n != 365 {
		t.Fatalf("默认保留天数应是 365，实际 %d", n)
	}
	if err := r.SaveRetentionPolicy(ctx, ws.ID, model.RetentionPolicy{
		ModelName: "dim_user", RetainDays: 30, Enabled: false,
	}); err != nil {
		t.Fatalf("保存第二条策略失败: %v", err)
	}
	// 同名更新：天数与归档目标一起变。
	if err := r.SaveRetentionPolicy(ctx, ws.ID, model.RetentionPolicy{
		ModelName: "dwd_order", RetainDays: 90, ArchiveTo: "oss://cold", Enabled: false,
	}); err != nil {
		t.Fatalf("更新保留策略失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM retention_policy WHERE workspace_id=$1`, ws.ID); n != 2 {
		t.Fatalf("upsert 不应新增行: %d", n)
	}

	got, err := r.GetRetentionPolicy(ctx, ws.ID, "dwd_order")
	if err != nil {
		t.Fatalf("取保留策略失败: %v", err)
	}
	if got.RetainDays != 90 || got.ArchiveTo != "oss://cold" || got.Enabled {
		t.Fatalf("更新未生效: %+v", got)
	}
	if got.WorkspaceID != ws.ID || got.ModelName != "dwd_order" {
		t.Fatalf("应回填工作区与模型名: %+v", got)
	}
	if _, err := r.GetRetentionPolicy(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("未配置的模型应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	list, err := r.ListRetentionPolicies(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列保留策略失败: %v", err)
	}
	if len(list) != 2 || list[0].ModelName != "dim_user" || list[1].ModelName != "dwd_order" {
		t.Fatalf("应按模型名排序: %+v", list)
	}
	if list[0].RetainDays != 30 || list[0].Enabled {
		t.Fatalf("第二条策略字段不对: %+v", list[0])
	}
	if got, err := r.ListRetentionPolicies(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空集合: %v %v", got, err)
	}
}

// 擦除任务（删除权）必须可追溯：谁申请、删了多少行、何时结束。
func TestErasureJobs(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	base := time.Now()

	// 状态非法会被 CHECK 挡住。
	if err := r.CreateErasureJob(ctx, model.ErasureJob{
		ID: "era_bad", WorkspaceID: ws.ID, ModelName: "dim_user", SubjectKey: "u1", Status: "queued",
	}); err == nil {
		t.Fatal("非法擦除状态应被拒绝")
	}

	if err := r.CreateErasureJob(ctx, model.ErasureJob{
		ID: "era_1", WorkspaceID: ws.ID, ModelName: "dim_user", SubjectKey: "u1",
		Status: "pending", Reason: "用户行使删除权", RequestedBy: "alice",
	}); err != nil {
		t.Fatalf("创建擦除任务失败: %v", err)
	}
	if err := r.CreateErasureJob(ctx, model.ErasureJob{
		ID: "era_2", WorkspaceID: ws.ID, ModelName: "dwd_order", SubjectKey: "u2",
		Status: "pending", Reason: "合规要求", RequestedBy: "bob",
	}); err != nil {
		t.Fatalf("创建第二个任务失败: %v", err)
	}
	execSQL(t, db, `UPDATE erasure_job SET created_at = $1 WHERE id='era_1'`, base.Add(-2*time.Hour))
	// 其他工作区的任务不应出现在列表里。
	if err := r.CreateErasureJob(ctx, model.ErasureJob{
		ID: "era_3", WorkspaceID: other.ID, ModelName: "dim_user", SubjectKey: "u3",
		Status: "pending",
	}); err != nil {
		t.Fatalf("其他工作区创建任务失败: %v", err)
	}

	// 运行中不落结束时间。
	if err := r.SetErasureStatus(ctx, "era_1", "running", 0, ""); err != nil {
		t.Fatalf("改擦除状态失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM erasure_job WHERE id='era_1' AND finished_at IS NOT NULL`); n != 0 {
		t.Fatalf("运行中不应落结束时间")
	}
	// 完成：行数与结束时间都落。
	if err := r.SetErasureStatus(ctx, "era_1", "done", 12, ""); err != nil {
		t.Fatalf("完成任务失败: %v", err)
	}
	// 失败：错误信息与结束时间都落。
	if err := r.SetErasureStatus(ctx, "era_2", "failed", 0, "目标表被锁"); err != nil {
		t.Fatalf("失败任务更新失败: %v", err)
	}

	jobs, err := r.ListErasureJobs(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列擦除任务失败: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("应有 2 条任务，实际 %d: %+v", len(jobs), jobs)
	}
	byID := map[string]model.ErasureJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	done := byID["era_1"]
	if done.Status != "done" || done.RowsDeleted != 12 || done.FinishedAt == nil {
		t.Fatalf("完成的任务字段不对: %+v", done)
	}
	if done.Reason != "用户行使删除权" || done.RequestedBy != "alice" || done.ModelName != "dim_user" {
		t.Fatalf("审计字段不对: %+v", done)
	}
	if done.WorkspaceID != ws.ID {
		t.Fatalf("应回填工作区: %+v", done)
	}
	failed := byID["era_2"]
	if failed.Status != "failed" || failed.Error != "目标表被锁" || failed.FinishedAt == nil {
		t.Fatalf("失败任务字段不对: %+v", failed)
	}

	// limit 生效；非法 limit 回落 50。
	if one, err := r.ListErasureJobs(ctx, ws.ID, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 未生效: %v %v", one, err)
	}
	if two, err := r.ListErasureJobs(ctx, ws.ID, 0); err != nil || len(two) != 2 {
		t.Fatalf("非法 limit 应回落默认值: %v %v", two, err)
	}
	if got, err := r.ListErasureJobs(ctx, other.ID, 10); err != nil || len(got) != 1 {
		t.Fatalf("其他工作区应只有自己的任务: %v %v", got, err)
	}
}

// 表分级取一组表里最高的级别：一张表的 secret 列不该把整库都拉高，
// 但被查询的这组表里只要有 secret，整体就是 high。
func TestTableClassification(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	// 空输入直接 none，不查库。
	if got, err := r.TableClassification(ctx, ws.ID, nil); err != nil || got != "none" {
		t.Fatalf("空表清单应返回 none: %q %v", got, err)
	}

	for _, p := range []model.ColumnPolicy{
		{TableName: "orders", ColumnName: "id", Classification: "public", MaskType: "none"},
		{TableName: "orders", ColumnName: "amount", Classification: "confidential", MaskType: "partial"},
		{TableName: "users", ColumnName: "phone", Classification: "secret", MaskType: "phone"},
		{TableName: "logs", ColumnName: "ip", Classification: "low", MaskType: "none"},
		{TableName: "odd", ColumnName: "x", Classification: "unknown-level", MaskType: "none"},
	} {
		if err := r.SaveColumnPolicy(ctx, ws.ID, p); err != nil {
			t.Fatalf("准备列策略失败: %v", err)
		}
	}

	cases := []struct {
		name   string
		tables []string
		want   string
	}{
		{"只查公开表", []string{"orders"}, "medium"}, // orders 最高是 confidential → medium
		{"含 secret", []string{"orders", "users"}, "high"},
		{"历史 low 写法", []string{"logs"}, "low"},
		{"未知分级按 0 处理", []string{"odd"}, "none"},
		{"混合未知与已知", []string{"odd", "orders"}, "medium"},
		{"表没配置任何策略", []string{"no_policy"}, "none"},
	}
	for _, c := range cases {
		got, err := r.TableClassification(ctx, ws.ID, c.tables)
		if err != nil {
			t.Fatalf("%s: 分级失败: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: 分级应为 %s，实际 %s", c.name, c.want, got)
		}
	}
	// 跨工作区不串味：其他工作区没有策略。
	if got, err := r.TableClassification(ctx, other.ID, []string{"users"}); err != nil || got != "none" {
		t.Fatalf("跨工作区应为 none: %q %v", got, err)
	}
}

// 文档站「最近一次成功/失败」：DISTINCT ON 取每个 (管道, 节点) 的最新终态。
// 非终态（pending/running）不参与，缺 finished_at 的旧数据用 created_at 兜底。
func TestLatestNodeRuns(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	p1 := seedPipeline(t, r, ws.ID, "p1")
	p2 := seedPipeline(t, r, ws.ID, "p2")

	oldRun := seedInstance(t, r, instSample(ws.ID, p1.ID, "r-old", "2024-01-01", "success"))
	newRun := seedInstance(t, r, instSample(ws.ID, p1.ID, "r-new", "2024-01-02", "failed"))
	pendingRun := seedInstance(t, r, instSample(ws.ID, p1.ID, "r-pending", "2024-01-03", "running"))
	otherRun := seedInstance(t, r, instSample(ws.ID, p2.ID, "r-p2", "2024-01-01", "success"))

	for _, inst := range []*model.Instance{oldRun, newRun, pendingRun, otherRun} {
		if err := r.AddTasks(ctx, inst.ID, []model.NodeSpec{{ID: "load", Type: "sql"}, {ID: "extract", Type: "sql"}}); err != nil {
			t.Fatalf("写入任务失败: %v", err)
		}
	}
	// 三次运行的任务都结束，只有 pendingRun 的保持 pending。
	execSQL(t, db, `UPDATE task_instance SET status='success', finished_at=now() - interval '2 hours' WHERE instance_id=$1`, oldRun.ID)
	execSQL(t, db, `UPDATE task_instance SET status='failed', finished_at=now() - interval '1 hour' WHERE instance_id=$1 AND node_id='load'`, newRun.ID)
	execSQL(t, db, `UPDATE task_instance SET status='success', finished_at=now() - interval '1 hour' WHERE instance_id=$1 AND node_id='extract'`, newRun.ID)
	execSQL(t, db, `UPDATE task_instance SET status='success', finished_at=now() WHERE instance_id=$1`, otherRun.ID)

	runs, err := r.LatestNodeRuns(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取节点最近运行失败: %v", err)
	}
	// p1 两个节点 + p2 两个节点；pending 那次不产生新行。
	if len(runs) != 4 {
		t.Fatalf("应有 4 条（2 管道 × 2 节点），实际 %d: %+v", len(runs), runs)
	}
	got := map[string]NodeRunView{}
	for _, n := range runs {
		got[n.PipelineID+"/"+n.NodeID] = NodeRunView{n.InstanceID, n.Status, n.BizDate, n.FinishedAt}
	}
	load := got[p1.ID+"/load"]
	if load.instanceID != newRun.ID || load.status != "failed" {
		t.Fatalf("p1.load 应取最新那次失败: %+v", load)
	}
	if load.bizDate != "2024-01-02" {
		t.Fatalf("业务日期应是 YYYY-MM-DD: %q", load.bizDate)
	}
	extract := got[p1.ID+"/extract"]
	if extract.instanceID != newRun.ID || extract.status != "success" {
		t.Fatalf("p1.extract 应取最新那次成功: %+v", extract)
	}
	if extract.finishedAt.IsZero() {
		t.Fatalf("应有完成时间: %+v", extract)
	}
	if got[p2.ID+"/load"].instanceID != otherRun.ID {
		t.Fatalf("p2.load 应来自另一条管道: %+v", got[p2.ID+"/load"])
	}

	// 缺 finished_at 时用 created_at 兜底：p1.load 的旧记录不该抢走最新位。
	execSQL(t, db, `UPDATE task_instance SET finished_at = NULL WHERE instance_id=$1 AND node_id='load'`, newRun.ID)
	execSQL(t, db, `UPDATE instance SET created_at = now() WHERE id=$1`, newRun.ID)
	runs, err = r.LatestNodeRuns(ctx, ws.ID)
	if err != nil {
		t.Fatalf("复查节点运行失败: %v", err)
	}
	for _, n := range runs {
		if n.PipelineID == p1.ID && n.NodeID == "load" && n.InstanceID != newRun.ID {
			t.Fatalf("缺 finished_at 时应按 created_at 兜底: %+v", n)
		}
	}

	// 其他工作区没有数据。
	if got, err := r.LatestNodeRuns(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区应为空: %v %v", got, err)
	}
}

type NodeRunView struct {
	instanceID string
	status     string
	bizDate    string
	finishedAt time.Time
}

// 文档站「这条规则上次过没过」：按 (管道, 节点, 规则名) 取最新一条。
// 只按规则名会串台（id_not_null 到处都是），这里顺手验证不会串。
func TestLatestQualityResults(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	p1 := seedPipeline(t, r, ws.ID, "p1")
	p2 := seedPipeline(t, r, ws.ID, "p2")

	run1 := seedInstance(t, r, instSample(ws.ID, p1.ID, "r1", "2024-01-01", "failed"))
	run2 := seedInstance(t, r, instSample(ws.ID, p1.ID, "r2", "2024-01-02", "success"))
	runOther := seedInstance(t, r, instSample(ws.ID, p2.ID, "r3", "2024-01-02", "failed"))

	save := func(inst *model.Instance, node, rule string, passed bool, observed string, ago time.Duration) {
		t.Helper()
		if err := r.SaveQualityResults(ctx, inst.ID, node, []qualityRow{
			{RuleName: rule, Observed: observed, Expected: "0", Severity: "error", Passed: passed},
		}); err != nil {
			t.Fatalf("写入质量结果失败: %v", err)
		}
		execSQL(t, db, `UPDATE quality_result SET created_at = now() - $3::interval
			WHERE instance_id=$1 AND rule_name=$2`,
			inst.ID, rule, ago.String())
	}
	save(run1, "load", "id_not_null", false, "3", 2*time.Hour)
	save(run2, "load", "id_not_null", true, "0", time.Hour)
	// 同名规则在另一条管道上必须分开统计。
	save(runOther, "load", "id_not_null", false, "7", time.Hour)

	snaps, err := r.LatestQualityResults(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取最近质量结果失败: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("应有 2 条（每条管道各一条），实际 %d: %+v", len(snaps), snaps)
	}
	byPipeline := map[string]QualityView{}
	for _, s := range snaps {
		byPipeline[s.PipelineID] = QualityView{s.RuleName, s.Passed, s.Observed, s.At}
	}
	if v := byPipeline[p1.ID]; !v.passed || v.observed != "0" || v.rule != "id_not_null" {
		t.Fatalf("p1 应取最新那次通过的结果: %+v", v)
	}
	if v := byPipeline[p2.ID]; v.passed || v.observed != "7" {
		t.Fatalf("p2 应独立统计: %+v", v)
	}
	if byPipeline[p1.ID].at.IsZero() {
		t.Fatalf("应带判定时间")
	}
	if got, err := r.LatestQualityResults(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区应为空: %v %v", got, err)
	}
}

type QualityView struct {
	rule     string
	passed   bool
	observed string
	at       time.Time
}

// 模型版本数：没保存过版本的模型也要出现（LEFT JOIN 出 0），
// 否则文档站会把「建过但没发版」的模型整个漏掉。
func TestModelVersionCounts(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	spec := model.ModelSpec{Kind: "model", Name: "dwd_order", Layer: "dwd"}
	if _, err := r.SaveModel(ctx, ws.ID, spec, "", "", "tester", false); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	if _, err := r.SaveModel(ctx, ws.ID, spec, "", "", "tester", true); err != nil {
		t.Fatalf("发布模型失败: %v", err)
	}
	// 直接插一个没有版本行的模型，模拟「刚建出来还没保存过定义」。
	execSQL(t, db, `INSERT INTO model(id,workspace_id,name,layer) VALUES('mdl_nover',$1,'dim_user','dim')`, ws.ID)
	// 其他工作区的模型不应出现。
	if _, err := r.SaveModel(ctx, other.ID, spec, "", "", "tester", false); err != nil {
		t.Fatalf("其他工作区保存模型失败: %v", err)
	}

	counts, err := r.ModelVersionCounts(ctx, ws.ID)
	if err != nil {
		t.Fatalf("统计版本数失败: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("应有 2 个模型，实际 %+v", counts)
	}
	if counts["dwd_order"] != 2 {
		t.Fatalf("dwd_order 应有 2 个版本，实际 %d", counts["dwd_order"])
	}
	if n, ok := counts["dim_user"]; !ok || n != 0 {
		t.Fatalf("无版本模型应记为 0: %v", counts)
	}
	if got, err := r.ModelVersionCounts(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空 map: %v %v", got, err)
	}
	// 名称里不应出现 SQL 片段之类的内容（这里顺便确认 key 就是模型名）。
	for name := range counts {
		if strings.Contains(name, " ") {
			t.Fatalf("模型名不对: %q", name)
		}
	}
}
