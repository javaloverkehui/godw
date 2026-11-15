package http

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/godw/godw/internal/auth"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/obs"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/scheduler"
	"github.com/godw/godw/internal/semantic"
	"gopkg.in/yaml.v3"
)

// Handler 组装全部 HTTP 能力。路由表集中在 Routes()，便于一眼看清接口面。
type Handler struct {
	Svc                *service.Service
	Cfg                config.Config
	Log                *slog.Logger
	DefaultWorkspaceID string
	Router             *Router
	ui                 embed.FS
}

func NewHandler(svc *service.Service, cfg config.Config, lg *slog.Logger, wsID string) *Handler {
	// 5xx 的根因只进日志，不进响应体。
	errs.Hook = func(err error) { lg.Error("请求处理失败", "err", err) }
	h := &Handler{Svc: svc, Cfg: cfg, Log: lg, DefaultWorkspaceID: wsID, ui: uiFS}
	h.Router = h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Router.ServeHTTP(w, r)
}

func (h *Handler) routes() *Router {
	r := NewRouter()

	r.GET("/healthz", func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
		ok(w, req, map[string]any{"status": "ok", "time": time.Now()})
	})
	r.GET("/readyz", h.readyz)

	// /metrics 不鉴权：Prometheus 抓取端通常没有登录态。
	// 生产部署应通过网关或网络策略限制来源。
	r.GET("/metrics", func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
		obs.Default.Handler().ServeHTTP(w, req)
	})

	r.POST("/api/v1/auth/login", h.login)
	r.GET("/api/v1/me", h.me)
	r.GET("/api/v1/overview", h.overview)

	r.GET("/api/v1/datasource-types", h.datasourceTypes)
	r.GET("/api/v1/datasources", h.listDatasources)
	r.POST("/api/v1/datasources", h.createDatasource)
	r.DELETE("/api/v1/datasources/{name}", h.deleteDatasource)
	r.POST("/api/v1/datasources/{name}/test", h.testDatasource)
	r.POST("/api/v1/datasources/{name}/collect", h.collectDatasource)
	r.GET("/api/v1/datasets", h.listDatasets)

	r.GET("/api/v1/models", h.listModels)
	r.POST("/api/v1/models", h.applyModel)
	r.POST("/api/v1/models/preview", h.previewModel)
	r.POST("/api/v1/models/check", h.checkModel)
	r.POST("/api/v1/models/scaffold", h.scaffoldModel)
	r.POST("/api/v1/models/diff", h.modelDiff)
	r.GET("/api/v1/models/{name}/rollback-preview", h.rollbackPreview)
	r.GET("/api/v1/models/{name}/versions", h.modelVersions)
	r.POST("/api/v1/models/{name}/rollback", h.rollbackModel)
	// refs 与 DELETE 都按 {name} 匹配，注册在 /models/preview、/models/check
	// 之后：那几条是固定路径，靠前缀顺序命中，不能被 {name} 抢走。
	r.GET("/api/v1/models/{name}/refs", h.modelRefs)
	r.DELETE("/api/v1/models/{name}", h.deleteModel)

	r.GET("/api/v1/pipelines", h.listPipelines)
	r.POST("/api/v1/pipelines", h.applyPipeline)
	r.POST("/api/v1/pipelines/dry-run", h.dryRunPipeline)
	r.POST("/api/v1/pipelines/sample-run", h.sampleRunPipeline)
	r.POST("/api/v1/pipelines/{name}/trigger", h.triggerPipeline)
	r.PUT("/api/v1/pipelines/{name}/enabled", h.togglePipeline)
	// refs 先给出「删除会带走什么」，前端拿它填确认框；DELETE 再校验一次，
	// 不能指望调用方先查过——接口是公开的，直接 DELETE 也得被拦住。
	r.GET("/api/v1/pipelines/{name}/refs", h.pipelineRefs)
	r.GET("/api/v1/pipelines/{name}/yaml", h.pipelineYAML)
	r.DELETE("/api/v1/pipelines/{name}", h.deletePipeline)
	// 选择器：按模型 / tag / 分层触发，一个表达式命中多条管道。
	r.GET("/api/v1/select/preview", h.selectPreview)
	r.POST("/api/v1/runs/select", h.selectRun)

	r.GET("/api/v1/instances", h.listInstances)
	// 批量动作必须注册在 /instances/{id} 之前：两者都是 2 段路径，
	// 路由是前缀顺序匹配，靠后的 {id} 会把 "batch" 当成实例 ID 吞掉。
	r.POST("/api/v1/instances/batch", h.batchInstances)
	// 同理：matrix 也必须在 {id} 之前，否则会被当成实例 ID。
	r.GET("/api/v1/instances/matrix", h.instanceMatrix)
	r.GET("/api/v1/instances/{id}", h.getInstance)
	r.GET("/api/v1/instances/{id}/events", h.instanceEvents)
	r.GET("/api/v1/instances/{id}/sql", h.instanceNodeSQL)
	r.POST("/api/v1/instances/{id}/retry", h.retryInstance)

	r.GET("/api/v1/metrics", h.listMetrics)
	r.POST("/api/v1/metrics", h.applyMetric)
	// 指标向导要在「保存前」看到曲线，否则用户得先落一个写错的口径再回头改。
	r.POST("/api/v1/metrics/preview", h.previewMetric)
	r.GET("/api/v1/metrics/{name}/query", h.queryMetric)
	// 指标是看板卡片的引用目标，删除前要告诉用户哪几块看板会空掉。
	r.GET("/api/v1/metrics/{name}/refs", h.metricRefs)
	r.DELETE("/api/v1/metrics/{name}", h.deleteMetric)

	r.GET("/api/v1/datasets/semantic", h.listDatasetsSemantic)
	r.GET("/api/v1/datasets/semantic/{name}", h.getDataset)
	r.POST("/api/v1/datasets/semantic", h.applyDataset)
	r.DELETE("/api/v1/datasets/semantic/{name}", h.deleteDataset)
	r.POST("/api/v1/datasets/semantic/query", h.datasetQuery)
	r.POST("/api/v1/datasets/semantic/{name}/sql", h.datasetSQL)

	r.GET("/api/v1/charts", h.listCharts)
	r.POST("/api/v1/charts", h.applyChart)
	r.GET("/api/v1/charts/types", h.chartTypes)
	r.GET("/api/v1/charts/{name}/query", h.queryChart)
	r.DELETE("/api/v1/charts/{name}", h.deleteChart)

	r.GET("/api/v1/dashboards", h.listDashboards)
	r.POST("/api/v1/dashboards", h.applyDashboard)
	r.GET("/api/v1/dashboards/{name}", h.queryDashboard)
	r.DELETE("/api/v1/dashboards/{name}", h.deleteDashboard)

	// 自助查询：只读 + 单语句 + 超时 + 行数上限，仅 admin/editor。
	r.POST("/api/v1/sql/query", h.rawSQLQuery)

	r.GET("/api/v1/quality", h.listQuality)
	r.GET("/api/v1/quality/overview", h.qualityOverview)
	r.GET("/api/v1/quality/trend", h.qualityTrend)
	r.POST("/api/v1/quality/profile", h.profileTable)
	// 规则是模型上的对象，管道执行时按模型名取用；此前的内联写法仍然兼容。
	r.GET("/api/v1/quality-rules", h.listQualityRules)
	r.PUT("/api/v1/quality-rules", h.saveQualityRules)
	r.GET("/api/v1/lineage", h.lineage)
	r.GET("/api/v1/lineage/graph", h.lineageGraph)
	r.GET("/api/v1/lineage/focus", h.lineageFocus)
	r.GET("/api/v1/lineage/paths", h.lineagePaths)
	r.GET("/api/v1/lineage/dot", h.lineageDOT)
	r.GET("/api/v1/lineage/cache", h.lineageCache)
	r.POST("/api/v1/instances/{id}/waive", h.waiveQuality)
	r.GET("/api/v1/instances/{id}/waivers", h.qualityWaivers)
	r.GET("/api/v1/lineage/columns", h.columnLineage)
	r.GET("/api/v1/lineage/column-graph", h.columnGraph)
	r.GET("/api/v1/lineage/impact", h.impact)

	r.GET("/api/v1/table/rows", h.previewTableRows)

	r.GET("/api/v1/policies/columns", h.listColumnPolicies)
	r.POST("/api/v1/policies/columns", h.saveColumnPolicy)
	r.GET("/api/v1/policies/rows", h.listRowPolicies)
	r.POST("/api/v1/policies/rows", h.saveRowPolicy)
	r.DELETE("/api/v1/policies/rows/{id}", h.deleteRowPolicy)
	r.GET("/api/v1/policies/simulate", h.simulatePolicies)

	r.GET("/api/v1/exports", h.listExports)
	r.POST("/api/v1/exports", h.createExport)
	r.POST("/api/v1/exports/{id}/approve", h.approveExport)
	r.POST("/api/v1/exports/{id}/reject", h.rejectExport)
	r.GET("/api/v1/exports/{id}", h.getExport)
	r.GET("/api/v1/exports/{id}/download", h.downloadExport)

	r.GET("/api/v1/subscriptions", h.listSubscriptions)
	r.POST("/api/v1/subscriptions", h.saveSubscription)
	r.POST("/api/v1/subscriptions/{id}/run", h.runSubscription)
	r.DELETE("/api/v1/subscriptions/{id}", h.deleteSubscription)

	r.GET("/api/v1/shares", h.listShares)
	r.POST("/api/v1/shares", h.createShare)
	r.DELETE("/api/v1/shares/{token}", h.deleteShare)

	r.GET("/api/v1/workspaces", h.listWorkspaces)
	r.POST("/api/v1/workspaces", h.createWorkspace)
	r.POST("/api/v1/workspaces/{id}/switch", h.switchWorkspace)
	r.GET("/api/v1/usage", h.tenantUsage)

	r.GET("/api/v1/members", h.listMembers)
	r.POST("/api/v1/members", h.addMember)
	r.DELETE("/api/v1/members/{userId}", h.removeMember)

	r.GET("/api/v1/cache/stats", h.cacheStats)
	r.POST("/api/v1/cache/purge", h.cachePurge)

	r.GET("/api/v1/public/share/{token}", h.publicShare)
	r.GET("/api/v1/dictionary", h.dictionary)
	r.GET("/api/v1/dictionary/export", h.dictionaryExport)
	// 数据文档站：按模型聚合字段/质量/血缘/运行，另可导出单文件 HTML。
	r.GET("/api/v1/catalog", h.catalogDocs)
	r.GET("/api/v1/catalog/export", h.catalogExport)
	r.GET("/api/v1/layer-rules", h.layerRules)
	// 规范存在 workspace.settings 里，改它影响所有模型提交，仅 admin。
	r.PUT("/api/v1/layer-rules", h.saveLayerRules)
	r.DELETE("/api/v1/layer-rules", h.resetLayerRules)

	r.GET("/api/v1/retention", h.listRetention)
	r.POST("/api/v1/retention", h.saveRetention)
	r.GET("/api/v1/retention/plan", h.retentionPlan)
	r.POST("/api/v1/retention/run", h.retentionRun)

	r.GET("/api/v1/erasure-jobs", h.listErasureJobs)
	r.POST("/api/v1/erasure-jobs", h.createErasureJob)

	r.GET("/api/v1/audit", h.listAudit)
	r.GET("/api/v1/schedule/preview", h.previewSchedule)
	r.POST("/api/v1/worker/run-once", h.runOnce)
	r.GET("/api/v1/queue", h.queueView)
	r.POST("/api/v1/queue/dispatch", h.dispatchOnce)

	r.GET("/api/v1/resource-groups", h.listResourceGroups)
	r.POST("/api/v1/resource-groups", h.saveResourceGroup)

	r.GET("/api/v1/alert-rules", h.listAlertRules)
	r.POST("/api/v1/alert-rules", h.saveAlertRule)
	r.DELETE("/api/v1/alert-rules/{name}", h.deleteAlertRule)
	r.POST("/api/v1/alert-rules/test", h.testAlertRule)
	r.GET("/api/v1/alerts", h.listAlerts)
	r.POST("/api/v1/alerts/{id}/handle", h.handleAlert)

	// 补数（T0906）
	r.POST("/api/v1/instances/{id}/cancel", h.cancelInstance)
	r.GET("/api/v1/backfill/plan", h.backfillPlan)
	r.POST("/api/v1/backfill", h.createBackfill)
	r.GET("/api/v1/backfill", h.listBackfills)
	r.GET("/api/v1/backfill/{id}", h.backfillProgress)
	// 注意：不能叫 /instances/gantt —— 会被更早注册的 /instances/{id} 抢走。
	r.GET("/api/v1/runbook/gantt", h.ganttData)

	// 环境绑定、采集变更、敏感识别（T0203 / T0208 / T0209）
	r.GET("/api/v1/environments", h.listEnvironments)
	r.POST("/api/v1/environments/bind", h.bindEnvironment)
	r.GET("/api/v1/environments/bindings", h.envBindings)
	r.POST("/api/v1/catalog/collect", h.collectWithDiff)
	r.GET("/api/v1/schema-changes", h.listSchemaChanges)
	r.POST("/api/v1/pii/detect", h.detectPII)
	r.POST("/api/v1/pii/apply", h.applyPII)

	// 元数据搜索与标签（T0210）
	r.GET("/api/v1/search", h.search)
	r.GET("/api/v1/tags", h.listTags)
	r.POST("/api/v1/tags", h.saveTag)
	r.DELETE("/api/v1/tags/{name}", h.deleteTag)
	r.GET("/api/v1/tags/{name}/entities", h.entitiesByTag)
	r.POST("/api/v1/tags/attach", h.tagEntity)
	r.GET("/api/v1/entity/tags", h.entityTags)
	r.DELETE("/api/v1/tags/{name}/entities/{type}/{entity}", h.untagEntity)

	r.GET("/api/v1/instances/{id}/logs", h.instanceLogsSnapshot)
	r.GET("/api/v1/instances/{id}/logs/stream", h.instanceLogs)

	// 内嵌控制台：不依赖 npm 构建，二进制自带可用界面。
	sub, err := fs.Sub(h.ui, "ui")
	if err == nil {
		fileServer := http.FileServer(http.FS(sub))
		r.GET("/", func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
			req.URL.Path = "/"
			fileServer.ServeHTTP(w, req)
		})
		r.GET("/assets/{file}", func(w http.ResponseWriter, req *http.Request, p map[string]string) {
			req.URL.Path = "/assets/" + p["file"]
			fileServer.ServeHTTP(w, req)
		})
	}
	return r
}

