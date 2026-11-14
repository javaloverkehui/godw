package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/obs/alert"
)

// 指标是语义层对外的第一个 API：定义要能存、查询要真出数、维度未声明要被拒。
func TestMetricApplyAndQuery(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	today := time.Now().Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", today, today)

	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 0 {
		t.Fatalf("初始指标应为空，得到 %d", len(items))
	}

	// YAML 语法错误在解析阶段 400。
	e.fail("POST", "/api/v1/metrics", map[string]any{"yaml": "name: ["}, http.StatusBadRequest)
	// 引用未发布模型：400，并指出是哪个模型。
	msg := e.fail("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: ghost_gmv
model: no_such_model
agg: sum
expr: amount
`}, http.StatusBadRequest)
	if !strings.Contains(msg, "未发布") {
		t.Fatalf("应提示模型未发布，得到 %q", msg)
	}
	// 表达式必须是列名，不允许写 SQL 片段。
	e.fail("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: bad_expr
model: dwd_order
agg: sum
expr: "sum(amount)"
`}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/metrics", map[string]any{"yaml": "kind: Metric"}, http.StatusForbidden)

	metricYAML := `kind: Metric
name: order_amount
displayName: 订单金额
model: dwd_order
agg: sum
expr: amount
timeColumn: dt
dimensions: [status]
unit: 元
`
	saved := obj(t, e.ok("POST", "/api/v1/metrics", map[string]any{"yaml": metricYAML}))
	if field(t, saved, "name") != "order_amount" || field(t, saved, "unit") != "元" {
		t.Fatalf("指标保存结果不对: %v", saved)
	}
	if items := list(t, e.ok("GET", "/api/v1/metrics", nil)); len(items) != 1 {
		t.Fatalf("指标列表应有 1 条，得到 %d", len(items))
	}

	// 查询：按天聚合，指定已声明维度。
	series := obj(t, e.ok("GET", "/api/v1/metrics/order_amount/query?granularity=day&dimension=status&limit=10", nil))
	if field(t, series, "metric") != "order_amount" {
		t.Fatalf("查询返回的指标名不对: %v", series)
	}
	points := list(t, series["points"])
	if len(points) != 1 {
		t.Fatalf("应有 1 个数据点，得到 %d: %v", len(points), series)
	}
	point := obj(t, points[0])
	if field(t, point, "dim") != "paid" || point["value"] == nil {
		t.Fatalf("数据点内容不对: %v", point)
	}
	if field(t, series, "sql") == "" {
		t.Fatalf("查询应回显实际执行的 SQL: %v", series)
	}

	// 未声明维度、非法粒度、未知指标分别是 400 / 400 / 404。
	e.fail("GET", "/api/v1/metrics/order_amount/query?dimension=dt", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/metrics/order_amount/query?granularity=hour", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/metrics/ghost/query", nil, http.StatusNotFound)

	// 缓存关闭时 Cached 必须缺席（或为假），避免前端展示假的「命中缓存」。
	if v, ok := series["cached"]; ok && v == true {
		t.Fatalf("未开启缓存时不应标记 cached: %v", series)
	}
}

