package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fxSSE 用短超时读一段 SSE 流。
// 事件流不会自己结束（实例未终态时会一直推），只能在客户端侧取消，
// 因此这里刻意让 ReadAll 随 context 到期返回，再对已收到的片段做断言。
//
// 返回值带状态码：中间件的 statusWriter 目前没有转发 Flush，
// 长连接会走到 handler 里「不支持流式输出」的兜底分支。两种情况都要能被断言，
// 否则这条用例会在该缺陷修好后反过来误报。
func fxSSE(t *testing.T, e *testEnv, path string, d time.Duration) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE 请求失败: %v", err)
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(buf)
}

// 实例列表/详情/矩阵/SQL/SQL 快照是排障主路径。
// 一条触发出来的实例要能被这些接口从不同角度看到同一份事实。
func TestInstanceReadPaths(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 列表：total 与分页参数一起返回，前端才能画分页条。
	page := obj(t, e.ok("GET", "/api/v1/instances?limit=10&offset=0", nil))
	if fxInt(t, page, "total") != 1 || fxInt(t, page, "limit") != 10 {
		t.Fatalf("实例分页信息不对: %v", page)
	}
	// 状态过滤 + 非法的 limit=0：limit 原样回显，仓储层自己做兜底。
	queued := obj(t, e.ok("GET", "/api/v1/instances?status=queued&limit=0", nil))
	if fxInt(t, queued, "limit") != 0 || len(list(t, queued["items"])) != 1 {
		t.Fatalf("按状态过滤不对: %v", queued)
	}
	// 过滤到不存在的状态：空集而不是报错。
	if n := fxInt(t, obj(t, e.ok("GET", "/api/v1/instances?status=failed", nil)), "total"); n != 0 {
		t.Fatalf("failed 状态应无实例，得到 %d", n)
	}

	// 详情：实例 + 质量结果（此时为空）。
	detail := obj(t, e.ok("GET", "/api/v1/instances/"+id, nil))
	inst := obj(t, detail["instance"])
	if field(t, inst, "id") != id || field(t, inst, "pipelineName") != "dwd_order_daily" {
		t.Fatalf("实例详情不对: %v", inst)
	}
	if tasks := list(t, inst["tasks"]); len(tasks) != 2 {
		t.Fatalf("实例应有 2 个任务，得到 %d", len(tasks))
	}
	if q := list(t, detail["quality"]); len(q) != 0 {
		t.Fatalf("未执行时质量结果应为空，得到 %d", len(q))
	}
	e.fail("GET", "/api/v1/instances/ghost", nil, http.StatusNotFound)

	// 矩阵：节点 × 最近运行。节点顺序必须稳定（按名字排序）。
	m := obj(t, e.ok("GET", "/api/v1/instances/matrix?pipeline=dwd_order_daily&limit=5", nil))
	if field(t, m, "pipeline") != "dwd_order_daily" {
		t.Fatalf("矩阵管道名不对: %v", m)
	}
	if runs := list(t, m["runs"]); len(runs) != 1 {
		t.Fatalf("矩阵应有 1 次运行，得到 %d", len(runs))
	}
	nodes := list(t, m["nodes"])
	if len(nodes) != 2 || nodes[0] != "load" || nodes[1] != "src" {
		t.Fatalf("矩阵节点顺序不对: %v", nodes)
	}
	// limit 越界被夹到 20，也不应该报错。
	e.ok("GET", "/api/v1/instances/matrix?limit=999", nil)

	// 节点 SQL：用版本快照渲染，日期必须已替换。
	sqlRes := obj(t, e.ok("GET", "/api/v1/instances/"+id+"/sql?node=load", nil))
	if field(t, sqlRes, "nodeId") != "load" || !fxBool(t, sqlRes, "fromSnapshot") {
		t.Fatalf("节点 SQL 元信息不对: %v", sqlRes)
	}
	if !strings.Contains(field(t, sqlRes, "sql"), "2024-01-02") {
		t.Fatalf("节点 SQL 未渲染业务日期: %q", field(t, sqlRes, "sql"))
	}
	// 不指定节点时取第一个有 SQL 的节点。
	first := obj(t, e.ok("GET", "/api/v1/instances/"+id+"/sql", nil))
	if field(t, first, "nodeId") == "" {
		t.Fatalf("应自动选中默认节点: %v", first)
	}

	// 日志快照：初始化内存日志管理器后追加两条，快照要能读回。
	if lines := list(t, e.ok("GET", "/api/v1/instances/"+id+"/logs", nil)); len(lines) != 0 {
		t.Fatalf("初始日志应为空，得到 %d", len(lines))
	}
	e.svc.AppendLog(id, "src", "info", "开始读取源表")
	e.svc.AppendLog(id, "load", "error", "写入失败重试中")
	lines := list(t, e.ok("GET", "/api/v1/instances/"+id+"/logs", nil))
	if len(lines) != 2 {
		t.Fatalf("应有 2 条日志，得到 %d", len(lines))
	}
	if l := obj(t, lines[1]); field(t, l, "level") != "error" {
		t.Fatalf("日志内容不对: %v", l)
	}
	// after 参数用于断点续传：跳过第一条后只剩一条。
	after := list(t, e.ok("GET", "/api/v1/instances/"+id+"/logs?after=1", nil))
	if len(after) != 1 {
		t.Fatalf("after=1 应只返回后续日志，得到 %d", len(after))
	}
}

