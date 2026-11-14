package api_test

import (
	"context"
	"fmt"
	"testing"
)

// 本文件放 HTTP 用例共用的夹具构造器。
// 名字统一加 fx 前缀，避免与 harness_test.go 的通用断言重名。

// fxModelYAML 生成一个能通过建模规范（dwd_ 前缀、必填 dt、声明主键）的模型。
// layer 固定 dwd：它同时覆盖「必填列 + 粒度声明」两类规则，比 ods 更能验证规则链。
func fxModelYAML(name, schema, ds string) string {
	return fmt.Sprintf(`kind: Model
name: %s
layer: dwd
description: 订单明细（测试）
owner: qa
datasource: %s
schema: %s
columns:
  - name: order_id
    type: bigint
    nullable: false
  - name: amount
    type: decimal
    nullable: true
  - name: status
    type: string
    nullable: true
  - name: dt
    type: date
    nullable: false
primary_key: [order_id]
partition_by: [dt]
`, name, ds, schema)
}

// fxPipelineYAML 生成一条「只读源表 → 写模型」的两节点管道。
// 源表与目标模型都落在同一个测试 schema，dry-run / sample-run 都能直接跑。
func fxPipelineYAML(name, ds, model, schema string) string {
	return fmt.Sprintf(`kind: Pipeline
name: %s
description: 订单日更（测试）
schedule: "0 2 * * *"
timezone: Asia/Shanghai
params:
  - name: biz_date
    type: date
    required: true
nodes:
  - id: src
    type: source
    datasource: %s
    sql: |
      SELECT order_id, amount, status, dt FROM %s.%s WHERE dt = ${biz_date}
  - id: load
    type: sql
    name: 写入明细
    depends_on: [src]
    datasource: %s
    model: %s
    write_mode: upsert
    sql: |
      SELECT order_id, amount, status, dt FROM %s.%s WHERE dt = ${biz_date}
`, name, ds, schema, model, ds, model, schema, model)
}

// fxApplyModel 通过 HTTP 落一个模型，返回响应对象。
func fxApplyModel(t *testing.T, e *testEnv, yaml string, publish, apply bool) map[string]any {
	t.Helper()
	return obj(t, e.ok("POST", "/api/v1/models", map[string]any{
		"yaml": yaml, "publish": publish, "apply": apply,
	}))
}

// fxApplyPipeline 通过 HTTP 落一条管道，返回响应对象。
func fxApplyPipeline(t *testing.T, e *testEnv, yaml string, publish bool) map[string]any {
	t.Helper()
	return obj(t, e.ok("POST", "/api/v1/pipelines", map[string]any{
		"yaml": yaml, "publish": publish,
	}))
}

// fxSeedOrders 直接往源表写几行业务数据。
// 指标/看板/采样/画像都需要真实数据才能断言「有值」，空表只能证明不报错。
func fxSeedOrders(t *testing.T, e *testEnv, schema, table string, dates ...string) {
	t.Helper()
	ctx := context.Background()
	for i, d := range dates {
		_, err := e.svc.Repo.DB.Pool.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s.%s (order_id, amount, status, dt) VALUES ($1,$2,$3,$4)`,
				quoteIdent(schema), quoteIdent(table)),
			int64(1000+i), float64(10*(i+1)), "paid", d)
		if err != nil {
			t.Fatalf("写入测试数据失败: %v", err)
		}
	}
}

// quoteIdent 把标识符加双引号，避免保留字与大小写问题。
func quoteIdent(s string) string { return `"` + s + `"` }

// fxInt 读取对象里的数字字段（JSON 解码后是 float64）。
func fxInt(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("字段 %s 不存在: %v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %T", key, v)
	}
	return int(f)
}

// fxBool 读取对象里的布尔字段。
func fxBool(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("字段 %s 不存在: %v", key, m)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("字段 %s 不是布尔: %T", key, v)
	}
	return b
}

// fxMap 读取对象里的子对象字段。
func fxMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("字段 %s 不存在: %v", key, m)
	}
	return obj(t, v)
}

// fxFirstInstance 触发一次管道并返回实例 id，减少每个用例重复三行。
func fxFirstInstance(t *testing.T, e *testEnv, pipeline, bizDate string) string {
	t.Helper()
	data := obj(t, e.ok("POST", "/api/v1/pipelines/"+pipeline+"/trigger", map[string]any{"bizDate": bizDate}))
	inst := obj(t, data["instance"])
	return field(t, inst, "id")
}

// fxReadyModel 是「数据源 + 源 schema + 已发布模型」的通用前置。
// 返回模型名与源 schema，绝大多数用例都从这里开始。
func fxReadyModel(t *testing.T, e *testEnv, modelName string) (model, schema string) {
	t.Helper()
	ds := e.pgDatasource("pgmain")
	schema = e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML(modelName, schema, ds), true, true)
	return modelName, schema
}
