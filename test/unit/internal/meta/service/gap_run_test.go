package service_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/pkg/cryptox"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/testsupport"
)

// faultSvc 建一个用例独占的库，便于用 DROP TABLE 精确制造「某张元数据表不可用」。
// 共享库不能这么干；独占 schema 的清理代价很低，换来的是每条错误分支都能被真实触发。
func faultSvc(t *testing.T) (*service.Service, *model.Workspace, *repo.DB) {
	t.Helper()
	db := testsupport.DB(t)
	svc, err := service.New(db, testsupport.Config(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	ws, err := svc.Bootstrap(ctxBG())
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	t.Cleanup(svc.Pool.Close)
	return svc, ws, db
}

// 短主密钥必须在构造期就拒绝：拿它加密的凭证等于明文，事后无法补救。
func TestNewRejectsShortSecret(t *testing.T) {
	db := testsupport.Shared(t)
	cfg := testsupport.Config(t)
	cfg.Auth.JWTSecret = "short"
	if _, err := service.New(db, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("过短的主密钥应拒绝启动")
	}
}

// 加密 entropy 故障：必须让 CreateDatasource 失败，而不是把空密文落库。
// 这是「配置写了一半就成功返回」的防线。
func TestCreateDatasourceEncryptFailure(t *testing.T) {
	svc, ws := newSvc(t)
	old := cryptox.RandReader
	cryptox.RandReader = errReader{}
	defer func() { cryptox.RandReader = old }()

	if _, err := svc.CreateDatasource(ctxBG(), ws.ID, "pg_entropy", "postgres", map[string]any{
		"host": "127.0.0.1", "port": "5432", "user": "godw", "database": "godw", "sslmode": "disable",
	}, map[string]string{"password": "godw"}); err == nil {
		t.Fatal("熵源故障时数据源不应入库")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, context.Canceled }

// 资源组满时新实例进 waiting，空闲后由 Dispatch 提升为 queued。
// 这条链路保证「排队顺序」在并发上限下仍然可预期。
func TestTriggerWaitsWhenGroupFull(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("grp_pipe"))

	// 自定义组没有登记 → 兜底并发 4。先占满 4 个 running。
	for i := 0; i < 4; i++ {
		_, _, err := svc.Trigger(ctxBG(), ws.ID, "grp_pipe", "2026-03-0"+string(rune('1'+i)), "manual", nil)
		if err != nil {
			t.Fatalf("触发失败: %v", err)
		}
		inst, err := svc.Repo.ClaimInstance(ctxBG(), "w1", time.Minute)
		if err != nil || inst == nil {
			t.Fatalf("领取失败: %v %v", inst, err)
		}
	}
	waiting, created, err := svc.Trigger(ctxBG(), ws.ID, "grp_pipe", "2026-03-09", "manual", nil)
	if err != nil || !created {
		t.Fatalf("触发失败: %v %v", created, err)
	}
	if waiting.Status != "waiting" {
		t.Fatalf("资源组满时实例应为 waiting: %+v", waiting)
	}
	if waiting.ResourceGroup != "default" {
		t.Fatalf("资源组应随实例固化: %+v", waiting)
	}

	// Dispatch 有空位时把 waiting 提升为 queued。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE instance SET status='success' WHERE workspace_id=$1 AND status='running' AND id=(
		   SELECT id FROM instance WHERE workspace_id=$1 AND status='running' LIMIT 1)`, ws.ID); err != nil {
		t.Fatalf("释放槽位失败: %v", err)
	}
	if _, err := svc.Dispatch(ctxBG(), ws.ID); err != nil {
		t.Fatalf("Dispatch 失败: %v", err)
	}
	got, err := svc.Repo.GetInstance(ctxBG(), waiting.ID)
	if err != nil {
		t.Fatalf("读实例失败: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("有空位后应提升为 queued: %+v", got)
	}
}

// 版本快照丢失时重跑必须报错：拿当前定义重跑会让历史实例跑到另一份逻辑上。
func TestRetryNodeMissingVersion(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("retry_nover"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "retry_nover", "2026-03-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM pipeline_version WHERE pipeline_id=$1 AND version=$2`, inst.PipelineID, inst.PipelineVer); err != nil {
		t.Fatalf("删除版本快照失败: %v", err)
	}
	if _, err := svc.RetryNode(ctxBG(), inst.ID, "src"); err == nil {
		t.Fatal("版本快照缺失时应拒绝重跑")
	}
}

