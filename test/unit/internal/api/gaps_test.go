package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apihttp "github.com/godw/godw/internal/api/http"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------- 指标向导的「保存前试算」 ----------

// previewMetric 是向导里的即时试算：草稿口径能出数，但绝不能落库，
// 并且校验规则要和正式保存一致（引用未发布模型必须明确拒绝）。
func TestPreviewMetricDraft(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	today := time.Now().Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", today)

	series := obj(t, e.ok("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{
			"name": "draft_amount", "model": "dwd_order", "agg": "sum",
			"expr": "amount", "timeColumn": "dt",
		},
		"query": map[string]any{"granularity": "day", "limit": 10},
	}))
	if field(t, series, "metric") != "draft_amount" {
		t.Fatalf("试算返回的指标名不对: %v", series)
	}
	if points := list(t, series["points"]); len(points) != 1 {
		t.Fatalf("试算应有 1 个数据点，得到 %d: %v", len(points), series)
	}
	if field(t, series, "sql") == "" {
		t.Fatalf("试算应回显 SQL: %v", series)
	}
	// 试算不落库：指标列表仍然为空。
	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 0 {
		t.Fatalf("试算不应保存指标，得到 %d 条", len(items))
	}

	// 未发布模型 / 非法定义 / 坏 JSON 都要明确失败。
	e.fail("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{"name": "ghost", "model": "no_such", "agg": "sum", "expr": "amount"},
	}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/metrics/preview", map[string]any{
		"metric": map[string]any{"name": "bad", "model": "dwd_order", "agg": "sum", "expr": "sum(amount)"},
	}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/metrics/preview", "{", http.StatusBadRequest)
}

// ---------- 调度预览的非法 cron ----------

// 非法 cron 必须回到「参数错误」，而不是让前端拿到 500 或空列表。
// 这里用百分号编码传参，避免空格被 HTTP 层提前判为畸形请求。
func TestSchedulePreviewInvalidCron(t *testing.T) {
	e := newEnv(t)

	msg := e.fail("GET", "/api/v1/schedule/preview?cron=99%202%20*%20*%20*", nil, http.StatusBadRequest)
	if !strings.Contains(msg, "cron") {
		t.Fatalf("错误信息应指向 cron: %q", msg)
	}
	e.fail("GET", "/api/v1/schedule/preview?cron=not-a-cron", nil, http.StatusBadRequest)
	// 空表达式同样非法。
	e.fail("GET", "/api/v1/schedule/preview", nil, http.StatusBadRequest)

	// 合法表达式的边界：每 5 分钟，未来 5 次都要能算出来。
	prev := obj(t, e.ok("GET", "/api/v1/schedule/preview?cron=*/5%20*%20*%20*%20*", nil))
	if next := list(t, prev["next"]); len(next) != 5 {
		t.Fatalf("应预览 5 次，得到 %d", len(next))
	}
}

// ---------- 直接打 Router：不经过中间件 ----------

// 这批分支只在「请求没有经过 withAuth/withCommon」时才会执行：
// 身份读取的兜底值（anonymous / viewer / 空工作区）与未命名请求 ID。
// 走完整中间件时上下文一定被填满，兜底分支永远不可达，所以这里直接调 Router。
func TestDirectRouterFallbacks(t *testing.T) {
	e := newEnv(t)
	h := apihttp.NewHandler(e.svc, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), e.wsID)

	// /me 会读 actor / workspace / role 三个上下文值，全部落到兜底分支。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("直连 /me 应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	me := fxDecode(t, rec.Body.Bytes())
	if field(t, me, "userId") != "anonymous" || field(t, me, "role") != "viewer" {
		t.Fatalf("无中间件时身份兜底不对: %v", me)
	}
	if field(t, me, "workspaceId") != "" {
		t.Fatalf("无中间件时工作区应为空: %v", me)
	}

	// 文档站导出会写审计日志：审计里的 requestID 同样取不到，
	// 必须回落到空串而不是 panic（这类兜底一旦漏掉就是 500）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/catalog/export?format=html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("直连文档站导出应 200，得到 %d: %.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("文档站导出应为 HTML: %.120s", rec.Body.String())
	}
}

// ---------- readyz：库可达但 schema 未初始化 ----------