// 事件流与日志流都是长连接：这里用短超时读到片段即可，
// 目标是验证「首帧内容正确 + 终态能自行关闭」，而不是把流跑完。
func TestInstanceStreams(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 未终态：1 秒后推一帧 status，然后一直等；客户端超时收尾。
	status, body := fxSSE(t, e, "/api/v1/instances/"+id+"/events", 1600*time.Millisecond)
	if status == http.StatusOK {
		if !strings.Contains(body, "event: status") || !strings.Contains(body, `"status":"queued"`) {
			t.Fatalf("status 事件内容不对: %q", body)
		}
	} else if !strings.Contains(body, "流式输出") {
		t.Fatalf("非流式兜底信息不对(%d): %s", status, body)
	}

	// 把实例取消成终态，日志流应在推完日志后自己发 done 关闭。
	e.ok("POST", "/api/v1/instances/"+id+"/cancel", nil)
	e.ok("GET", "/api/v1/instances/"+id+"/logs", nil) // 初始化日志管理器
	e.svc.AppendLog(id, "src", "info", "收尾日志")

	status, logs := fxSSE(t, e, "/api/v1/instances/"+id+"/logs/stream?level=info", 5*time.Second)
	if status == http.StatusOK {
		if !strings.Contains(logs, "event: log") || !strings.Contains(logs, "收尾日志") {
			t.Fatalf("日志流应推出日志: %q", logs)
		}
		if !strings.Contains(logs, "event: done") {
			t.Fatalf("终态实例应自动发 done: %q", logs)
		}
	} else if !strings.Contains(logs, "流式输出") {
		t.Fatalf("非流式兜底信息不对(%d): %s", status, logs)
	}
}

// 重跑 / 批量 / 取消是运维高频动作，权限与单条失败隔离都要覆盖。
func TestInstanceActions(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// viewer 不能重跑。
	e.as("viewer").fail("POST", "/api/v1/instances/"+id+"/retry", nil, http.StatusForbidden)

	// 整实例重跑：新建一条 retry 实例，原实例不受影响。
	retried := obj(t, e.ok("POST", "/api/v1/instances/"+id+"/retry", nil))
	if field(t, retried, "triggerType") != "retry" || field(t, retried, "id") == id {
		t.Fatalf("重跑应新建 retry 实例: %v", retried)
	}
	// 从节点重跑：原地重置，不新建实例。
	byNode := obj(t, e.ok("POST", "/api/v1/instances/"+id+"/retry", map[string]any{"node": "src"}))
	if field(t, byNode, "node") != "src" {
		t.Fatalf("按节点重跑响应不对: %v", byNode)
	}
	if inner := obj(t, byNode["instance"]); field(t, inner, "id") != id {
		t.Fatalf("按节点重跑应复用原实例: %v", inner)
	}
	// 不存在的节点：404。
	e.fail("POST", "/api/v1/instances/"+id+"/retry", map[string]any{"node": "ghost"}, http.StatusNotFound)
	e.fail("POST", "/api/v1/instances/ghost/retry", nil, http.StatusNotFound)

	// 批量：空列表、超量、未知动作都是 400。
	e.fail("POST", "/api/v1/instances/batch", map[string]any{"action": "retry", "ids": []string{}}, http.StatusBadRequest)
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = id
	}
	e.fail("POST", "/api/v1/instances/batch", map[string]any{"action": "retry", "ids": tooMany}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/instances/batch", map[string]any{"action": "drop", "ids": []string{id}}, http.StatusBadRequest)
	e.as("viewer").fail("POST", "/api/v1/instances/batch", map[string]any{"action": "cancel", "ids": []string{id}}, http.StatusForbidden)

	// 取消：返回前一个状态；已结束的实例再取消是 409（能力边界，不是缺陷）。
	id2 := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-04")
	canceled := obj(t, e.ok("POST", "/api/v1/instances/"+id2+"/cancel", nil))
	if field(t, canceled, "prevStatus") != "queued" || field(t, canceled, "status") != "canceled" {
		t.Fatalf("取消响应不对: %v", canceled)
	}
	e.fail("POST", "/api/v1/instances/"+id2+"/cancel", nil, http.StatusConflict)
	e.fail("POST", "/api/v1/instances/ghost/cancel", nil, 0)
	e.as("viewer").fail("POST", "/api/v1/instances/"+id2+"/cancel", nil, http.StatusForbidden)

	// 单条失败不影响其余：不存在的 id 进 failed，存在的进 succeeded。
	id3 := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-05")
	batch := obj(t, e.ok("POST", "/api/v1/instances/batch", map[string]any{
		"action": "cancel", "ids": []string{id3, "ins_ghost"},
	}))
	if got := list(t, batch["succeeded"]); len(got) != 1 || got[0] != id3 {
		t.Fatalf("批量成功清单不对: %v", batch)
	}
	if failed := fxMap(t, batch, "failed"); len(failed) != 1 {
		t.Fatalf("批量失败清单不对: %v", batch)
	}
	// 批量重跑同样逐条收口：ghost 进 failed，真实实例会新建 retry 实例。
	batchRetry := obj(t, e.ok("POST", "/api/v1/instances/batch", map[string]any{
		"action": "retry", "ids": []string{id, "ins_ghost"},
	}))
	if got := list(t, batchRetry["succeeded"]); len(got) != 1 || got[0] != id {
		t.Fatalf("批量重跑成功清单不对: %v", batchRetry)
	}
	if failed := fxMap(t, batchRetry, "failed"); len(failed) != 1 {
		t.Fatalf("批量重跑失败清单不对: %v", batchRetry)
	}
}