// ---------- auth ----------

func (h *Handler) login(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	u, err := h.Svc.Repo.UserByEmail(r.Context(), h.DefaultWorkspaceID, body.Email)
	if err != nil {
		fail(w, r, errs.New(errs.CodeUnauthorized, "邮箱或密码错误"))
		return
	}
	if !checkPassword(u.PasswordHash, body.Password) {
		fail(w, r, errs.New(errs.CodeUnauthorized, "邮箱或密码错误"))
		return
	}
	token, err := h.Svc.Signer.Sign(u.ID, u.WorkspaceID, u.Role)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"token": token, "user": u})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ok(w, r, map[string]any{
		"userId": actorOf(r.Context()), "workspaceId": workspaceOf(r.Context()),
		"role": roleOf(r.Context()),
		// env 给前端画环境条：prod 标红、staging 标橙。
		// 用户误在 prod 上点「全量重跑」的代价太高，必须一眼可辨。
		"env": h.Cfg.Env,
	})
}

// ---------- health ----------

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.Svc.Repo.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, Response{Code: int(errs.CodeUnavailable), Message: "数据库不可达"})
		return
	}
	version, n, err := h.Svc.Repo.DB.Version(ctx)
	if err != nil || n == 0 {
		writeJSON(w, http.StatusServiceUnavailable, Response{Code: int(errs.CodeUnavailable), Message: "schema 未初始化，请执行 godw migrate"})
		return
	}
	ok(w, r, map[string]any{"status": "ready", "schemaVersion": version, "migrations": n})
}