// fxUnmigratedEnv 指向一个「连得上、但没有建表」的 schema：
// Ping 成功而版本查询失败，正是 readyz 第二条降级分支的真实场景。
func fxUnmigratedEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := testsupport.Config(t)
	cfg.Env = "test"
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))

	schema := "empty_" + randomSuffix()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testsupport.DSN())
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("创建空 schema 失败: %v", err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		c, cerr := pgx.Connect(context.Background(), testsupport.DSN())
		if cerr == nil {
			_, _ = c.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			_ = c.Close(context.Background())
		}
	})

	pc, err := pgxpool.ParseConfig(testsupport.DSN())
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	// 只把这个池子指到空 schema，不动物理库的 search_path。
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatalf("构造空 schema 连接池失败: %v", err)
	}
	t.Cleanup(pool.Close)

	svc, err := service.New(&repo.DB{Pool: pool}, cfg, lg)
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	h := apihttp.NewHandler(svc, cfg, lg, "ws_empty")
	srv := httptest.NewServer(h.Middleware())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv, svc: svc, cfg: cfg, wsID: "ws_empty"}
}

// 库连得上但迁移没跑过时，readyz 必须返回「schema 未初始化」，
// 而不是伪装成 ready —— 否则编排系统会把流量打到一张空库上。
func TestReadyzSchemaNotInitialized(t *testing.T) {
	e := fxUnmigratedEnv(t)

	status, data, raw := e.doRaw("GET", "/readyz", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("schema 未初始化时 readyz 应 503，得到 %d: %s", status, raw)
	}
	_ = data
	if !strings.Contains(raw, "schema 未初始化") {
		t.Fatalf("错误信息应提示执行迁移: %s", raw)
	}
}

// ---------- 流式接口的收尾分支（直连 Router） ----------

// 事件流有两个「静默退出」分支：客户端断开、以及实例已不存在。
// 两者都不会给客户端任何响应体，只能通过「handler 及时返回」来验证。
func TestInstanceEventsQuietExit(t *testing.T) {
	e := newEnv(t)
	h := apihttp.NewHandler(e.svc, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), e.wsID)

	// 客户端断开：请求上下文一取消，handler 必须立刻返回。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/instances/ins_x/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	fxServeWithin(t, h, rec, req, 5*time.Second)
	if got := rec.Body.String(); got != "" {
		t.Fatalf("连接已取消时不应写出事件: %q", got)
	}

	// 实例不存在：ticker 到点后查库失败，handler 静默返回。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/instances/ins_ghost/events", nil)
	fxServeWithin(t, h, rec, req, 8*time.Second)
	if got := rec.Body.String(); strings.Contains(got, "event: done") {
		t.Fatalf("实例不存在时不应发 done: %q", got)
	}
}

// 日志流同理：客户端断开时不能继续推流。
func TestInstanceLogStreamQuietExit(t *testing.T) {
	e := newEnv(t)
	h := apihttp.NewHandler(e.svc, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), e.wsID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/instances/ins_x/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	fxServeWithin(t, h, rec, req, 5*time.Second)
	if got := rec.Body.String(); got != "" {
		t.Fatalf("连接已取消时不应推日志: %q", got)
	}
}

// fxServeWithin 在后台跑 handler，超时即判定为「挂住」。
func fxServeWithin(t *testing.T, h *apihttp.Handler, rec *httptest.ResponseRecorder, req *http.Request, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("handler 超时未返回: %s", req.URL.Path)
	}
}

// ---------- 选择器真触发 ----------

