package api_test

import (
	"context"
	"net/http"
	"testing"
)

// 环境绑定解决「同一模型在 dev/prod 指向不同库」：绑定表是唯一的可变点，
// 所以接口必须回显别名→数据源名，而不是内部 ID。
func TestEnvironmentBinding(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")

	// 首次访问自动创建 dev/prod，幂等。
	envs := list(t, e.ok("GET", "/api/v1/environments", nil))
	if len(envs) != 2 {
		t.Fatalf("应有 dev/prod 两个环境，得到 %d", len(envs))
	}
	if again := list(t, e.ok("GET", "/api/v1/environments", nil)); len(again) != 2 {
		t.Fatalf("重复初始化不应新增环境，得到 %d", len(again))
	}

	// 缺字段：400；数据源不存在：404。
	e.fail("POST", "/api/v1/environments/bind", map[string]any{"env": "prod", "alias": "pg_dw"}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/environments/bind", map[string]any{
		"env": "prod", "alias": "pg_dw", "datasource": "ghost",
	}, http.StatusNotFound)
	e.as("viewer").fail("POST", "/api/v1/environments/bind", map[string]any{
		"env": "prod", "alias": "pg_dw", "datasource": ds,
	}, http.StatusForbidden)

	bound := obj(t, e.ok("POST", "/api/v1/environments/bind", map[string]any{
		"env": "prod", "alias": "pg_dw", "datasource": ds,
	}))
	if field(t, bound, "alias") != "pg_dw" || field(t, bound, "datasource") != ds {
		t.Fatalf("绑定响应不对: %v", bound)
	}

	// 绑定列表按环境过滤，值必须是数据源名而不是 ID。
	bindings := obj(t, e.ok("GET", "/api/v1/environments/bindings?env=prod", nil))
	if field(t, bindings, "pg_dw") != ds {
		t.Fatalf("绑定列表不对: %v", bindings)
	}
	if dev := obj(t, e.ok("GET", "/api/v1/environments/bindings?env=dev", nil)); len(dev) != 0 {
		t.Fatalf("dev 环境不应有绑定: %v", dev)
	}

	// 重复绑定同一别名是覆盖而不是报错。
	e.ok("POST", "/api/v1/environments/bind", map[string]any{
		"env": "prod", "alias": "pg_dw", "datasource": ds,
	})
	if got := obj(t, e.ok("GET", "/api/v1/environments/bindings?env=prod", nil)); len(got) != 1 {
		t.Fatalf("重复绑定应保持 1 条，得到 %d", len(got))
	}
}

// 结构变更检测的价值在「对比上一次」：第一次采集只是建档，
// 只有两次之间增减列才是漂移，必须被记录并可查。
func TestCollectWithDiffAndSchemaChanges(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	// 缺 datasource 与未知数据源。
	e.fail("POST", "/api/v1/catalog/collect", map[string]any{}, http.StatusBadRequest)
	e.fail("POST", "/api/v1/catalog/collect", map[string]any{"datasource": "ghost"}, http.StatusNotFound)

	// 第一次采集：新表不算变更。
	first := obj(t, e.ok("POST", "/api/v1/catalog/collect", map[string]any{"datasource": ds}))
	if fxInt(t, first, "tables") < 1 {
		t.Fatalf("首次采集应有表: %v", first)
	}
	// 第一次采集：新表不算变更（changes 为空，序列化成 null）。
	if c := first["changes"]; c != nil && len(list(t, c)) != 0 {
		t.Fatalf("首次采集不应产生变更: %v", c)
	}

	// 手工加一列，模拟上游 DDL 漂移。
	if _, err := e.svc.Repo.DB.Pool.Exec(context.Background(),
		`ALTER TABLE `+quoteIdent(schema)+`.`+quoteIdent("dwd_order")+` ADD COLUMN note text`); err != nil {
		t.Fatalf("变更源表失败: %v", err)
	}

	second := obj(t, e.ok("POST", "/api/v1/catalog/collect", map[string]any{"datasource": ds}))
	changes := list(t, second["changes"])
	if len(changes) != 1 {
		t.Fatalf("应检出 1 条变更，得到 %d", len(changes))
	}
	c := obj(t, changes[0])
	if field(t, c, "kind") != "add_column" || field(t, c, "column") != "note" || fxBool(t, c, "breaking") {
		t.Fatalf("变更内容不对: %v", c)
	}

	// 变更落库后可查。
	stored := list(t, e.ok("GET", "/api/v1/schema-changes?limit=10", nil))
	if len(stored) != 1 {
		t.Fatalf("结构变更列表应有 1 条，得到 %d", len(stored))
	}
	if s := obj(t, stored[0]); field(t, s, "column") != "note" {
		t.Fatalf("结构变更记录不对: %v", s)
	}
}