// ---------- overview ----------

func (h *Handler) overview(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ctx := r.Context()
	ws := workspaceOf(ctx)
	ds, _ := h.Svc.Repo.ListDatasources(ctx, ws)
	models, _ := h.Svc.Repo.ListModels(ctx, ws)
	pipes, _ := h.Svc.Repo.ListPipelines(ctx, ws)
	insts, _ := h.Svc.Repo.ListInstances(ctx, ws, 20)

	byStatus := map[string]int{}
	for _, i := range insts {
		byStatus[i.Status]++
	}
	datasets, _ := h.Svc.Repo.ListDatasets(ctx, ws, "")
	ok(w, r, map[string]any{
		"datasources": len(ds), "models": len(models), "pipelines": len(pipes),
		"datasets": len(datasets), "instances": insts, "byStatus": byStatus,
	})
}

// ---------- datasource ----------

func (h *Handler) datasourceTypes(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ok(w, r, connector.Registry())
}

func (h *Handler) listDatasources(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListDatasources(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	// 凭证永远不出网关：config 里只回显非敏感项。
	for i := range items {
		delete(items[i].Config, "password")
	}
	ok(w, r, items)
}

func (h *Handler) createDatasource(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Name   string            `json:"name"`
		Type   string            `json:"type"`
		Config map[string]any    `json:"config"`
		Secret map[string]string `json:"secret"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	ds, err := h.Svc.CreateDatasource(r.Context(), workspaceOf(r.Context()), body.Name, body.Type, body.Config, body.Secret)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "datasource.create", body.Name, map[string]any{"type": body.Type})
	created(w, r, ds)
}

func (h *Handler) deleteDatasource(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := paramsFrom(r.Context())["name"]
	if err := h.Svc.Repo.DeleteDatasource(r.Context(), workspaceOf(r.Context()), name); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "datasource.delete", name, nil)
	ok(w, r, map[string]any{"deleted": name})
}

func (h *Handler) testDatasource(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	name := paramsFrom(r.Context())["name"]
	if err := h.Svc.TestDatasource(r.Context(), workspaceOf(r.Context()), name); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"status": "ok"})
}

func (h *Handler) collectDatasource(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := paramsFrom(r.Context())["name"]
	n, err := h.Svc.CollectMetadata(r.Context(), workspaceOf(r.Context()), name)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "datasource.collect", name, map[string]any{"tables": n})
	ok(w, r, map[string]any{"tables": n})
}

func (h *Handler) listDatasets(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListDatasets(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("datasourceId"))
	if err != nil {
		fail(w, r, err)
		return
	}
	page := paginate(items, queryInt(r, "page", 1), queryInt(r, "pageSize", 200))
	ok(w, r, page)
}

// ---------- model ----------

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListModels(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) applyModel(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		YAML string `json:"yaml"`
		// Spec 与 YAML 二选一。图形化向导走 Spec，避免前端手拼 YAML 字符串——
		// 那里的引号/换行转义一旦出错，报错会指向「第 N 行」而不是「哪个选择错了」。
		Spec    *model.ModelSpec `json:"spec"`
		Publish bool             `json:"publish"`
		Apply   bool             `json:"apply"`
		Force   bool             `json:"force"`
		Reason  string           `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	raw := []byte(body.YAML)
	if body.Spec != nil {
		b, merr := yaml.Marshal(body.Spec)
		if merr != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, merr, "模型定义序列化失败"))
			return
		}
		raw = b
	}
	m, err := h.Svc.ApplyModel(r.Context(), workspaceOf(r.Context()), actorOf(r.Context()), raw, body.Publish, body.Apply, body.Force)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "model.apply", m.Name, map[string]any{"version": m.ActiveVersion, "publish": body.Publish, "force": body.Force, "reason": body.Reason})
	created(w, r, m)
}