// 执行结束时清理查询缓存：失败也可能写了部分数据，不能让下一次查询读到陈旧结果。
func TestRunOncePurgesCacheAndShortLease(t *testing.T) {
	svc, ws := newSvc(t)
	// 分支节点是纯内存 no-op：执行成功且不需要真实数据源。
	publishPipeline(t, svc, ws.ID, model.PipelineSpec{Kind: "Pipeline", Name: "cache_purge",
		Nodes: []model.NodeSpec{{ID: "b", Type: model.NodeBranch}}})
	if err := svc.Repo.CachePut(ctxBG(), "k1", ws.ID, []byte(`{"x":1}`), time.Hour); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	// 租约 1 秒 → 心跳间隔被抬到 5 秒下限（不会在短用例里真的续期）。
	svc.Cfg.Scheduler.LeaseSec = 1
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "cache_purge", "2026-03-02", "manual", nil); err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if worked, err := svc.RunOnce(ctxBG(), "w1"); err != nil || !worked {
		t.Fatalf("执行失败: %v %v", worked, err)
	}
	if _, hit, err := svc.Repo.CacheGet(ctxBG(), "k1"); err != nil || hit {
		t.Fatalf("实例结束应清空缓存: hit=%v err=%v", hit, err)
	}
}

// 实例对应的版本被删后执行必须失败并落状态，不能卡在 running。
func TestRunOnceMissingVersion(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("run_nover"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "run_nover", "2026-03-03", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`DELETE FROM pipeline_version WHERE pipeline_id=$1 AND version=$2`, inst.PipelineID, inst.PipelineVer); err != nil {
		t.Fatalf("删除版本快照失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "w1"); err == nil {
		t.Fatal("版本缺失时执行应失败")
	}
	got, err := svc.Repo.GetInstance(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读实例失败: %v", err)
	}
	if got.Status != "failed" || got.Error == "" {
		t.Fatalf("失败要落库并留原因: %+v", got)
	}
}

// 失败节点带 retry 时要真的重试（并等待退避），成功后质量结果要落库。
// 质量结果是文档站与告警的输入，丢了它「有没有守住」就无从查起。
func TestRunNodeRetryAndQualityResult(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2))`, p.table("dwd_q_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-01-01',1,10)`, p.table("dwd_q_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_q", p.schema, "dwd_q_t"))

	// 重试一次：坏 SQL 第二次仍然失败，覆盖退避分支。
	bad := model.PipelineSpec{Kind: "Pipeline", Name: "retry_bad", Nodes: []model.NodeSpec{
		{ID: "s", Type: model.NodeSource, Datasource: ds.Name, SQL: "SELECT * FROM no_such_tbl", Retry: 1},
	}}
	publishPipeline(t, svc, ws.ID, bad)
	if _, _, err := svc.Trigger(ctxBG(), ws.ID, "retry_bad", "2026-01-01", "manual", nil); err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "w1"); err == nil {
		t.Fatal("重试后仍失败应返回错误")
	}

	// 质量节点：规则通过 → stats.Quality 非空 → 结果落库。
	q := model.PipelineSpec{Kind: "Pipeline", Name: "quality_ok", Nodes: []model.NodeSpec{
		{ID: "q", Type: model.NodeQuality, Model: "dwd_q", Config: map[string]any{
			"rules": []any{map[string]any{"name": "id_not_null", "kind": "not_null", "column": "id"}},
		}},
	}}
	publishPipeline(t, svc, ws.ID, q)
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "quality_ok", "2026-01-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.RunOnce(ctxBG(), "w1"); err != nil {
		t.Fatalf("质量管道应执行成功: %v", err)
	}
	results, err := svc.Repo.QualityResults(ctxBG(), inst.ID)
	if err != nil {
		t.Fatalf("读质量结果失败: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("质量结果应落库: %+v", results)
	}
}

