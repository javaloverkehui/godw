package api_test

import (
	"net/http"
	"testing"
)

// 数据源是整条链路的第一环：连不上、名字冲突、凭证回显、采集结果落库，
// 任何一处错都会让后续模型/管道用例失真，所以先把它单独踩实。
func TestDatasourceLifecycle(t *testing.T) {
	e := newEnv(t)

	// 类型清单来自 connector 注册表，前端靠它渲染新建表单。
	types := obj(t, e.ok("GET", "/api/v1/datasource-types", nil))
	if _, ok := types["postgres"]; !ok {
		t.Fatalf("类型清单应包含 postgres: %v", types)
	}

	// 初始为空：新工作空间不该自带数据源。
	if got := list(t, e.ok("GET", "/api/v1/datasources", nil)); len(got) != 0 {
		t.Fatalf("初始数据源应为空，得到 %d", len(got))
	}

	// name/type 缺失在 service 里拦成 400，不进入连接探测。
	if msg := e.fail("POST", "/api/v1/datasources", map[string]any{
		"name": "", "type": "postgres",
	}, http.StatusBadRequest); msg == "" {
		t.Fatal("缺少 name 应返回可读错误")
	}

	// viewer 不能建数据源。
	e.as("viewer").fail("POST", "/api/v1/datasources", map[string]any{
		"name": "pg_ro", "type": "postgres",
	}, http.StatusForbidden)

	name := e.pgDatasource("pgmain")

	items := list(t, e.ok("GET", "/api/v1/datasources", nil))
	if len(items) != 1 {
		t.Fatalf("应有 1 个数据源，得到 %d", len(items))
	}
	ds := obj(t, items[0])
	if field(t, ds, "name") != name || field(t, ds, "status") != "ok" {
		t.Fatalf("数据源状态不对: %v", ds)
	}
	// 凭证不允许出现在列表响应里：这里断言的是「即使 config 里带了 password 也会被剔除」。
	if cfg := fxMap(t, ds, "config"); cfg["password"] != nil {
		t.Fatalf("列表不应回显密码: %v", cfg)
	}

	// 重名：唯一索引转成 409，而不是 500。
	e.fail("POST", "/api/v1/datasources", map[string]any{
		"name": name, "type": "postgres",
		"config": map[string]any{"host": "127.0.0.1", "port": "5432", "user": "godw", "database": "godw", "sslmode": "disable"},
		"secret": map[string]string{"password": "godw"},
	}, http.StatusConflict)

	// 连通性探测成功。
	if data := obj(t, e.ok("POST", "/api/v1/datasources/"+name+"/test", nil)); field(t, data, "status") != "ok" {
		t.Fatalf("连接测试应返回 ok: %v", data)
	}
	// 不存在的数据源是 404，而不是笼统的 500。
	e.fail("POST", "/api/v1/datasources/ghost/test", nil, http.StatusNotFound)

	// 删除后再删一次必须 404：区分「删掉了」和「根本没这个」。
	e.ok("DELETE", "/api/v1/datasources/"+name, nil)
	if got := list(t, e.ok("GET", "/api/v1/datasources", nil)); len(got) != 0 {
		t.Fatalf("删除后应为空，得到 %d", len(got))
	}
	e.fail("DELETE", "/api/v1/datasources/"+name, nil, http.StatusNotFound)

	// 没有任何数据源时采集也是 404。
	e.fail("POST", "/api/v1/datasources/"+name+"/collect", nil, http.StatusNotFound)
}

// 采集与数据集列表是一对：采集把库表写进 catalog，列表把它们读出来。
// 同时验证分页包装与 datasourceId 过滤。
func TestDatasourceCollectAndDatasets(t *testing.T) {
	e := newEnv(t)
	_, schema := fxReadyModel(t, e, "dwd_order")

	// 采集会扫全库；只要测试 schema 里有表，结果就不该是 0。
	data := obj(t, e.ok("POST", "/api/v1/datasources/pgmain/collect", nil))
	if n := fxInt(t, data, "tables"); n < 1 {
		t.Fatalf("采集到的表数应 >=1，得到 %d", n)
	}

	page := obj(t, e.ok("GET", "/api/v1/datasets?page=1&pageSize=2", nil))
	if fxInt(t, page, "total") < 1 || fxInt(t, page, "pageSize") != 2 {
		t.Fatalf("数据集分页不对: %v", page)
	}
	rows := list(t, page["items"])
	if len(rows) > 2 {
		t.Fatalf("pageSize=2 最多返回 2 条，得到 %d", len(rows))
	}

	// datasourceId 过滤：随便给个不存在的 id 就应该是空集，而不是全量。
	empty := obj(t, e.ok("GET", "/api/v1/datasets?datasourceId=ds_nope", nil))
	if fxInt(t, empty, "total") != 0 {
		t.Fatalf("不存在的 datasourceId 应返回空集: %v", empty)
	}

	// pageSize 上限 200、下限 20：这里验证越界参数被夹紧而不是报错。
	zero := obj(t, e.ok("GET", "/api/v1/datasets?page=0&pageSize=0", nil))
	if fxInt(t, zero, "page") != 1 || fxInt(t, zero, "pageSize") < 1 {
		t.Fatalf("越界分页参数应被夹紧: %v", zero)
	}
	_ = schema
}

// readyz 与 overview 是运维入口，必须给出真实计数而不是写死的常量。
func TestReadyzAndOverview(t *testing.T) {
	e := newEnv(t)

	ready := obj(t, e.ok("GET", "/readyz", nil))
	if field(t, ready, "status") != "ready" {
		t.Fatalf("readyz 状态不对: %v", ready)
	}
	if field(t, ready, "schemaVersion") == "" || fxInt(t, ready, "migrations") < 1 {
		t.Fatalf("readyz 应带迁移信息: %v", ready)
	}

	// 空工作空间：计数全 0，instances 是 [] 不是 null。
	ov := obj(t, e.ok("GET", "/api/v1/overview", nil))
	for _, k := range []string{"datasources", "models", "pipelines", "datasets"} {
		if fxInt(t, ov, k) != 0 {
			t.Fatalf("空空间 %s 应为 0: %v", k, ov)
		}
	}
	if insts := list(t, ov["instances"]); len(insts) != 0 {
		t.Fatalf("空空间 instances 应为空，得到 %d", len(insts))
	}
	if by := fxMap(t, ov, "byStatus"); len(by) != 0 {
		t.Fatalf("空空间 byStatus 应为空: %v", by)
	}

	// 建齐数据源 / 模型 / 管道 / 实例后，计数与状态分布都要跟着变。
	_, schema := fxReadyModel(t, e, "dwd_order")
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", "pgmain", "dwd_order", schema), true)
	fxFirstInstance(t, e, "dwd_order_daily", "2024-01-01")

	ov = obj(t, e.ok("GET", "/api/v1/overview", nil))
	if fxInt(t, ov, "datasources") != 1 || fxInt(t, ov, "models") != 1 || fxInt(t, ov, "pipelines") != 1 {
		t.Fatalf("计数未随创建更新: %v", ov)
	}
	if insts := list(t, ov["instances"]); len(insts) != 1 {
		t.Fatalf("应统计到 1 个实例，得到 %d", len(insts))
	}
	by := fxMap(t, ov, "byStatus")
	if len(by) != 1 {
		t.Fatalf("byStatus 应只有一种状态: %v", by)
	}
}