func (h *Handler) previewModel(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	// 既接受 YAML 也接受结构化 spec：设计器每改一个字段就重编 DDL，
	// 每次都序列化 YAML 再解析回来是纯浪费，直接传对象更直接。
	var body struct {
		YAML string           `json:"yaml"`
		Spec *model.ModelSpec `json:"spec"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	spec, err := h.modelSpecFrom(body.YAML, body.Spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	ddl, err := h.Svc.PreviewModelDDL(r.Context(), workspaceOf(r.Context()), spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"ddl": ddl})
}

func (h *Handler) modelVersions(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	name := paramsFrom(r.Context())["name"]
	items, err := h.Svc.Repo.ModelVersions(r.Context(), workspaceOf(r.Context()), name)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) rollbackModel(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := paramsFrom(r.Context())["name"]
	var body struct {
		Version int `json:"version"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if err := h.Svc.Repo.RollbackModel(r.Context(), workspaceOf(r.Context()), name, body.Version); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "model.rollback", name, map[string]any{"version": body.Version})
	ok(w, r, map[string]any{"rolledBack": body.Version})
}

// modelRefs 返回按名字引用该模型的指标与管道。前端拿它填「删除前的影响面」，
// 不做删除也可以单独调用——排查「这个模型还有人用吗」同样需要。
func (h *Handler) modelRefs(w http.ResponseWriter, r *http.Request, p map[string]string) {
	refs, err := h.Svc.Repo.ModelRefs(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, refs)
}