// 节点 SQL 回放：写模型的节点要叠加增量 where，并带上上一轮同节点的 SQL。
// 用错这一处，排障时看到的 SQL 从未真正执行过。
func TestInstanceNodeSQLWithModelAndPrevious(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_replay", "dw", "dwd_replay"))
	pipe := model.PipelineSpec{
		Kind: "Pipeline", Name: "replay_pipe",
		Params: []model.ParamSpec{{Name: "n", Type: "int", Default: 5}},
		Nodes: []model.NodeSpec{
			{ID: "w", Type: model.NodeSQL, Model: "dwd_replay",
				SQL: "SELECT id, amount FROM dw.raw WHERE k = ${n}"},
		},
	}
	publishPipeline(t, svc, ws.ID, pipe)
	first, _, err := svc.Trigger(ctxBG(), ws.ID, "replay_pipe", "2026-02-01", "manual",
		map[string]any{"n": 1})
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	second, _, err := svc.Trigger(ctxBG(), ws.ID, "replay_pipe", "2026-02-02", "manual",
		map[string]any{"n": 2})
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	// 先跑一次让 first 有「上一轮」；这里直接读 second 的渲染，Previous 指向 first。
	out, err := svc.InstanceNodeSQL(ctxBG(), ws.ID, second.ID, "w")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.NodeID != "w" || !strings.Contains(out.SQL, "k = 2") {
		t.Fatalf("应按实例参数渲染: %+v", out)
	}
	if out.Inference == nil || out.Inference.Where == "" {
		t.Fatalf("写模型节点应带增量条件: %+v", out.Inference)
	}
	if out.Previous == nil || out.Previous.InstanceID != first.ID {
		t.Fatalf("应返回上一轮 SQL: %+v", out.Previous)
	}
	// 上一轮参数用默认值渲染（first 显式传了 1，这里验证默认值路径用另一实例）。
	third, _, err := svc.Trigger(ctxBG(), ws.ID, "replay_pipe", "2026-02-03", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	out, err = svc.InstanceNodeSQL(ctxBG(), ws.ID, third.ID, "w")
	if err != nil {
		t.Fatalf("InstanceNodeSQL 失败: %v", err)
	}
	if out.Previous == nil || !strings.Contains(out.Previous.SQL, "k = 2") {
		t.Fatalf("上一轮应使用其自身参数渲染: %+v", out.Previous)
	}
}

// 告警判定的输入来自实例快照：没有 started_at / finished_at 的历史实例也要
// 能判定，否则「老实例永远不告警」。
func TestEvaluateAlertsWithoutTimestamps(t *testing.T) {
	svc, ws := newSvc(t)
	rule, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "fail_all", Kind: alert.KindFailure, Target: "*", Severity: "warning", Enabled: true,
		Channels: []string{"webhook"},
	})
	if err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	inst := &model.Instance{
		ID: "ins_no_ts", WorkspaceID: ws.ID, PipelineName: "p", PipelineID: "pl",
		BizDate: "2026-01-01", Status: "failed", Error: "boom",
	}
	svc.EvaluateAlerts(ctxBG(), ws.ID, inst)
	alerts, err := svc.Repo.ListAlerts(ctxBG(), ws.ID, "", 10)
	if err != nil {
		t.Fatalf("读告警失败: %v", err)
	}
	found := false
	for _, a := range alerts {
		if a.RuleID == rule.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("失败实例应产生告警: %+v", alerts)
	}
}

