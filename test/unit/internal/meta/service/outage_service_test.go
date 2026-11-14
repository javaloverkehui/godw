package service_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/testsupport"
)

// 数据库中断是真实运行场景（主从切换、连接池耗尽）。此时每个用例都应该
// 返回可读错误而不是 panic / 半写状态。这里用独立 schema 的库，跑完直接关池，
// 逐个调用入口断言「快速失败」。
func TestDatabaseOutagePaths(t *testing.T) {
	db := testsupport.DB(t)
	svc, err := service.New(db, testsupport.Config(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	ws, err := svc.Bootstrap(ctxBG())
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	publishPipeline(t, svc, ws.ID, simplePipeline("outage_pipe"))
	// 关闭底层连接池，后续所有查询都会失败。
	db.Pool.Close()

	cases := []struct {
		name string
		call func() error
	}{
		{"Bootstrap", func() error { _, err := svc.Bootstrap(ctxBG()); return err }},
		{"CreateDatasource", func() error {
			_, err := svc.CreateDatasource(ctxBG(), ws.ID, "pg2", "postgres", map[string]any{
				"host": "127.0.0.1", "port": "5432", "user": "godw", "database": "godw", "sslmode": "disable",
			}, map[string]string{"password": "godw"})
			return err
		}},
		{"TestDatasource", func() error { return svc.TestDatasource(ctxBG(), ws.ID, "pg") }},
		{"CollectMetadata", func() error { _, err := svc.CollectMetadata(ctxBG(), ws.ID, "pg"); return err }},
		{"ApplyModelSpec", func() error {
			_, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "alice", modelSpec("pg", "dwd_outage", "dw", "dwd_outage"), true, false, true)
			return err
		}},
		{"PreviewModelDDL", func() error {
			_, err := svc.PreviewModelDDL(ctxBG(), ws.ID, modelSpec("pg", "dwd_outage", "dw", "dwd_outage"))
			return err
		}},
		{"ApplyPipeline", func() error {
			_, err := svc.ApplyPipeline(ctxBG(), ws.ID, "alice", []byte("kind: Pipeline\nname: p_out\nnodes:\n  - id: s\n    type: source\n    sql: SELECT 1\n"), true)
			return err
		}},
		{"ApplyMetric", func() error { _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("m", "dwd_outage")); return err }},
		{"QueryMetric", func() error { _, err := svc.QueryMetric(ctxBG(), ws.ID, "m", semantic.Query{}); return err }},
		{"ApplyDashboard", func() error {
			_, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{Name: "d", Cards: []semantic.Card{{Title: "t", Metric: "m", Days: 1}}})
			return err
		}},
		{"QueryDashboard", func() error { _, err := svc.QueryDashboard(ctxBG(), ws.ID, "d"); return err }},
		{"PlanBackfill", func() error {
			_, err := svc.PlanBackfill(ctxBG(), ws.ID, "outage_pipe", "2026-01-01", "2026-01-02")
			return err
		}},
		{"Backfill", func() error {
			_, err := svc.Backfill(ctxBG(), ws.ID, "outage_pipe", "2026-01-01", "2026-01-02", "", "alice", false)
			return err
		}},
		{"BackfillProgress", func() error { _, err := svc.BackfillProgress(ctxBG(), ws.ID, "bf_x"); return err }},
		{"ListBackfills", func() error { _, err := svc.ListBackfills(ctxBG(), ws.ID, 5); return err }},
		{"CatalogDocs", func() error { _, err := svc.CatalogDocs(ctxBG(), ws.ID); return err }},
		{"CatalogHTML", func() error { _, err := svc.CatalogHTML(ctxBG(), ws.ID); return err }},
		{"DetectPII", func() error { _, err := svc.DetectPII(ctxBG(), ws.ID, "pg", 10); return err }},
		{"Search", func() error { _, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "x"}); return err }},
		{"SaveTag", func() error { _, err := svc.SaveTag(ctxBG(), ws.ID, "t", "", ""); return err }},
		{"ListTags", func() error { _, err := svc.ListTags(ctxBG(), ws.ID); return err }},
		{"DeleteTag", func() error { return svc.DeleteTag(ctxBG(), ws.ID, "t") }},
		{"TagEntity", func() error { return svc.TagEntity(ctxBG(), ws.ID, "model", "x", []string{"t"}) }},
		{"UntagEntity", func() error { return svc.UntagEntity(ctxBG(), ws.ID, "model", "x", "t") }},
		{"EntityTags", func() error { _, err := svc.EntityTags(ctxBG(), ws.ID, "model", "x"); return err }},
		{"EntitiesByTag", func() error { _, err := svc.EntitiesByTag(ctxBG(), ws.ID, "t", 5); return err }},
		{"EnsureEnvironments", func() error { return svc.EnsureEnvironments(ctxBG(), ws.ID) }},
		{"ListEnvironments", func() error { _, err := svc.ListEnvironments(ctxBG(), ws.ID); return err }},
		{"BindEnv", func() error { return svc.BindEnv(ctxBG(), ws.ID, "dev", "a", "b") }},
		{"ResolveDatasource", func() error { _, err := svc.ResolveDatasource(ctxBG(), ws.ID, "dev", "a"); return err }},
		{"EnvBindings", func() error { _, err := svc.EnvBindings(ctxBG(), ws.ID, "dev"); return err }},
		{"DeleteWorkspace", func() error { return svc.DeleteWorkspace(ctxBG(), ws.ID) }},
		{"CollectWithDiff", func() error { _, err := svc.CollectWithDiff(ctxBG(), ws.ID, "pg"); return err }},
		{"ListSchemaChanges", func() error { _, err := svc.ListSchemaChanges(ctxBG(), ws.ID, 5); return err }},
		{"ApplyPIIFindings", func() error {
			_, err := svc.ApplyPIIFindings(ctxBG(), ws.ID, "pg", []service.PIIFinding{{Table: "t", Column: "c", Level: service.PIILow}})
			return err
		}},
		{"PlanRetention", func() error { _, err := svc.PlanRetention(ctxBG(), ws.ID, "m"); return err }},
		{"RunRetention", func() error { _, err := svc.RunRetention(ctxBG(), ws.ID, "m", "alice"); return err }},
		{"RequestErasure", func() error {
			_, err := svc.RequestErasure(ctxBG(), ws.ID, "m", "id", "1", "r", "alice")
			return err
		}},
		{"ExportTables", func() error { _, err := svc.ExportTables(ctxBG(), ws.ID, "metric", "m"); return err }},
		{"ExportSensitivity", func() error { _, _, err := svc.ExportSensitivity(ctxBG(), ws.ID, "metric", "m"); return err }},
		{"Dispatch", func() error { _, err := svc.Dispatch(ctxBG(), ws.ID); return err }},
		{"UpsertResourceGroup", func() error { return svc.UpsertResourceGroup(ctxBG(), ws.ID, "g", 2, 60, 5, nil) }},
		{"WaiveQuality", func() error { return svc.WaiveQuality(ctxBG(), ws.ID, "ins_x", "n", "r", "a") }},
		{"QualityWaivers", func() error { _, err := svc.QualityWaivers(ctxBG(), ws.ID, "ins_x"); return err }},
		{"CreateExport", func() error {
			_, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "m", nil, "alice")
			return err
		}},
		{"ApproveExport", func() error { _, err := svc.ApproveExport(ctxBG(), ws.ID, "exp_x", "admin"); return err }},
		{"RejectExport", func() error { _, err := svc.RejectExport(ctxBG(), ws.ID, "exp_x", "r", "admin"); return err }},
		{"RunSubscription", func() error {
			_, err := svc.RunSubscription(ctxBG(), ws.ID, model.Subscription{Name: "s", Kind: "metric", Target: "m", Channel: "file"})
			return err
		}},
		{"Impact", func() error { _, err := svc.Impact(ctxBG(), ws.ID, "t", "c", 3); return err }},
		{"PreviewTable", func() error {
			_, err := svc.PreviewTable(ctxBG(), ws.ID, "pg", "s", "t", "", "", 10)
			return err
		}},
		{"ConnectorFor", func() error { _, err := svc.ConnectorFor(ctxBG(), ws.ID, "pg"); return err }},
		{"LineageGraph", func() error { _, err := svc.LineageGraph(ctxBG(), ws.ID); return err }},
		{"LineageFocus", func() error { _, err := svc.LineageFocus(ctxBG(), ws.ID, "t", "up", 2); return err }},
		{"LineagePaths", func() error { _, err := svc.LineagePaths(ctxBG(), ws.ID, "a", "b", 3); return err }},
		{"LineageDOT", func() error { _, err := svc.LineageDOT(ctxBG(), ws.ID, ""); return err }},
		{"ColumnGraph", func() error { _, err := svc.ColumnGraph(ctxBG(), ws.ID, "t", "", "", 2); return err }},
		{"ModelDiff", func() error { _, err := svc.ModelDiff(ctxBG(), ws.ID, modelSpec("pg", "x", "dw", "x")); return err }},
		{"RollbackPreview", func() error { _, err := svc.RollbackPreview(ctxBG(), ws.ID, "x", 1); return err }},
		{"Dictionary", func() error { _, err := svc.Dictionary(ctxBG(), ws.ID); return err }},
		{"DictionaryMarkdown", func() error { _, err := svc.DictionaryMarkdown(ctxBG(), ws.ID); return err }},
		{"PublishedModels", func() error { _, err := svc.PublishedModels(ctxBG(), ws.ID); return err }},
		{"ApplyDataset", func() error { _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("d", "m", "dt")); return err }},
		{"DatasetQuery", func() error {
			_, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{Dataset: "d"}, "", "")
			return err
		}},
		{"ApplyChart", func() error {
			_, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{Name: "c", Type: "line", Dataset: "d"})
			return err
		}},
		{"ChartQuery", func() error { _, err := svc.ChartQuery(ctxBG(), ws.ID, "c", "", ""); return err }},
		{"DatasetUsage", func() error { _, err := svc.DatasetUsage(ctxBG(), ws.ID, "d"); return err }},
		{"Trigger", func() error {
			_, _, err := svc.Trigger(ctxBG(), ws.ID, "outage_pipe", "2026-01-01", "manual", nil)
			return err
		}},
		{"TriggerByPipelineID", func() error {
			_, _, err := svc.TriggerByPipelineID(ctxBG(), ws.ID, "pl", "2026-01-01", "retry", nil)
			return err
		}},
		{"RetryNode", func() error { _, err := svc.RetryNode(ctxBG(), "ins", "n"); return err }},
		{"RunOnce", func() error { _, err := svc.RunOnce(ctxBG(), "w"); return err }},
		{"Recover", func() error { _, err := svc.Recover(ctxBG()); return err }},
		{"InstanceNodeSQL", func() error { _, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, "ins", "n"); return err }},
		{"Profile", func() error { _, err := svc.Profile(ctxBG(), ws.ID, "pg", "s", "t", 10); return err }},
		{"RawSQLQuery", func() error {
			_, err := svc.RawSQLQuery(ctxBG(), ws.ID, "pg", "SELECT 1", "", "", 10)
			return err
		}},
		{"Logs", func() error { _, err := svc.Logs(ctxBG(), "ins", 0, 10); return err }},
	}
	for _, c := range cases {
		if err := c.call(); err == nil {
			t.Errorf("%s 在数据库不可用时应返回错误", c.name)
		}
	}

	// 这些方法在 DB 故障时按设计「降级」而不是报错，但要保证可调用、不 panic。
	if got, applied := svc.MaskRows(ctxBG(), ws.ID, "t", []string{"a"}, [][]any{{"x"}}); got[0][0] != "x" || len(applied) != 0 {
		t.Errorf("service.MaskRows 降级应原样返回: %+v %+v", got, applied)
	}
	if sim, err := svc.SimulatePolicies(ctxBG(), ws.ID, "t", "u", "viewer"); !errs.Is(err, errs.CodeInternal) && err == nil {
		t.Errorf("SimulatePolicies 应返回错误: %+v", sim)
	}
	if _, err := svc.ScaffoldModel(ctxBG(), ws.ID, modelc.ScaffoldOptions{
		Layer: "dwd", Name: "x", Datasource: "pg", Schema: "dw",
		Upstream: []model.ColumnSpec{{Name: "id", Type: "bigint"}, {Name: "dt", Type: "date"}},
	}); err != nil {
		t.Errorf("ScaffoldModel 有上游列时应降级成功: %v", err)
	}
	if res, err := svc.DryRun(ctxBG(), ws.ID, model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
		{ID: "w", Type: model.NodeSQL, Model: "m", SQL: "SELECT 1"},
	}}, "2026-01-01", nil); err != nil || res.Nodes[0].Error == "" {
		t.Errorf("DryRun 应把模型不可读降级成节点错误: %v %+v", err, res)
	}
	svc.AppendLog("ins", "", "info", "db down")
	svc.ReleaseLogs(ctxBG(), "ins")
}