// deleteModel 删模型元数据。仍有指标/管道引用时默认拦下，只有显式 force=1 才放行：
// 解除引用要先改指标口径或管道定义，不该由一次误点顺带完成。
func (h *Handler) deleteModel(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := p["name"]
	ws := workspaceOf(r.Context())
	refs, err := h.Svc.Repo.ModelRefs(r.Context(), ws, name)
	if err != nil {
		fail(w, r, err)
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if !force && (len(refs.Metrics) > 0 || len(refs.Pipelines) > 0) {
		fail(w, r, errs.New(errs.CodeConflict, "模型仍被指标或管道引用，确认影响后加 force=1 重试").
			WithMeta("refs", refs))
		return
	}
	if err := h.Svc.Repo.DeleteModel(r.Context(), ws, name); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "model.delete", name, map[string]any{
		"force": force, "metrics": refs.Metrics, "pipelines": refs.Pipelines,
	})
	ok(w, r, map[string]any{"deleted": name, "refs": refs})
}

// ---------- pipeline ----------

func (h *Handler) listPipelines(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListPipelines(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) applyPipeline(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		YAML string `json:"yaml"`
		// Spec 与 YAML 二选一。管道向导用 Spec：节点、依赖、调度都是表单里选的，
		// 让前端把结构体再拼回 YAML 只是多了一处可能拼错的地方。
		Spec    *model.PipelineSpec `json:"spec"`
		Publish bool                `json:"publish"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	raw := []byte(body.YAML)
	if body.Spec != nil {
		b, merr := yaml.Marshal(body.Spec)
		if merr != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, merr, "管道定义序列化失败"))
			return
		}
		raw = b
	}
	p, err := h.Svc.ApplyPipeline(r.Context(), workspaceOf(r.Context()), actorOf(r.Context()), raw, body.Publish)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "pipeline.apply", p.Name, map[string]any{"publish": body.Publish})
	created(w, r, p)
}

func (h *Handler) triggerPipeline(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := paramsFrom(r.Context())["name"]
	var body struct {
		BizDate string         `json:"bizDate"`
		Params  map[string]any `json:"params"`
	}
	_ = decodeBody(r, &body)
	inst, createdNew, err := h.Svc.Trigger(r.Context(), workspaceOf(r.Context()), name, body.BizDate, "manual", body.Params)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "pipeline.trigger", name, map[string]any{"instance": inst.ID, "created": createdNew})
	ok(w, r, map[string]any{"instance": inst, "created": createdNew})
}

func (h *Handler) togglePipeline(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := paramsFrom(r.Context())["name"]
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if err := h.Svc.Repo.SetPipelineEnabled(r.Context(), workspaceOf(r.Context()), name, body.Enabled); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "pipeline.toggle", name, map[string]any{"enabled": body.Enabled})
	ok(w, r, map[string]any{"enabled": body.Enabled})
}

// pipelineRefs 返回删除这条管道会连带删掉多少运行历史。
// 不做删除也能单独调用：排查「这条管道还在用吗」同样需要这个数字。
func (h *Handler) pipelineRefs(w http.ResponseWriter, r *http.Request, p map[string]string) {
	refs, err := h.Svc.Repo.PipelineRefs(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, refs)
}

// pipelineYAML 返回当前生效版本（没发布过就取最新草稿）的规范化 YAML。
//
// 前端「高级」区拿它当编辑起点：从 JSON 在浏览器里手写序列化会丢掉
// params / defaults / node.config 这些嵌套字段，改一次调度就把参数弄没了。
// 在服务端序列化，前端看到的与后端认的是同一份。
func (h *Handler) pipelineYAML(w http.ResponseWriter, r *http.Request, p map[string]string) {
	spec, version, err := h.Svc.Repo.PipelineSpecForEdit(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	b, err := yaml.Marshal(spec)
	if err != nil {
		fail(w, r, errs.Wrap(errs.CodeInternal, err, "管道定义序列化失败"))
		return
	}
	// spec 一起返回：草稿管道在列表接口里没有 spec（列表只读 active_version），
	// 编辑页要能画出节点表，就不能只给它一段 YAML。
	ok(w, r, map[string]any{"yaml": string(b), "version": version, "spec": spec})
}

// deletePipeline 删除管道及其版本与运行历史。
//
// 两道刹车：
//  1. 有排队/运行中的实例时一律拒绝——执行器正拿着这份版本快照，
//     删掉会让进程和一个不存在的管道对话。
//  2. 有历史实例时要求 force=1，把「历史一起没了」变成一次显式选择，
//     而不是误点「删除」的副作用。
//
// 只想停止排期请用 PUT /pipelines/{name}/enabled，别删。
func (h *Handler) deletePipeline(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := p["name"]
	ws := workspaceOf(r.Context())
	refs, err := h.Svc.Repo.PipelineRefs(r.Context(), ws, name)
	if err != nil {
		fail(w, r, err)
		return
	}
	if refs.Running > 0 {
		fail(w, r, errs.New(errs.CodeConflict,
			"管道还有排队或运行中的实例，等它们结束（或先取消）再删除").
			WithMeta("refs", refs))
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if !force && refs.Instances > 0 {
		fail(w, r, errs.New(errs.CodeConflict,
			"管道已有运行记录，删除会一并清除，确认影响后加 force=1 重试").
			WithMeta("refs", refs))
		return
	}
	if err := h.Svc.Repo.DeletePipeline(r.Context(), ws, name); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "pipeline.delete", name, map[string]any{"force": force, "instances": refs.Instances})
	ok(w, r, map[string]any{"deleted": name, "refs": refs})
}

// ---------- instance ----------

func (h *Handler) listInstances(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	items, total, err := h.Svc.Repo.ListInstancesPage(r.Context(), workspaceOf(r.Context()), model.InstanceQuery{
		Pipeline: q.Get("pipeline"),
		Status:   q.Get("status"),
		From:     q.Get("from"),
		To:       q.Get("to"),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	// total 与分页信息一起返回：前端要画「1-20 / 137」，只给 items 是画不出来的。
	ok(w, r, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset})
}

// instanceMatrix 返回「节点 × 最近 N 次运行」的状态矩阵，供运行实例页的矩阵视图使用。
func (h *Handler) instanceMatrix(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	m, err := h.Svc.Repo.InstanceMatrix(r.Context(), workspaceOf(r.Context()),
		r.URL.Query().Get("pipeline"), queryInt(r, "limit", 20))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, m)
}

func (h *Handler) getInstance(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	id := paramsFrom(r.Context())["id"]
	inst, err := h.Svc.Repo.GetInstance(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	q, _ := h.Svc.Repo.QualityResults(r.Context(), id)
	ok(w, r, map[string]any{"instance": inst, "quality": q})
}

// instanceNodeSQL 返回某个实例里某节点「当次实际执行」的渲染 SQL。
// 只读接口，viewer 也能看：排障时最常见的就是「我只有只读权限但要确认跑了什么」。
func (h *Handler) instanceNodeSQL(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	id := paramsFrom(r.Context())["id"]
	node := r.URL.Query().Get("node")
	res, err := h.Svc.InstanceNodeSQL(r.Context(), workspaceOf(r.Context()), id, node)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) retryInstance(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	id := paramsFrom(r.Context())["id"]
	var body struct {
		// Node 非空表示「从该节点重跑」：原地重置该节点与下游，不新建实例。
		Node string `json:"node"`
	}
	_ = decodeBody(r, &body)

	if body.Node != "" {
		inst, err := h.Svc.RetryNode(r.Context(), id, body.Node)
		if err != nil {
			fail(w, r, err)
			return
		}
		h.audit(r, "instance.retry_node", id, map[string]any{"node": body.Node})
		ok(w, r, map[string]any{"instance": inst, "node": body.Node})
		return
	}

	inst, err := h.Svc.Repo.GetInstance(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	// 重跑复用原版本快照与业务日期，不重新取当前定义。
	newInst, _, err := h.Svc.TriggerByPipelineID(r.Context(), workspaceOf(r.Context()), inst.PipelineID, inst.BizDate, "retry", inst.Params)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "instance.retry", id, map[string]any{"newInstance": newInst.ID})
	ok(w, r, newInst)
}

// batchInstances 批量重跑 / 取消：运维一次要处理十几条失败实例，逐条点太慢。
// 单个失败不影响其余：逐条收集结果，最后如实返回成功与失败的清单。
func (h *Handler) batchInstances(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Action string   `json:"action"` // retry | cancel
		IDs    []string `json:"ids"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if len(body.IDs) == 0 {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "至少选择一条实例"))
		return
	}
	if len(body.IDs) > 100 {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "单次最多处理 100 条"))
		return
	}

	ctx := r.Context()
	ws := workspaceOf(r.Context())
	var okIDs []string
	failed := map[string]string{}
	for _, id := range body.IDs {
		var err error
		switch body.Action {
		case "cancel":
			_, err = h.Svc.Repo.CancelQueuedInstance(ctx, id)
		case "retry":
			var inst *model.Instance
			inst, err = h.Svc.Repo.GetInstance(ctx, id)
			if err == nil {
				_, _, err = h.Svc.TriggerByPipelineID(ctx, ws, inst.PipelineID, inst.BizDate, "retry", inst.Params)
			}
		default:
			fail(w, r, errs.New(errs.CodeInvalidArgument, "不支持的动作: "+body.Action))
			return
		}
		if err != nil {
			failed[id] = err.Error()
			continue
		}
		okIDs = append(okIDs, id)
	}
	h.audit(r, "instance.batch_"+body.Action, "", map[string]any{
		"total": len(body.IDs), "succeeded": len(okIDs), "failed": len(failed),
	})
	ok(w, r, map[string]any{"succeeded": okIDs, "failed": failed})
}