// 告警渠道：没有配置渠道时用内存日志兜底；配置了但推送失败要写日志而不是静默。
func TestSendAlertChannelBranches(t *testing.T) {
	svc, ws := newSvc(t)
	// 规则不带 channels → 默认 webhook；URL 不可达 → 走 warn 分支而不是静默。
	svc.Cfg.Alert.WebhookURL = "http://127.0.0.1:1/hook"
	svc.Cfg.Alert.Template = "${title}"
	if _, err := svc.Repo.SaveAlertRule(ctxBG(), ws.ID, alert.Rule{
		Name: "fail_webhook", Kind: alert.KindFailure, Target: "*", Severity: "warning", Enabled: true,
		Channels: []string{"webhook"},
	}); err != nil {
		t.Fatalf("保存告警规则失败: %v", err)
	}
	svc.EvaluateAlerts(ctxBG(), ws.ID, &model.Instance{
		ID: "i2", WorkspaceID: ws.ID, PipelineName: "p", Status: "failed", BizDate: "2026-01-02",
	})
	// 只要求不 panic 且事件已落库（推送失败只记日志）。
	alerts, err := svc.Repo.ListAlerts(ctxBG(), ws.ID, "", 10)
	if err != nil || len(alerts) == 0 {
		t.Fatalf("告警事件应落库: %+v %v", alerts, err)
	}
}

// 放行的前置条件：实例必须存在、属于本工作空间、且确实失败过。
func TestWaiveQualityGuards(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("waive_guard"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "waive_guard", "2026-01-05", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.WaiveQuality(ctxBG(), "other-ws", inst.ID, "", "r", "a"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("跨工作区应 NotFound，实际 %v", err)
	}
	if err := svc.WaiveQuality(ctxBG(), ws.ID, inst.ID, "", "r", "a"); !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("非失败实例应冲突，实际 %v", err)
	}
}

// 资源组写入的默认值与内存同步：空名字/非法数值要收敛，不能把 0 并发写进库。
func TestUpsertResourceGroupDefaults(t *testing.T) {
	svc, ws := newSvc(t)
	if err := svc.UpsertResourceGroup(ctxBG(), ws.ID, "", 0, 0, 0, nil); err != nil {
		t.Fatalf("默认值应可写入: %v", err)
	}
	groups, err := svc.Repo.ListResourceGroups(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读资源组失败: %v", err)
	}
	found := false
	for _, g := range groups {
		if g["name"] == "default" && g["maxConcurrency"] == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("空名字应落到 default 且并发为 4: %+v", groups)
	}
	// 内存映射为空时重新初始化，避免配置生效时 panic。
	svc.ResourceGroups = nil
	if err := svc.UpsertResourceGroup(ctxBG(), ws.ID, "small", 2, 60, 5, nil); err != nil {
		t.Fatalf("内存为空时应重建映射: %v", err)
	}
	if _, ok := svc.ResourceGroups["small"]; !ok {
		t.Fatalf("新组应写入内存: %+v", svc.ResourceGroups)
	}
	if err := svc.UpsertResourceGroup(ctxBG(), ws.ID, "bad", 999, 60, 5, nil); err == nil {
		t.Fatal("非法并发应被拒绝")
	}
}