// 敏感识别是两层判定（列名 + 值采样），接口要给出分级统计与逐条理由，
// 并且「只识别不落库」与「显式采纳」是两条独立路径。
func TestPIIDetectAndApply(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)

	// 一张带邮箱/手机号的用户表。
	userModel := `kind: Model
name: dwd_user
layer: dwd
description: 用户（测试）
datasource: ` + ds + `
schema: ` + schema + `
columns:
  - name: user_id
    type: bigint
    nullable: false
  - name: email
    type: string
    nullable: true
  - name: phone
    type: string
    nullable: true
  - name: dt
    type: date
    nullable: false
primary_key: [user_id]
`
	fxApplyModel(t, e, userModel, true, true)
	e.ok("POST", "/api/v1/datasources/"+ds+"/collect", nil)

	res := obj(t, e.ok("POST", "/api/v1/pii/detect", map[string]any{"datasource": ds, "sampleLimit": 20}))
	items := list(t, res["items"])
	if len(items) < 2 {
		t.Fatalf("应识别出邮箱与手机号，得到 %d 条: %v", len(items), res)
	}
	levels := fxMap(t, res, "byLevel")
	if fxInt(t, levels, "medium") < 2 {
		t.Fatalf("邮箱/手机号应判为 medium: %v", res)
	}
	var emailFinding map[string]any
	for _, it := range items {
		item := obj(t, it)
		if field(t, item, "column") == "email" {
			emailFinding = item
		}
	}
	if emailFinding == nil || field(t, emailFinding, "reason") == "" {
		t.Fatalf("邮箱列应带判定理由: %v", res)
	}

	// 只识别不落库：此时 dataset 还没有 pii 级别。
	before := obj(t, e.ok("GET", "/api/v1/datasets?pageSize=200", nil))
	for _, d := range list(t, before["items"]) {
		item := obj(t, d)
		if field(t, item, "name") == "dwd_user" && item["pii"] != nil && item["pii"] != "none" {
			t.Fatalf("detect 不应写库: %v", item)
		}
	}

	// 采纳：把识别结果写入 dataset。
	e.as("viewer").fail("POST", "/api/v1/pii/apply", map[string]any{
		"datasource": ds, "findings": items,
	}, http.StatusForbidden)
	applied := obj(t, e.ok("POST", "/api/v1/pii/apply", map[string]any{"datasource": ds, "findings": items}))
	if fxInt(t, applied, "updated") != len(items) {
		t.Fatalf("采纳条数不对: %v", applied)
	}

	// 落库后数据集上能看到级别。
	after := obj(t, e.ok("GET", "/api/v1/datasets?pageSize=200", nil))
	found := false
	for _, d := range list(t, after["items"]) {
		item := obj(t, d)
		if field(t, item, "name") == "dwd_user" && field(t, item, "pii") == "medium" {
			found = true
		}
	}
	if !found {
		t.Fatalf("采纳后数据集应标记 medium: %v", after)
	}

	// 空 findings 是合法的「什么都不做」。
	if n := fxInt(t, obj(t, e.ok("POST", "/api/v1/pii/apply", map[string]any{
		"datasource": ds, "findings": []any{},
	})), "updated"); n != 0 {
		t.Fatalf("空 findings 应为 0，得到 %d", n)
	}
}