// instanceEvents 用 SSE 推送节点状态，前端无需轮询。
func (h *Handler) instanceEvents(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	id := paramsFrom(r.Context())["id"]
	flusher, okc := w.(http.Flusher)
	if !okc {
		fail(w, r, errs.New(errs.CodeInternal, "当前连接不支持流式输出"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			inst, err := h.Svc.Repo.GetInstance(r.Context(), id)
			if err != nil {
				return
			}
			payload := fmt.Sprintf(`{"status":%q,"tasks":%d}`, inst.Status, len(inst.Tasks))
			if payload != last {
				last = payload
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload)
				flusher.Flush()
			}
			if inst.Status == "success" || inst.Status == "failed" || inst.Status == "canceled" {
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", payload)
				flusher.Flush()
				return
			}
		}
	}
}

// ---------- metric ----------

func (h *Handler) listMetrics(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListMetrics(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) applyMetric(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	// 与模型、管道、看板一致：结构化对象是主入口，YAML 是它的另一种写法。
	// 只收 YAML 会把「改个聚合方式」变成「编辑一段配置文件」，指标页因此没法给不会写 YAML 的人用。
	var body struct {
		YAML   string           `json:"yaml"`
		Metric *semantic.Metric `json:"metric"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	var m semantic.Metric
	switch {
	case body.Metric != nil:
		m = *body.Metric
	case body.YAML != "":
		if err := yamlUnmarshal([]byte(body.YAML), &m); err != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, err, "指标 YAML 解析失败"))
			return
		}
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "需要 metric 或 yaml 之一"))
		return
	}
	saved, err := h.Svc.ApplyMetric(r.Context(), workspaceOf(r.Context()), m)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "metric.apply", saved.Name, nil)
	created(w, r, saved)
}

// previewMetric 用未保存的指标定义直接跑一次聚合。语义校验与正式保存一致，
// 只是不落库——向导里每改一个口径就能看到曲线。
func (h *Handler) previewMetric(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Metric semantic.Metric `json:"metric"`
		Query  semantic.Query  `json:"query"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	series, err := h.Svc.PreviewMetric(r.Context(), workspaceOf(r.Context()), body.Metric, body.Query,
		actorOf(r.Context()), roleOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, series)
}

func (h *Handler) queryMetric(w http.ResponseWriter, r *http.Request, p map[string]string) {
	q := semantic.Query{
		From:        r.URL.Query().Get("from"),
		To:          r.URL.Query().Get("to"),
		Granularity: r.URL.Query().Get("granularity"),
		Dimension:   r.URL.Query().Get("dimension"),
		Limit:       queryInt(r, "limit", 500),
	}
	series, err := h.Svc.QueryMetricFor(r.Context(), workspaceOf(r.Context()), p["name"], q,
		actorOf(r.Context()), roleOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, series)
}

// metricRefs 返回引用了该指标的看板。看板卡片只存指标名，
// 指标删了卡片会变成一块报错的空壳，所以这个列表要摆给用户看。
func (h *Handler) metricRefs(w http.ResponseWriter, r *http.Request, p map[string]string) {
	dashboards, err := h.Svc.Repo.MetricRefs(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"dashboards": dashboards})
}

// deleteMetric 删除指标定义。仍被看板引用时默认拦下：
// 解除引用要么改卡片、要么删卡片，都不该由一次「删除指标」顺带完成。
func (h *Handler) deleteMetric(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := p["name"]
	ws := workspaceOf(r.Context())
	// 先确认对象存在，再看引用：不存在的指标去查引用面只会查到「某个看板碰巧写了这个名字」，
	// 报出来的是 409 而不是 404，排查时会误以为指标还在。
	if _, err := h.Svc.Repo.MetricByName(r.Context(), ws, name); err != nil {
		fail(w, r, err)
		return
	}
	dashboards, err := h.Svc.Repo.MetricRefs(r.Context(), ws, name)
	if err != nil {
		fail(w, r, err)
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if !force && len(dashboards) > 0 {
		fail(w, r, errs.New(errs.CodeConflict, "指标仍被看板引用，确认影响后加 force=1 重试").
			WithMeta("dashboards", dashboards))
		return
	}
	if err := h.Svc.Repo.DeleteMetric(r.Context(), ws, name); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "metric.delete", name, map[string]any{"force": force, "dashboards": dashboards})
	ok(w, r, map[string]any{"deleted": name, "dashboards": dashboards})
}

// ---------- dashboard ----------

func (h *Handler) listDashboards(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListDashboards(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) applyDashboard(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	// 两种提交形态：可视化编辑器给结构化对象，高级模式给 YAML。
	// YAML 是结构化对象的另一种写法，不该成为唯一入口。
	var body struct {
		YAML      string              `json:"yaml"`
		Dashboard *semantic.Dashboard `json:"dashboard"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	var d semantic.Dashboard
	switch {
	case body.Dashboard != nil:
		d = *body.Dashboard
	case body.YAML != "":
		if err := yamlUnmarshal([]byte(body.YAML), &d); err != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, err, "看板 YAML 解析失败"))
			return
		}
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "需要 dashboard 或 yaml 之一"))
		return
	}
	saved, err := h.Svc.ApplyDashboard(r.Context(), workspaceOf(r.Context()), d)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "dashboard.apply", saved.Name, nil)
	created(w, r, saved)
}

func (h *Handler) queryDashboard(w http.ResponseWriter, r *http.Request, p map[string]string) {
	// days / dim 是页面级全局筛选；不传就按看板定义原样渲染。
	// 带上访问者身份，图表卡的数据集查询才能按行列策略生效。
	data, err := h.Svc.QueryDashboardFor(r.Context(), workspaceOf(r.Context()), p["name"],
		actorOf(r.Context()), roleOf(r.Context()), semantic.RunOptions{
			Days: queryInt(r, "days", 0),
			Dim:  r.URL.Query().Get("dim"),
		})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, data)
}

// deleteDashboard 删掉一页排布。指标、图表与数据都不动——
// 「这块看板不要了」和「这些口径不要了」是两件事，别让一次删除同时成立。
func (h *Handler) deleteDashboard(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	name := p["name"]
	if err := h.Svc.Repo.DeleteDashboard(r.Context(), workspaceOf(r.Context()), name); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "dashboard.delete", name, nil)
	ok(w, r, map[string]any{"deleted": name})
}

// ---------- quality / lineage / audit ----------

func (h *Handler) listQuality(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	instID := r.URL.Query().Get("instanceId")
	if instID == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 instanceId"))
		return
	}
	items, err := h.Svc.Repo.QualityResults(r.Context(), instID)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) qualityOverview(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.QualityOverview(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 200))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) qualityTrend(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.QualityTrend(r.Context(), workspaceOf(r.Context()), queryInt(r, "days", 14))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) profileTable(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Datasource string `json:"datasource"`
		Schema     string `json:"schema"`
		Table      string `json:"table"`
		SampleRows int    `json:"sampleRows"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	res, err := h.Svc.Profile(r.Context(), workspaceOf(r.Context()), body.Datasource, body.Schema, body.Table, body.SampleRows)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) lineage(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	edges, err := h.Svc.Repo.LineageGraph(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	nodes := map[string]bool{}
	out := make([]map[string]string, 0, len(edges))
	for _, e := range edges {
		nodes[e[0]] = true
		nodes[e[1]] = true
		out = append(out, map[string]string{"source": e[0], "target": e[1]})
	}
	list := make([]string, 0, len(nodes))
	for n := range nodes {
		list = append(list, n)
	}
	ok(w, r, map[string]any{"nodes": list, "edges": out})
}

func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListAudit(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 100))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) previewSchedule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	expr := r.URL.Query().Get("cron")
	s, err := scheduler.Parse(expr)
	if err != nil {
		fail(w, r, errs.Wrap(errs.CodeInvalidArgument, err, "cron 解析失败"))
		return
	}
	var next []string
	t := time.Now()
	for i := 0; i < 5; i++ {
		nt, okNext := s.Next(t)
		if !okNext {
			break
		}
		next = append(next, nt.Format("2006-01-02 15:04"))
		t = nt
	}
	ok(w, r, map[string]any{"cron": s.String(), "next": next})
}

// ---------- worker ----------

func (h *Handler) runOnce(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	worker := "api-" + ids.Short(3)
	ran, err := h.Svc.RunOnce(r.Context(), worker)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"ran": ran, "worker": worker})
}

// ---------- helpers ----------

func (h *Handler) audit(r *http.Request, action, resource string, detail map[string]any) {
	err := h.Svc.Repo.Audit(r.Context(), model.AuditLog{
		WorkspaceID: workspaceOf(r.Context()), Actor: actorOf(r.Context()),
		Action: action, Resource: resource, Detail: detail, RequestID: requestID(r.Context()),
	})
	if err != nil {
		h.Log.Warn("写审计日志失败", "err", err)
	}
}

func checkPassword(hash, plain string) bool { return auth.CheckPassword(hash, plain) }

func yamlUnmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }

var _ = io.Discard
var _ = strings.TrimSpace