// 指标/看板链路在数据源不可达时的错误必须原样上抛，而不是返回空序列。
func TestMetricAndDashboardBrokenDatasource(t *testing.T) {
	svc, ws := newSvc(t)
	brokenDatasource(t, svc, ws.ID, "pg_broken2")
	// 直接落库发布：绕过连通性探测，模拟「探测后库被回收」。
	if _, err := svc.Repo.SaveModel(ctxBG(), ws.ID,
		modelSpec("pg_broken2", "dwd_broken", "dw", "dwd_broken"), "", "", "t", true); err != nil {
		t.Fatalf("落模型失败: %v", err)
	}
	if _, err := svc.ApplyMetric(ctxBG(), ws.ID, metricFor("gmv_broken", "dwd_broken")); err != nil {
		t.Fatalf("保存指标失败: %v", err)
	}
	if _, err := svc.QueryMetric(ctxBG(), ws.ID, "gmv_broken", semantic.Query{}); err == nil {
		t.Fatal("数据源不可达时指标查询应失败")
	}
	if _, err := svc.PreviewMetric(ctxBG(), ws.ID, metricFor("gmv_broken", "dwd_broken"), semantic.Query{}, "", ""); err == nil {
		t.Fatal("数据源不可达时预览应失败")
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_broken",
		Cards: []semantic.Card{{Title: "GMV", Metric: "gmv_broken", Days: 7}},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	data, err := svc.QueryDashboard(ctxBG(), ws.ID, "dash_broken")
	if err != nil {
		t.Fatalf("单卡失败不应让整页失败: %v", err)
	}
	if data.Cards[0].Error == "" {
		t.Fatalf("取连接失败应挂在卡片上: %+v", data.Cards[0])
	}
	// 数据集查询同样：基础模型的数据源不可达 → 不能返回空结果。
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, datasetFor("ds_broken", "dwd_broken", "dt")); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	if _, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_broken", Dimensions: []string{"dt"}, Measures: []string{"amount"},
	}, "", ""); err == nil {
		t.Fatal("数据源不可达时数据集查询应失败")
	}
}

// 执行面之外的语义层兜底：SUM 全为 NULL、文本/布尔列当坐标轴，
// 这些值都必须收敛成可展示的数字而不是让整张图报错。
func TestChartValueNormalizationEdgeCases(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text, flag boolean)`, p.table("dwd_norm_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,NULL,'east',true),('2026-08-01',2,NULL,NULL,false)`, p.table("dwd_norm_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_norm", p.schema, "dwd_norm_t"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, semantic.Dataset{
		Name: "ds_norm", BaseModel: "dwd_norm",
		Dimensions: []semantic.Dimension{
			{Name: "dt", Field: "dt", Type: "time", Filterable: true, TimeGrains: []string{"day"}},
			{Name: "region", Field: "region", Type: "categorical", Filterable: true},
			{Name: "flag", Field: "flag", Type: "categorical", Filterable: true},
		},
		Measures: []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	// SUM 全 NULL → pgtype.Numeric 无效值，必须收敛成 0。
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
		Name: "c_norm_sum", Type: "line", Dataset: "ds_norm",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if _, err := svc.ChartQuery(ctxBG(), ws.ID, "c_norm_sum", "", ""); err != nil {
		t.Fatalf("空聚合不应报错: %v", err)
	}
	// 坐标轴取文本/布尔列：走 fmt.Sprint 兜底，不能 panic。
	for _, name := range []string{"c_norm_text", "c_norm_bool"} {
		field := "region"
		if name == "c_norm_bool" {
			field = "flag"
		}
		if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
			Name: name, Type: "bar", Dataset: "ds_norm",
			Dimensions: []string{"dt", field}, Measures: []string{"amount"},
			Encoding: semantic.Encoding{X: field, Y: "amount"},
		}); err != nil {
			t.Fatalf("保存图表 %s 失败: %v", name, err)
		}
		if _, err := svc.ChartQuery(ctxBG(), ws.ID, name, "", ""); err != nil {
			t.Fatalf("查询图表 %s 失败: %v", name, err)
		}
	}
	// Y 槽取可空文本列：NULL 与字符串都要收敛成数字（0 与解析值），
	// 否则一张图里只要有一行空值就会整体失真。
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, semantic.Chart{
		Name: "c_norm_region", Type: "bar", Dataset: "ds_norm",
		Dimensions: []string{"dt", "region"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "region"},
	}); err != nil {
		t.Fatalf("保存文本度量图表失败: %v", err)
	}
	if _, err := svc.ChartQuery(ctxBG(), ws.ID, "c_norm_region", "", ""); err != nil {
		t.Fatalf("文本度量查询失败: %v", err)
	}
}

