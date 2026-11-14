package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/obs/logstore"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/testsupport"
)

var timeZero = time.Unix(0, 0)

func alertRuleSample() alert.Rule {
	return alert.Rule{Name: "r", Kind: alert.KindFailure, Target: "*", Severity: "warning", Channels: []string{}}
}

func alertEventSample() alert.Event {
	return alert.Event{RuleID: "alr_1", Title: "t"}
}

// 连接池不可用时，所有读写都必须把错误抛给上层。
// 这里最容易写错的是「查询失败当成空结果」——页面会显示「没有数据」，
// 而真实原因是库挂了。把这些返回值逐个钉住。
func TestQueriesFailWhenPoolClosed(t *testing.T) {
	db := testsupport.DB(t)
	testsupport.Reset(t, db)
	r := repo.New(db)
	ctx := context.Background()
	db.Close()

	const ws = "ws_x"
	cases := []struct {
		name string
		call func() error
	}{
		{"EnsureWorkspace", func() error { _, err := r.EnsureWorkspace(ctx, "w"); return err }},
		{"CreateUser", func() error {
			return r.CreateUser(ctx, model.User{ID: "u", WorkspaceID: ws, Email: "e", PasswordHash: "h"})
		}},
		{"UserByEmail", func() error { _, err := r.UserByEmail(ctx, ws, "e"); return err }},
		{"CreateDatasource", func() error {
			_, err := r.CreateDatasource(ctx, model.Datasource{WorkspaceID: ws, Name: "n", Type: "t"}, nil)
			return err
		}},
		{"ListDatasources", func() error { _, err := r.ListDatasources(ctx, ws); return err }},
		{"DatasourceByName", func() error { _, _, err := r.DatasourceByName(ctx, ws, "n"); return err }},
		{"UpdateDatasourceStatus", func() error { return r.UpdateDatasourceStatus(ctx, "ds", "ok", "") }},
		{"DeleteDatasource", func() error { return r.DeleteDatasource(ctx, ws, "n") }},
		{"UpsertDataset", func() error { return r.UpsertDataset(ctx, model.Dataset{Name: "t"}, ws) }},
		{"ListDatasets", func() error { _, err := r.ListDatasets(ctx, ws, ""); return err }},
		{"DatasetColumnsByName", func() error { _, err := r.DatasetColumnsByName(ctx, ws, "t"); return err }},
		{"UpdateDatasetPII", func() error { return r.UpdateDatasetPII(ctx, ws, "t", "high", "") }},
		{"ListPIIOverrides", func() error { _, err := r.ListPIIOverrides(ctx, ws, ""); return err }},
		{"SaveModel", func() error {
			_, err := r.SaveModel(ctx, ws, model.ModelSpec{Name: "m", Layer: "ods"}, "", "", "t", false)
			return err
		}},
		{"ListModels", func() error { _, err := r.ListModels(ctx, ws); return err }},
		{"PublishedModelSpec", func() error { _, err := r.PublishedModelSpec(ctx, ws, "m"); return err }},
		{"ModelVersions", func() error { _, err := r.ModelVersions(ctx, ws, "m"); return err }},
		{"RollbackModel", func() error { return r.RollbackModel(ctx, ws, "m", 1) }},
		{"SavePipeline", func() error {
			_, err := r.SavePipeline(ctx, ws, model.PipelineSpec{Name: "p"}, "t", false)
			return err
		}},
		{"ListPipelines", func() error { _, err := r.ListPipelines(ctx, ws); return err }},
		{"PipelineByName", func() error { _, _, _, err := r.PipelineByName(ctx, ws, "p"); return err }},
		{"PipelineVersionSpec", func() error { _, err := r.PipelineVersionSpec(ctx, "pl", 1); return err }},
		{"SetPipelineEnabled", func() error { return r.SetPipelineEnabled(ctx, ws, "p", true) }},
		{"ListEnabledPipelines", func() error { _, err := r.ListEnabledPipelines(ctx); return err }},
		{"SaveMetric", func() error { _, err := r.SaveMetric(ctx, ws, semantic.Metric{Name: "m"}); return err }},
		{"ListMetrics", func() error { _, err := r.ListMetrics(ctx, ws); return err }},
		{"MetricByName", func() error { _, err := r.MetricByName(ctx, ws, "m"); return err }},
		{"SaveDashboard", func() error { _, err := r.SaveDashboard(ctx, ws, semantic.Dashboard{Name: "d"}); return err }},
		{"ListDashboards", func() error { _, err := r.ListDashboards(ctx, ws); return err }},
		{"DashboardByName", func() error { _, err := r.DashboardByName(ctx, ws, "d"); return err }},
		{"ListLineage", func() error { _, err := r.ListLineage(ctx, ws); return err }},
		{"EnsureEnvironment", func() error { return r.EnsureEnvironment(ctx, ws, "prod", "") }},
		{"ListEnvironments", func() error { _, err := r.ListEnvironments(ctx, ws); return err }},
		{"BindEnvDatasource", func() error { return r.BindEnvDatasource(ctx, ws, "prod", "a", "ds") }},
		{"EnvBinding", func() error { _, err := r.EnvBinding(ctx, ws, "prod", "a"); return err }},
		{"ListEnvBindings", func() error { _, err := r.ListEnvBindings(ctx, ws, "prod"); return err }},
		{"DatasourceNameByID", func() error { _, err := r.DatasourceNameByID(ctx, ws, "ds"); return err }},
		{"RunningInstances", func() error { _, err := r.RunningInstances(ctx, ws); return err }},
		{"SoftDeleteWorkspace", func() error { return r.SoftDeleteWorkspace(ctx, ws) }},
		{"SaveSchemaChange", func() error {
			return r.SaveSchemaChange(ctx, ws, "ds", model.SchemaChange{Table: "t", Kind: "add_table"})
		}},
		{"ListSchemaChanges", func() error { _, err := r.ListSchemaChanges(ctx, ws, 10); return err }},
		{"RecordCollectJob", func() error { return r.RecordCollectJob(ctx, ws, "ds", "success", 1, 0, "") }},
		{"CreateInstance", func() error {
			_, _, err := r.CreateInstance(ctx, model.Instance{WorkspaceID: ws, PipelineID: "pl", BizDate: "2024-01-01"})
			return err
		}},
		{"ClaimInstance", func() error { _, err := r.ClaimInstance(ctx, "w", 0); return err }},
		{"Heartbeat", func() error { return r.Heartbeat(ctx, "ins", "w", 0) }},
		{"FinishInstance", func() error { return r.FinishInstance(ctx, "ins", "success", "") }},
		{"AddTasks", func() error { return r.AddTasks(ctx, "ins", []model.NodeSpec{{ID: "n", Type: "sql"}}) }},
		{"StartTask", func() error { return r.StartTask(ctx, "ins", "n", 1) }},
		{"FinishTask", func() error { return r.FinishTask(ctx, "ins", "n", "success", 0, 0, "") }},
		{"SkipDownstream", func() error { return r.SkipDownstream(ctx, "ins", []string{"n"}, "x") }},
		{"GetInstance", func() error { _, err := r.GetInstance(ctx, "ins"); return err }},
		{"Tasks", func() error { _, err := r.Tasks(ctx, "ins"); return err }},
		{"ListInstancesPage", func() error { _, _, err := r.ListInstancesPage(ctx, ws, model.InstanceQuery{}); return err }},
		{"ResetTasks", func() error { return r.ResetTasks(ctx, "ins", []string{"n"}) }},
		{"RequeueInstance", func() error { return r.RequeueInstance(ctx, "ins", "x") }},
		{"ListInstances", func() error { _, err := r.ListInstances(ctx, ws, 1); return err }},
		{"PreviousInstance", func() error { _, err := r.PreviousInstance(ctx, ws, "pl", "ins", timeZero); return err }},
		{"RequeueStale", func() error { _, err := r.RequeueStale(ctx, 0); return err }},
		{"ReplaceLineage", func() error { return r.ReplaceLineage(ctx, ws, "d", []string{"u"}) }},
		{"AddLineage", func() error { return r.AddLineage(ctx, ws, "d", []string{"u"}) }},
		{"LineageGraph", func() error { _, err := r.LineageGraph(ctx, ws); return err }},
		{"Audit", func() error { return r.Audit(ctx, model.AuditLog{WorkspaceID: ws, Action: "x"}) }},
		{"ListAudit", func() error { _, err := r.ListAudit(ctx, ws, 10); return err }},
		{"QualityOverview", func() error { _, err := r.QualityOverview(ctx, ws, 10); return err }},
		{"QualityTrend", func() error { _, err := r.QualityTrend(ctx, ws, 7); return err }},
		{"RunningCount", func() error { _, err := r.RunningCount(ctx, ws, "g"); return err }},
		{"Promote", func() error { _, err := r.Promote(ctx, ws, "g", 1); return err }},
		{"PendingInstances", func() error { _, err := r.PendingInstances(ctx, ws, 10); return err }},
		{"SetInstanceResources", func() error { return r.SetInstanceResources(ctx, "ins", "g", 1) }},
		{"RecordInstanceUsage", func() error { return r.RecordInstanceUsage(ctx, "ins", 1, 1) }},
		{"SetInstanceStatus", func() error { return r.SetInstanceStatus(ctx, "ins", "queued") }},
		{"PreviousInstanceRows", func() error { _, err := r.PreviousInstanceRows(ctx, ws, "pl", "2024-01-01"); return err }},
		{"CancelQueuedInstance", func() error { _, err := r.CancelQueuedInstance(ctx, "ins"); return err }},
		{"InstanceMatrix", func() error { _, err := r.InstanceMatrix(ctx, ws, "", 10); return err }},
		{"SaveAlertRule", func() error {
			_, err := r.SaveAlertRule(ctx, ws, alertRuleSample())
			return err
		}},
		{"ListAlertRules", func() error { _, err := r.ListAlertRules(ctx, ws); return err }},
		{"DeleteAlertRule", func() error { return r.DeleteAlertRule(ctx, ws, "r") }},
		{"EmitAlert", func() error { _, _, err := r.EmitAlert(ctx, ws, alertEventSample()); return err }},
		{"ListAlerts", func() error { _, err := r.ListAlerts(ctx, ws, "", 10); return err }},
		{"HandleAlert", func() error { return r.HandleAlert(ctx, ws, "ale", "resolved", "a") }},
		{"MarkAlertNotified", func() error { return r.MarkAlertNotified(ctx, "ale") }},
		{"ListResourceGroups", func() error { _, err := r.ListResourceGroups(ctx, ws); return err }},
		{"UpsertResourceGroup", func() error { return r.UpsertResourceGroup(ctx, ws, "g", 1, 1, 1, nil) }},
		{"WriteBatch", func() error {
			return repo.LogSink{DB: db}.WriteBatch(ctx, "ins", []logstore.Line{{Seq: 1, Message: "m"}})
		}},
		{"RecordQualityWaiver", func() error { return r.RecordQualityWaiver(ctx, ws, "ins", "n", "r", "a") }},
		{"ListQualityWaivers", func() error { _, err := r.ListQualityWaivers(ctx, ws, "ins"); return err }},
		{"ReplaceColumnLineage", func() error {
			return r.ReplaceColumnLineage(ctx, ws, "d", []model.ColumnEdge{{UpstreamTable: "u", UpstreamColumn: "c", DownstreamColumn: "c"}})
		}},
		{"ColumnLineage", func() error { _, err := r.ColumnLineage(ctx, ws, "t", ""); return err }},
		{"ImpactAnalysis", func() error { _, err := r.ImpactAnalysis(ctx, ws, "t", "", 3); return err }},
		{"SaveColumnPolicy", func() error { return r.SaveColumnPolicy(ctx, ws, model.ColumnPolicy{TableName: "t", ColumnName: "c"}) }},
		{"ListColumnPolicies", func() error { _, err := r.ListColumnPolicies(ctx, ws, ""); return err }},
		{"SaveRowPolicy", func() error {
			_, err := r.SaveRowPolicy(ctx, ws, model.RowPolicy{TableName: "t", FilterExpr: "1=1"})
			return err
		}},
		{"ListRowPolicies", func() error { _, err := r.ListRowPolicies(ctx, ws); return err }},
		{"RowFiltersFor", func() error { _, err := r.RowFiltersFor(ctx, ws, "t", "r"); return err }},
		{"DeleteRowPolicy", func() error { return r.DeleteRowPolicy(ctx, ws, "rpl") }},
		{"CacheGet", func() error { _, _, err := r.CacheGet(ctx, "k"); return err }},
		{"CachePut", func() error { return r.CachePut(ctx, "k", ws, []byte(`1`), 0) }},
		{"CachePurge", func() error { _, err := r.CachePurge(ctx); return err }},
		{"CacheStats", func() error { _, err := r.CacheStats(ctx, ws); return err }},
		{"CachePurgeByWorkspace", func() error { _, err := r.CachePurgeByWorkspace(ctx, ws); return err }},
		{"CreateExport", func() error { _, err := r.CreateExport(ctx, model.ExportJob{WorkspaceID: ws}); return err }},
		{"SetExportStatus", func() error { return r.SetExportStatus(ctx, "e", "running", "") }},
		{"FinishExport", func() error { return r.FinishExport(ctx, "e", "success", "", "", 0) }},
		{"GetExport", func() error { _, err := r.GetExport(ctx, ws, "e"); return err }},
		{"ListExports", func() error { _, err := r.ListExports(ctx, ws, 10); return err }},
		{"SaveSubscription", func() error {
			_, err := r.SaveSubscription(ctx, ws, model.Subscription{Name: "s", Target: "t"})
			return err
		}},
		{"ListSubscriptions", func() error { _, err := r.ListSubscriptions(ctx, ws); return err }},
		{"MarkSubscriptionRun", func() error { return r.MarkSubscriptionRun(ctx, "sub", "success") }},
		{"DeleteSubscription", func() error { return r.DeleteSubscription(ctx, ws, "sub") }},
		{"ListEnabledSubscriptions", func() error { _, err := r.ListEnabledSubscriptions(ctx); return err }},
		{"CreateShare", func() error { _, err := r.CreateShare(ctx, ws, "dashboard", "t", "a", 0); return err }},
		{"GetShare", func() error { _, err := r.GetShare(ctx, "tok"); return err }},
		{"ListShares", func() error { _, err := r.ListShares(ctx, ws); return err }},
		{"DeleteShare", func() error { return r.DeleteShare(ctx, ws, "tok") }},
		{"ListMembers", func() error { _, err := r.ListMembers(ctx, ws); return err }},
		{"AddMember", func() error { return r.AddMember(ctx, ws, "u", "viewer") }},
		{"RemoveMember", func() error { return r.RemoveMember(ctx, ws, "u") }},
		{"ListWorkspacesFor", func() error { _, err := r.ListWorkspacesFor(ctx, "u", "admin"); return err }},
		{"WorkspaceByID", func() error { _, err := r.WorkspaceByID(ctx, ws); return err }},
		{"CreateWorkspace", func() error { _, err := r.CreateWorkspace(ctx, "n", ""); return err }},
		{"TenantUsage", func() error { _, err := r.TenantUsage(ctx, ws); return err }},
		{"AllColumnPolicies", func() error { _, err := r.AllColumnPolicies(ctx, ws); return err }},
		{"AllColumnEdges", func() error { _, err := r.AllColumnEdges(ctx, ws); return err }},
		{"SaveRetentionPolicy", func() error { return r.SaveRetentionPolicy(ctx, ws, model.RetentionPolicy{ModelName: "m"}) }},
		{"GetRetentionPolicy", func() error { _, err := r.GetRetentionPolicy(ctx, ws, "m"); return err }},
		{"ListRetentionPolicies", func() error { _, err := r.ListRetentionPolicies(ctx, ws); return err }},
		{"CreateErasureJob", func() error {
			return r.CreateErasureJob(ctx, model.ErasureJob{ID: "e", WorkspaceID: ws, ModelName: "m", Status: "pending"})
		}},
		{"SetErasureStatus", func() error { return r.SetErasureStatus(ctx, "e", "done", 0, "") }},
		{"ListErasureJobs", func() error { _, err := r.ListErasureJobs(ctx, ws, 10); return err }},
		{"TableClassification", func() error { _, err := r.TableClassification(ctx, ws, []string{"t"}); return err }},
		{"SaveBackfillBatch", func() error { return r.SaveBackfillBatch(ctx, ws, &model.BackfillBatch{ID: "b"}) }},
		{"GetBackfillBatch", func() error { _, err := r.GetBackfillBatch(ctx, ws, "b"); return err }},
		{"ListBackfillBatches", func() error { _, err := r.ListBackfillBatches(ctx, ws, 10); return err }},
		{"BackfillStatus", func() error { _, _, err := r.BackfillStatus(ctx, ws, "p", "2024-01-01", "2024-01-02"); return err }},
		{"BackfillGantt", func() error { _, err := r.BackfillGantt(ctx, ws, 10); return err }},
		{"ExistingBizDates", func() error { _, err := r.ExistingBizDates(ctx, ws, "p", "2024-01-01", "2024-01-02"); return err }},
		{"SaveDataset", func() error { _, err := r.SaveDataset(ctx, ws, semantic.Dataset{Name: "d"}); return err }},
		{"GetDataset", func() error { _, err := r.GetDataset(ctx, ws, "d"); return err }},
		{"ListDatasetsScoped", func() error { _, err := r.ListDatasetsScoped(ctx, ws); return err }},
		{"DeleteDataset", func() error { return r.DeleteDataset(ctx, ws, "d") }},
		{"SaveChart", func() error { _, err := r.SaveChart(ctx, ws, semantic.Chart{Name: "c"}); return err }},
		{"GetChart", func() error { _, err := r.GetChart(ctx, ws, "c"); return err }},
		{"ListCharts", func() error { _, err := r.ListCharts(ctx, ws); return err }},
		{"DeleteChart", func() error { return r.DeleteChart(ctx, ws, "c") }},
		{"CountChartsUsingDataset", func() error { _, err := r.CountChartsUsingDataset(ctx, ws, "d"); return err }},
		{"SearchMetadata", func() error { _, err := r.SearchMetadata(ctx, ws, repo.SearchFilter{Q: "x"}); return err }},
		{"SaveTag", func() error { _, err := r.SaveTag(ctx, ws, "t", "", ""); return err }},
		{"ListTags", func() error { _, err := r.ListTags(ctx, ws); return err }},
		{"DeleteTag", func() error { return r.DeleteTag(ctx, ws, "t") }},
		{"TagEntity", func() error { return r.TagEntity(ctx, ws, "model", "m", []string{"t"}) }},
		{"UntagEntity", func() error { return r.UntagEntity(ctx, ws, "model", "m", "t") }},
		{"EntityTags", func() error { _, err := r.EntityTags(ctx, ws, "model", "m"); return err }},
		{"EntitiesByTag", func() error { _, err := r.EntitiesByTag(ctx, ws, "t", 10); return err }},
		{"LatestNodeRuns", func() error { _, err := r.LatestNodeRuns(ctx, ws); return err }},
		{"LatestQualityResults", func() error { _, err := r.LatestQualityResults(ctx, ws); return err }},
		{"ModelVersionCounts", func() error { _, err := r.ModelVersionCounts(ctx, ws); return err }},
	}

	for _, c := range cases {
		if err := c.call(); err == nil {
			t.Errorf("%s: 连接池已关闭，应返回错误而不是空结果", c.name)
		}
	}
}