// 真实执行 DDL：模型发布时勾选 apply_ddl，表必须真的建出来。
func TestApplyModelSpecExecutesDDL(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	spec := modelSpec(ds.Name, "dwd_ddl_run", p.schema, "dwd_ddl_t")

	m, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "alice", spec, true, true, false)
	if err != nil {
		t.Fatalf("执行 DDL 失败: %v", err)
	}
	if m.ActiveVersion != 1 {
		t.Fatalf("应发布成功: %+v", m)
	}
	var n int
	if err := p.conn.QueryRow(ctxBG(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='dwd_ddl_t'`,
		p.schema).Scan(&n); err != nil {
		t.Fatalf("查询表失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("表应被真实创建: %d", n)
	}
}

// 采样带模型：数据源可从模型推导，且增量条件要注入，保证采到的就是「本次会跑的范围」。
func TestSampleRunWithModelInference(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text)`, p.table("dwd_sm_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,1,'a'),('2026-08-02',2,2,'b')`, p.table("dwd_sm_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_sm", p.schema, "dwd_sm_t"))

	spec := model.PipelineSpec{Name: "sample_model", Nodes: []model.NodeSpec{
		{ID: "w", Type: model.NodeSQL, Model: "dwd_sm",
			SQL: "SELECT dt, id, amount FROM " + p.table("dwd_sm_t")},
	}}
	res, err := svc.SampleRun(ctxBG(), ws.ID, spec, "", "2026-08-01", 10)
	if err != nil {
		t.Fatalf("SampleRun 失败: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("增量条件应只采到业务日期当天: %+v", res.Rows)
	}
	if !strings.Contains(res.SQL, "godw_sample") {
		t.Fatalf("采样应包一层 LIMIT: %s", res.SQL)
	}
}

// 指标查询缓存：开启后第二次查询应命中缓存，且带 Cached 标记。
func TestQueryMetricCacheHit(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_cache", "dwd_sem")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	svc.CacheTTL = time.Minute

	q := semantic.Query{Granularity: "day"}
	first, err := svc.QueryMetricFor(ctxBG(), ws.ID, "gmv_cache", q, "alice", "viewer")
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if first.Cached {
		t.Fatal("首次查询不应命中缓存")
	}
	second, err := svc.QueryMetricFor(ctxBG(), ws.ID, "gmv_cache", q, "alice", "viewer")
	if err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if !second.Cached || len(second.Points) != len(first.Points) {
		t.Fatalf("二次查询应命中缓存: %+v", second)
	}
}

// 删除权：RequestErasure 的异步路径也要真的删掉数据。
func TestRequestErasureAsync(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, user_id text)`, p.table("dwd_erasync_t"))
	p.exec(t, `INSERT INTO %s VALUES (now(),1,'u1'),(now(),2,'u2')`, p.table("dwd_erasync_t"))
	spec := modelSpec(ds.Name, "dwd_erasync", p.schema, "dwd_erasync_t")
	spec.Columns = append(spec.Columns, model.ColumnSpec{Name: "user_id", Type: "string"})
	publishModel(t, svc, ws.ID, spec)

	job, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_erasync", "user_id", "u1", "用户申请", "alice")
	if err != nil {
		t.Fatalf("RequestErasure 失败: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := svc.Repo.ListErasureJobs(ctxBG(), ws.ID, 10)
		for _, j := range jobs {
			if j.ID == job.ID && j.Status == "done" {
				if j.RowsDeleted != 1 {
					t.Fatalf("应删除 1 行: %+v", j)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("删除任务超时未完成: %s", job.ID)
}

// runErasure 失败：目标表不存在时任务必须落 failed 并带原因，而不是卡在 running。
func TestRunErasureFailure(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_erfail", "no_such_schema", "dwd_erfail_t")
	publishModel(t, svc, ws.ID, spec)
	created, err := svc.RequestErasure(ctxBG(), ws.ID, "dwd_erfail", "id", "1", "r", "alice")
	if err != nil {
		t.Fatalf("提交删除请求失败: %v", err)
	}
	done := waitErasure(t, svc, ws.ID, created.ID)
	if done.Status != "failed" || done.Error == "" {
		t.Fatalf("表不存在时任务应失败并留原因: %+v", done)
	}
}

// 导出目录不可写时要立刻失败，不能留下 running 的任务。
func TestRunExportDirFailure(t *testing.T) {
	svc, ws := newSvc(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "not_a_dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	svc.Cfg.Semantic.ExportDir = file // 让 MkdirAll 失败

	job, err := svc.CreateExport(ctxBG(), ws.ID, "metric", "m", nil, "alice")
	if err != nil {
		t.Fatalf("提交导出失败: %v", err)
	}
	got := waitStatus(t, svc, ws.ID, job.ID)
	if got.Status != "failed" || !strings.Contains(got.Error, "创建导出目录失败") {
		t.Fatalf("目录不可写应失败: %+v", got)
	}
}

var _ = context.Background