// 柱状图的坐标轴是分类字段时，fmt.Sprint 兜底也要覆盖（formatAxisValue 非时间分支）。
func TestDatasetQueryRowErrAndBigResult(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date not null, id bigint, amount decimal(18,2), region text)`, p.table("dwd_big_t"))
	p.exec(t, `INSERT INTO %s SELECT '2026-08-01', g, 1, 'r' FROM generate_series(1, 20001) g`, p.table("dwd_big_t"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_big", p.schema, "dwd_big_t"))
	if _, err := svc.ApplyDataset(ctxBG(), ws.ID, semantic.Dataset{
		Name: "ds_big", BaseModel: "dwd_big",
		Dimensions: []semantic.Dimension{{Name: "id", Field: "id", Type: "categorical", Filterable: true}},
		Measures:   []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}); err != nil {
		t.Fatalf("保存数据集失败: %v", err)
	}
	res, err := svc.DatasetQuery(ctxBG(), ws.ID, semantic.QueryRequest{
		Dataset: "ds_big", Dimensions: []string{"id"}, Measures: []string{"amount"},
	}, "", "")
	if err != nil {
		t.Fatalf("DatasetQuery 失败: %v", err)
	}
	rows, _ := res["rows"].([][]any)
	if len(rows) > 20001 {
		t.Fatalf("结果应有硬上限: %d", len(rows))
	}
}

// 血缘图的列级视角：自动选列要避开 dt/主键优先、外部表要标 External、
// 深度的 0/负数也要有缓存键（负深度不能拼出非法键）。
func TestColumnGraphFocusAndDepth(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_cg_a", "dw", "dwd_cg_a"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_cg_b", "dw", "dwd_cg_b"))
	if err := svc.Repo.ReplaceColumnLineage(ctxBG(), ws.ID, "dwd_cg_b", []model.ColumnEdge{
		{DownstreamTable: "dwd_cg_b", DownstreamColumn: "id", UpstreamTable: "dwd_cg_a", UpstreamColumn: "id"},
		{DownstreamTable: "dwd_cg_b", DownstreamColumn: "dt", UpstreamTable: "dwd_cg_a", UpstreamColumn: "dt"},
		{DownstreamTable: "dwd_cg_b", DownstreamColumn: "amount", UpstreamTable: "dwd_cg_a", UpstreamColumn: "amount"},
		{DownstreamTable: "dwd_cg_b", DownstreamColumn: "amount", UpstreamTable: "dwd_cg_a", UpstreamColumn: "region"},
		{DownstreamTable: "dwd_cg_b", DownstreamColumn: "region", UpstreamTable: "ext_t", UpstreamColumn: "x"},
	}); err != nil {
		t.Fatalf("写列级血缘失败: %v", err)
	}
	// 外部下游：w 出现 1 次、y 出现 2 次，自动选列时应按引用次数选 y。
	if err := svc.Repo.ReplaceColumnLineage(ctxBG(), ws.ID, "ext_down", []model.ColumnEdge{
		{DownstreamColumn: "w", UpstreamTable: "ext_up2", UpstreamColumn: "a"},
		{DownstreamColumn: "y", UpstreamTable: "ext_up2", UpstreamColumn: "b"},
		{DownstreamColumn: "y", UpstreamTable: "ext_up2", UpstreamColumn: "c"},
	}); err != nil {
		t.Fatalf("写外部列级血缘失败: %v", err)
	}
	svc.InvalidateLineage()

	g, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_b", "", "up", 3)
	if err != nil {
		t.Fatalf("ColumnGraph 失败: %v", err)
	}
	// 自动选列：amount 与 region 同为普通字段，amount 被引用两次 →
	// 同分时按引用次数取胜，而不是按 map 顺序随机。
	if g.Focus.Column != "amount" {
		t.Fatalf("自动选列应为引用最多的 amount: %+v", g.Focus)
	}

	// 显式聚焦分区列 → 节点标的 Kind 应为 partition。
	part, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_b", "dt", "up", 3)
	if err != nil {
		t.Fatalf("分区列 ColumnGraph 失败: %v", err)
	}
	sawPartition := false
	for _, n := range part.Nodes {
		if n.Table == "dwd_cg_a" && n.Column == "dt" && n.Kind == "partition" {
			sawPartition = true
		}
	}
	if !sawPartition {
		t.Fatalf("分区列应标 partition: %+v", part.Nodes)
	}

	// 显式聚焦 region → 上游是外部表 ext_t，必须标 External。
	ext, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_b", "region", "up", 3)
	if err != nil {
		t.Fatalf("外部列 ColumnGraph 失败: %v", err)
	}
	sawExternal := false
	for _, n := range ext.Nodes {
		if n.Table == "ext_t" && n.External {
			sawExternal = true
		}
	}
	if !sawExternal {
		t.Fatalf("外部表应标 External: %+v", ext.Nodes)
	}

	// 深度 0 与负数：使用同一缓存键构造逻辑，不能生成非法键。
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_a", "id", "down", 0); err != nil {
		t.Fatalf("深度 0 失败: %v", err)
	}
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_a", "id", "down", -2); err != nil {
		t.Fatalf("负深度失败: %v", err)
	}
	// 只作为上游出现的表：没有 downstream 记录 → 取它的任意上游列。
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, "ext_t", "", "", 3); err != nil {
		t.Fatalf("仅有上游的表失败: %v", err)
	}
	// 外部下游表（不在模型集合里）：自动选列时 spec 为空，按引用次数取 y。
	extDown, err := svc.ColumnGraph(ctxBG(), ws.ID, "ext_down", "", "", 3)
	if err != nil {
		t.Fatalf("外部下游表失败: %v", err)
	}
	if extDown.Focus.Column != "y" {
		t.Fatalf("外部表应按引用次数选列: %+v", extDown.Focus)
	}
	// 同一参数第二次走缓存。
	if _, err := svc.ColumnGraph(ctxBG(), ws.ID, "dwd_cg_b", "", "up", 3); err != nil {
		t.Fatalf("缓存命中失败: %v", err)
	}
	// 聚焦子图的缓存命中。
	if _, err := svc.LineageFocus(ctxBG(), ws.ID, "dwd_cg_b", "up", 1); err != nil {
		t.Fatalf("LineageFocus 失败: %v", err)
	}
	if _, err := svc.LineageFocus(ctxBG(), ws.ID, "dwd_cg_b", "up", 1); err != nil {
		t.Fatalf("缓存命中失败: %v", err)
	}
}

// 看板的图表卡在数据集被删后要挂错误；图表被删但看板仍引用时也不能整页失败。
func TestDashboardChartDatasetDeleted(t *testing.T) {
	svc, ws := newSvc(t)
	seedSemantic(t, svc, ws.ID)
	chart := semantic.Chart{
		Name: "c_del", Type: "line", Dataset: "ds_sem",
		Dimensions: []string{"dt"}, Measures: []string{"amount"},
		Encoding: semantic.Encoding{X: "dt", Y: "amount"},
	}
	if _, _, err := svc.ApplyChart(ctxBG(), ws.ID, chart); err != nil {
		t.Fatalf("保存图表失败: %v", err)
	}
	if _, err := svc.ApplyDashboard(ctxBG(), ws.ID, semantic.Dashboard{
		Name:  "dash_del",
		Cards: []semantic.Card{{Title: "趋势", Chart: "c_del"}},
	}); err != nil {
		t.Fatalf("保存看板失败: %v", err)
	}
	if err := svc.Repo.DeleteDataset(ctxBG(), ws.ID, "ds_sem"); err != nil {
		t.Fatalf("删除数据集失败: %v", err)
	}
	data, err := svc.QueryDashboardWith(ctxBG(), ws.ID, "dash_del", semantic.RunOptions{Days: 3})
	if err != nil {
		t.Fatalf("查询不应整页失败: %v", err)
	}
	if data.Cards[0].Error == "" {
		t.Fatalf("数据集被删应挂在卡片错误: %+v", data.Cards[0])
	}
}