// 看板是卡片的容器：两种提交形态（结构化 / YAML）都要支持，
// 单卡失败挂在卡片上，不能整页 500。
func TestDashboardApplyAndQuery(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	// 看板卡片的窗口是 [今天-days, 今天)，不含今天，所以数据要落在昨天。
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	fxSeedOrders(t, e, schema, "dwd_order", yesterday)

	e.ok("POST", "/api/v1/metrics", map[string]any{"yaml": `kind: Metric
name: order_amount
model: dwd_order
agg: sum
expr: amount
timeColumn: dt
dimensions: [status]
`})

	if items := list(t, e.ok("GET", "/api/v1/dashboards", nil)); len(items) != 0 {
		t.Fatalf("初始看板应为空，得到 %d", len(items))
	}

	// 既没 dashboard 也没 yaml：400。
	e.fail("POST", "/api/v1/dashboards", map[string]any{}, http.StatusBadRequest)
	// 卡片缺陷：days<=0。
	e.fail("POST", "/api/v1/dashboards", map[string]any{"dashboard": map[string]any{
		"name": "bad", "cards": []any{map[string]any{"title": "x", "metric": "order_amount", "days": 0}},
	}}, http.StatusBadRequest)
	// 引用不存在的指标。
	e.fail("POST", "/api/v1/dashboards", map[string]any{"dashboard": map[string]any{
		"name": "bad", "cards": []any{map[string]any{"title": "x", "metric": "ghost", "days": 7}},
	}}, http.StatusBadRequest)
	// YAML 语法错误。
	e.fail("POST", "/api/v1/dashboards", map[string]any{"yaml": "cards: ["}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/dashboards", map[string]any{"yaml": "name: x"}, http.StatusForbidden)

	// 结构化提交。
	dash := obj(t, e.ok("POST", "/api/v1/dashboards", map[string]any{"dashboard": map[string]any{
		"name": "ops", "description": "经营总览",
		"cards": []any{map[string]any{"title": "订单金额", "metric": "order_amount", "days": 30, "span": 2}},
	}}))
	if field(t, dash, "name") != "ops" || len(list(t, dash["cards"])) != 1 {
		t.Fatalf("看板保存结果不对: %v", dash)
	}

	// 查询：卡片数据与错误字段都要存在。
	data := obj(t, e.ok("GET", "/api/v1/dashboards/ops?days=30", nil))
	cards := list(t, data["cards"])
	if len(cards) != 1 {
		t.Fatalf("查询应返回 1 张卡片，得到 %d", len(cards))
	}
	card := obj(t, cards[0])
	if field(t, card, "kind") != "metric" || card["error"] != nil {
		t.Fatalf("卡片渲染失败: %v", card)
	}
	if len(list(t, card["points"])) == 0 {
		t.Fatalf("卡片应有点数据: %v", card)
	}
	e.fail("GET", "/api/v1/dashboards/ghost", nil, http.StatusNotFound)

	// 同一份定义走 YAML 也能覆盖已有看板（upsert）。
	e.ok("POST", "/api/v1/dashboards", map[string]any{"yaml": `kind: Dashboard
name: ops
description: 经营总览
cards:
  - title: 订单金额
    metric: order_amount
    days: 7
`})
	if items := list(t, e.ok("GET", "/api/v1/dashboards", nil)); len(items) != 1 {
		t.Fatalf("YAML 覆盖后仍应只有 1 个看板，得到 %d", len(items))
	}
	again := obj(t, e.ok("GET", "/api/v1/dashboards/ops", nil))
	if d := fxInt(t, obj(t, list(t, again["cards"])[0]), "days"); d != 7 {
		t.Fatalf("YAML 覆盖未生效，days=%d", d)
	}
}

// 自助查询的边界就是它的安全边界：只读、单语句、行数上限、角色门禁。
func TestRawSQLQuery(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")

	// 只读校验先于连接：写语句与多语句都在这里被拒。
	e.fail("POST", "/api/v1/sql/query", map[string]any{"datasource": ds, "sql": "DELETE FROM t"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/sql/query", map[string]any{"datasource": ds, "sql": "SELECT 1; SELECT 2"}, http.StatusBadRequest)
	// 未指定数据源。
	e.fail("POST", "/api/v1/sql/query", map[string]any{"sql": "SELECT 1"}, http.StatusBadRequest)
	// 未知数据源。
	e.fail("POST", "/api/v1/sql/query", map[string]any{"datasource": "ghost", "sql": "SELECT 1"}, http.StatusNotFound)
	// viewer 不能查。
	e.as("viewer").fail("POST", "/api/v1/sql/query", map[string]any{"datasource": ds, "sql": "SELECT 1"}, http.StatusForbidden)

	res := obj(t, e.ok("POST", "/api/v1/sql/query", map[string]any{"datasource": ds, "sql": "SELECT 1 AS n"}))
	if cols := list(t, res["columns"]); len(cols) != 1 || cols[0] != "n" {
		t.Fatalf("列信息不对: %v", res)
	}
	if fxInt(t, res, "rowCount") != 1 || fxInt(t, res, "limit") != 500 {
		t.Fatalf("结果元信息不对: %v", res)
	}
	// 固定提示：原始模式不注入行级策略，必须显式告知使用者。
	notes := list(t, res["notes"])
	if len(notes) == 0 || !strings.Contains(notes[0].(string), "只读") {
		t.Fatalf("应带只读说明: %v", notes)
	}

	// 行数上限：limit=1 + 两行结果 → 截断。
	trunc := obj(t, e.ok("POST", "/api/v1/sql/query", map[string]any{
		"datasource": ds, "sql": "SELECT generate_series(1,3) AS n", "limit": 1,
	}))
	if !fxBool(t, trunc, "truncated") || fxInt(t, trunc, "rowCount") != 1 {
		t.Fatalf("limit 未生效: %v", trunc)
	}
	// 超上限的 limit 被夹紧到 MaxRawSQLLimit。
	capped := obj(t, e.ok("POST", "/api/v1/sql/query", map[string]any{
		"datasource": ds, "sql": "SELECT 1 AS n", "limit": 999999,
	}))
	if fxInt(t, capped, "limit") == 999999 {
		t.Fatalf("limit 应被夹紧: %v", capped)
	}
}

// 队列与资源组：资源组的并发上限决定分发，接口要能把「占用 / 上限」一起给出。
func TestQueueAndResourceGroups(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 初始：资源组表为空，但排队实例已经存在。
	q := obj(t, e.ok("GET", "/api/v1/queue", nil))
	if pending := list(t, q["pending"]); len(pending) != 1 {
		t.Fatalf("队列应有 1 条待执行实例，得到 %d", len(pending))
	}
	if groups := list(t, e.ok("GET", "/api/v1/resource-groups", nil)); len(groups) != 0 {
		t.Fatalf("初始资源组应为空，得到 %d", len(groups))
	}

	// 保存资源组：空名回落到 default，非法并发被拒。
	// 响应回显的是请求里的名字（handler 不做补全），所以用显式名字断言。
	if got := obj(t, e.ok("POST", "/api/v1/resource-groups", map[string]any{
		"name": "default", "maxConcurrency": 2, "maxTimeoutSec": 600, "maxPool": 5, "engines": []string{"postgres"},
	})); field(t, got, "name") != "default" {
		t.Fatalf("资源组保存响应不对: %v", got)
	}
	// 并发上限必须在 1..128：越界才真正触发校验（负数会被补成默认值）。
	e.fail("POST", "/api/v1/resource-groups", map[string]any{
		"name": "bad", "maxConcurrency": 999, "maxTimeoutSec": 600, "maxPool": 5,
	}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/resource-groups", map[string]any{"name": "etl"}, http.StatusForbidden)

	groups := list(t, e.ok("GET", "/api/v1/resource-groups", nil))
	if len(groups) != 1 {
		t.Fatalf("资源组应有 1 条，得到 %d", len(groups))
	}
	// 队列视图带上占用与上限。
	q = obj(t, e.ok("GET", "/api/v1/queue", nil))
	usage := fxMap(t, q, "usage")
	def := fxMap(t, usage, "default")
	if fxInt(t, def, "limit") != 2 {
		t.Fatalf("usage 上限不对: %v", usage)
	}

	// 分发：排队中的实例已被提升过，这里只验证接口返回计数且不报错。
	disp := obj(t, e.ok("POST", "/api/v1/queue/dispatch", nil))
	if _, ok := disp["promoted"]; !ok {
		t.Fatalf("分发响应应带 promoted: %v", disp)
	}
	e.as("viewer").fail("POST", "/api/v1/queue/dispatch", nil, http.StatusForbidden)
	// runner 角色也能分发。
	e.as("runner").ok("POST", "/api/v1/queue/dispatch", nil)
}

// 告警规则与事件：规则要能被构造事实即时验证，事件要能改状态。
func TestAlertRulesAndEvents(t *testing.T) {
	e := newEnv(t)

	if rules := list(t, e.ok("GET", "/api/v1/alert-rules", nil)); len(rules) != 0 {
		t.Fatalf("初始规则应为空，得到 %d", len(rules))
	}
	// 规则名必填。
	e.fail("POST", "/api/v1/alert-rules", map[string]any{"kind": "failure"}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/alert-rules", map[string]any{"name": "x"}, http.StatusForbidden)

	saved := obj(t, e.ok("POST", "/api/v1/alert-rules", map[string]any{
		"name": "failure-rule", "kind": "failure", "target": "*",
		"severity": "critical", "enabled": true, "channels": []string{},
	}))
	if field(t, saved, "name") != "failure-rule" || field(t, saved, "kind") != "failure" {
		t.Fatalf("规则保存结果不对: %v", saved)
	}
	if rules := list(t, e.ok("GET", "/api/v1/alert-rules", nil)); len(rules) != 1 {
		t.Fatalf("规则列表应有 1 条，得到 %d", len(rules))
	}

	// 即时验证：构造一条失败事实，规则应触发。
	tested := obj(t, e.ok("POST", "/api/v1/alert-rules/test", map[string]any{
		"rule": map[string]any{"name": "failure-rule", "kind": "failure", "target": "*", "severity": "error", "enabled": true},
		"fact": map[string]any{"status": "failed", "pipeline": "demo"},
	}))
	if !fxBool(t, tested, "triggered") {
		t.Fatalf("失败事实应触发规则: %v", tested)
	}
	// 未启用 / 目标不匹配时不触发。
	notTriggered := obj(t, e.ok("POST", "/api/v1/alert-rules/test", map[string]any{
		"rule": map[string]any{"name": "failure-rule", "kind": "failure", "target": "other", "severity": "error", "enabled": true},
		"fact": map[string]any{"status": "failed", "pipeline": "demo"},
	}))
	if fxBool(t, notTriggered, "triggered") {
		t.Fatalf("目标不匹配不应触发: %v", notTriggered)
	}
	e.fail("POST", "/api/v1/alert-rules/test", `{"rule":`, http.StatusBadRequest)

	// 事件列表：先造一条，再按状态过滤。事件必须挂在已存在的规则上（外键）。
	ctx := context.Background()
	ev, created, err := e.svc.Repo.EmitAlert(ctx, e.wsID, alert.Event{
		RuleID: field(t, saved, "id"), RuleName: "failure-rule", Pipeline: "demo", Severity: "critical",
		Title: "管道失败", Detail: "demo 执行失败", DedupeKey: "demo:2024-01-02",
	})
	if err != nil || !created {
		t.Fatalf("写入告警事件失败: %v", err)
	}
	events := list(t, e.ok("GET", "/api/v1/alerts", nil))
	if len(events) != 1 {
		t.Fatalf("告警列表应有 1 条，得到 %d", len(events))
	}
	if got := obj(t, events[0]); field(t, got, "title") != "管道失败" {
		t.Fatalf("告警内容不对: %v", got)
	}
	if filtered := list(t, e.ok("GET", "/api/v1/alerts?status=resolved", nil)); len(filtered) != 0 {
		t.Fatalf("按状态过滤应返回空，得到 %d", len(filtered))
	}

	// 状态机：非法状态 400；合法状态落库。
	e.fail("POST", "/api/v1/alerts/"+ev.ID+"/handle", map[string]any{"status": "closed"}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/alerts/"+ev.ID+"/handle", map[string]any{"status": "resolved"}, http.StatusForbidden)
	handled := obj(t, e.ok("POST", "/api/v1/alerts/"+ev.ID+"/handle", map[string]any{"status": "acknowledged"}))
	if field(t, handled, "status") != "acknowledged" {
		t.Fatalf("处理响应不对: %v", handled)
	}
	if acked := list(t, e.ok("GET", "/api/v1/alerts?status=acknowledged", nil)); len(acked) != 1 {
		t.Fatalf("确认后应按状态可查，得到 %d", len(acked))
	}

	// 删除规则后列表为空。
	e.ok("DELETE", "/api/v1/alert-rules/failure-rule", nil)
	if rules := list(t, e.ok("GET", "/api/v1/alert-rules", nil)); len(rules) != 0 {
		t.Fatalf("删除后规则应为空，得到 %d", len(rules))
	}
}

// 调度预览与 worker 空转：一个验证 cron 解析与未来 5 次，
// 一个验证「没有排队任务时如实返回 ran=false」。
func TestSchedulePreviewAndRunOnce(t *testing.T) {
	e := newEnv(t)

	// 合法 cron：返回规范化表达式与 5 个未来时刻。
	prev := obj(t, e.ok("GET", "/api/v1/schedule/preview?cron=0%202%20*%20*%20*", nil))
	if field(t, prev, "cron") != "0 2 * * *" {
		t.Fatalf("cron 回显不对: %v", prev)
	}
	if next := list(t, prev["next"]); len(next) != 5 {
		t.Fatalf("应预览 5 次，得到 %d", len(next))
	}
	// 段数不对 / 非法取值都是 400。
	e.fail("GET", "/api/v1/schedule/preview?cron=* * *", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/schedule/preview?cron=99 2 * * *", nil, http.StatusBadRequest)

	// 空队列：ran=false，worker 名带 api- 前缀。
	once := obj(t, e.ok("POST", "/api/v1/worker/run-once", nil))
	if fxBool(t, once, "ran") || !strings.HasPrefix(field(t, once, "worker"), "api-") {
		t.Fatalf("空队列 runOnce 结果不对: %v", once)
	}
	e.as("viewer").fail("POST", "/api/v1/worker/run-once", nil, http.StatusForbidden)
}

// 内嵌控制台不依赖 npm：首页与静态资源必须由二进制直接服务，
// 并且不经过鉴权（浏览器加载页面时无法携带 Authorization）。
func TestEmbeddedUI(t *testing.T) {
	e := newEnv(t)
	anon := *e
	anon.token = ""

	status, _, index := anon.doRaw("GET", "/", nil)
	if status != http.StatusOK || !strings.Contains(strings.ToLower(index), "<html") {
		t.Fatalf("首页应返回 HTML(%d): %.120s", status, index)
	}
	status, _, css := anon.doRaw("GET", "/assets/app.css", nil)
	if status != http.StatusOK || !strings.Contains(css, "{") {
		t.Fatalf("样式资源应返回 CSS(%d)", status)
	}
	status, _, js := anon.doRaw("GET", "/assets/app.js", nil)
	if status != http.StatusOK || js == "" {
		t.Fatalf("脚本资源应返回内容(%d)", status)
	}
	// 不存在的资源交给 FileServer 返回 404。
	if status, _, _ := anon.doRaw("GET", "/assets/nope.js", nil); status != http.StatusNotFound {
		t.Fatalf("未知资源应为 404，得到 %d", status)
	}
}