// selectRun 不走 dryRun 时必须真的建实例，并统计「新建了几条」。
// 这个计数是前端「本次触发了 N 条管道」的唯一依据。
func TestSelectRunCreatesInstances(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	// dryRun 只解析，不建实例。
	dry := obj(t, e.ok("POST", "/api/v1/runs/select", map[string]any{
		"select": "dwd_order+", "dryRun": true,
	}))
	if runs := list(t, dry["runs"]); len(runs) != 0 {
		t.Fatalf("dryRun 不应产生运行记录: %v", dry)
	}

	// 真触发：每条命中管道都要有独立结果，created 必须等于「成功且新建」的条数。
	// trigger_type=select 已进 CHECK 白名单（migrations/0007），这里可以断言全部成功。
	res := obj(t, e.ok("POST", "/api/v1/runs/select", map[string]any{
		"select": "dwd_order+", "bizDate": "2024-01-03",
	}))
	runs := list(t, res["runs"])
	if len(runs) == 0 {
		t.Fatalf("选择器应命中至少一条管道: %v", res)
	}
	created := 0
	for _, r := range runs {
		one := obj(t, r)
		if field(t, one, "pipeline") == "" {
			t.Fatalf("运行记录缺少管道名: %v", one)
		}
		if msg, _ := one["error"].(string); msg != "" {
			t.Fatalf("选择器触发不应失败: %v", one)
		}
		if one["created"] != true {
			t.Fatalf("命中管道应全部创建实例: %v", one)
		}
		created++
	}
	if fxInt(t, res, "created") != created || created != len(runs) {
		t.Fatalf("created 计数与逐条结果不一致: %v", res)
	}

	// 解析结果必须与命中清单一致：前端用它展示「这次会影响哪些模型」。
	resolved := fxMap(t, res, "resolved")
	if models := list(t, resolved["models"]); len(models) != 1 || models[0] != "dwd_order" {
		t.Fatalf("解析出的模型不对: %v", resolved)
	}
}

// ---------- 导出目录使用默认值 ----------

// 未配置 semantic.exportDir 时回落到 ./data/exports。
// 下载边界必须基于这个「解析后的绝对目录」判断，否则默认配置下会把合法文件判为越界。
func TestExportDownloadWithDefaultDir(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) { c.Semantic.ExportDir = "" })

	dir := filepath.Join("data", "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建默认导出目录失败: %v", err)
	}
	// 只回收本用例写下的文件与空目录，避免误删同目录下的其他产物。
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(dir, "default_dir.csv"))
		_ = os.Remove(dir)
		_ = os.Remove("data")
	})

	file := filepath.Join(dir, "default_dir.csv")
	if err := os.WriteFile(file, []byte("x,y\n1,2\n"), 0o644); err != nil {
		t.Fatalf("写入导出文件失败: %v", err)
	}
	job, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "m", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("创建导出任务失败: %v", err)
	}
	if err := e.svc.Repo.FinishExport(context.Background(), job.ID, "success", file, "", 2); err != nil {
		t.Fatalf("回写导出结果失败: %v", err)
	}
	status, _, body := e.doRaw("GET", "/api/v1/exports/"+job.ID+"/download", nil)
	if status != http.StatusOK || !strings.Contains(body, "x,y") {
		t.Fatalf("默认目录下的导出应可下载(%d): %s", status, body)
	}

	// 目录外的文件依旧拒绝：默认值不能放宽边界。
	outside := filepath.Join(os.TempDir(), "godw-default-outside.csv")
	if err := os.WriteFile(outside, []byte("no"), 0o644); err != nil {
		t.Fatalf("写入越界文件失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	job2, err := e.svc.Repo.CreateExport(context.Background(), model.ExportJob{
		WorkspaceID: e.wsID, Kind: "metric", Target: "m", CreatedBy: e.admin,
	})
	if err != nil {
		t.Fatalf("创建越界导出任务失败: %v", err)
	}
	if err := e.svc.Repo.FinishExport(context.Background(), job2.ID, "success", outside, "", 1); err != nil {
		t.Fatalf("回写越界导出失败: %v", err)
	}
	e.fail("GET", "/api/v1/exports/"+job2.ID+"/download", nil, http.StatusForbidden)
}

// ---------- 重跑时管道已不在当前工作区 ----------

// 整实例重跑依赖「管道仍在当前工作区」。管道被迁走或删除后，
// 必须明确报「管道不存在」，不能静默建出一条孤儿实例。
func TestRetryInstancePipelineMovedAway(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	p := fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	other, err := e.svc.Repo.CreateWorkspace(context.Background(), "team_other", "另一个团队")
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`UPDATE pipeline SET workspace_id=$1 WHERE id=$2`, other.ID, field(t, p, "id")); err != nil {
		t.Fatalf("迁移管道归属失败: %v", err)
	}

	msg := e.fail("POST", "/api/v1/instances/"+id+"/retry", nil, http.StatusNotFound)
	if !strings.Contains(msg, "管道不存在") {
		t.Fatalf("应提示管道不存在，得到 %q", msg)
	}
}

// fxDecode 把响应体解码成 map，供直连 Router 的用例使用（没有 doRaw 的包装）。
func fxDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, raw)
	}
	return env.Data
}