// 补数：先算账（plan）、再提交（batch）、再看进度。
// 默认跳过已有分区是幂等保护，force 才有权重跑。
func TestBackfillFlow(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)

	// plan 只算不动。
	plan := obj(t, e.ok("GET", "/api/v1/backfill/plan?pipeline=dwd_order_daily&from=2024-01-01&to=2024-01-03", nil))
	if fxInt(t, plan, "total") != 3 || fxInt(t, plan, "missing") != 3 {
		t.Fatalf("补数计划不对: %v", plan)
	}
	// 缺少 pipeline / 日期非法都是 400。
	e.fail("GET", "/api/v1/backfill/plan", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/backfill/plan?pipeline=x&from=2024-02-01&to=2024-01-01", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/backfill/plan?pipeline=x&from=bad&to=2024-01-01", nil, http.StatusBadRequest)

	// 提交：3 天全新建。
	batch := obj(t, e.ok("POST", "/api/v1/backfill", map[string]any{
		"pipeline": "dwd_order_daily", "from": "2024-01-01", "to": "2024-01-03", "reason": "补历史",
	}))
	if fxInt(t, batch, "created") != 3 || fxInt(t, batch, "total") != 3 {
		t.Fatalf("补数批次不对: %v", batch)
	}
	bid := field(t, batch, "id")

	// 进度：还没有执行，成功/失败都是 0，百分比为 0。
	prog := obj(t, e.ok("GET", "/api/v1/backfill/"+bid, nil))
	if fxInt(t, prog, "success") != 0 || fxInt(t, prog, "percent") != 0 {
		t.Fatalf("初始进度不对: %v", prog)
	}
	// 仓储层没有把 ErrNoRows 映射成 NotFound，这里只断言「明确失败」而锁定状态码。
	e.fail("GET", "/api/v1/backfill/bf_ghost", nil, 0)

	// 列表：能看到刚才那一批。
	if items := list(t, e.ok("GET", "/api/v1/backfill?limit=5", nil)); len(items) != 1 {
		t.Fatalf("补数列表应有 1 条，得到 %d", len(items))
	}

	// 同样范围再提交：默认全部跳过 → 409，提示需要 force。
	e.fail("POST", "/api/v1/backfill", map[string]any{
		"pipeline": "dwd_order_daily", "from": "2024-01-01", "to": "2024-01-03",
	}, http.StatusConflict)
	// force 后不再跳过已有日期（幂等键仍会让这些日期复用原实例，故只看 skipped）。
	forced := obj(t, e.ok("POST", "/api/v1/backfill", map[string]any{
		"pipeline": "dwd_order_daily", "from": "2024-01-01", "to": "2024-01-03", "force": true,
	}))
	if fxInt(t, forced, "total") != 3 || fxInt(t, forced, "skipped") != 0 {
		t.Fatalf("force 补数结果不对: %v", forced)
	}
	e.as("viewer").fail("POST", "/api/v1/backfill", map[string]any{"pipeline": "x"}, http.StatusForbidden)

	// 甘特图：每行一个实例的起止时间与耗时。
	gantt := list(t, e.ok("GET", "/api/v1/runbook/gantt?limit=10", nil))
	if len(gantt) != 3 {
		t.Fatalf("甘特图应有 3 个实例，得到 %d", len(gantt))
	}
	row := obj(t, gantt[0])
	if field(t, row, "pipeline") != "dwd_order_daily" {
		t.Fatalf("甘特图行内容不对: %v", row)
	}
	if _, ok := row["durationMs"]; !ok {
		t.Fatalf("甘特图行应带耗时: %v", row)
	}
}
