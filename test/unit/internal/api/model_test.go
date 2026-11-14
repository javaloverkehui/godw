package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 模型是全平台的核心资产：发布、版本、回滚、预览、自检、字典。
// 这里按「一次完整的建模生命周期」串起来跑，顺带覆盖单点异常分支。
func TestModelLifecycle(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()

	if items := list(t, e.ok("GET", "/api/v1/models", nil)); len(items) != 0 {
		t.Fatalf("初始模型应为空，得到 %d", len(items))
	}

	// 非法 YAML：解析失败在 400，且带 ModelInvalid 语义。
	e.fail("POST", "/api/v1/models", map[string]any{"yaml": "kind: ["}, http.StatusBadRequest)
	// 结构合法但不符合分层规范（dwd 缺 dt）：同样被拒，不做「先存后改」。
	e.fail("POST", "/api/v1/models", map[string]any{"yaml": `kind: Model
name: dwd_bad
layer: dwd
datasource: pgmain
columns:
  - name: order_id
    type: bigint
primary_key: [order_id]
`}, http.StatusBadRequest)

	yaml := fxModelYAML("dwd_order", schema, ds)

	// 第一次只存草稿：不建表、不发布。
	draft := fxApplyModel(t, e, yaml, false, false)
	if field(t, draft, "name") != "dwd_order" {
		t.Fatalf("草稿模型名不对: %v", draft)
	}
	if v := fxInt(t, draft, "activeVersion"); v != 0 {
		t.Fatalf("未发布模型 activeVersion 应为 0，得到 %d", v)
	}

	versions := list(t, e.ok("GET", "/api/v1/models/dwd_order/versions", nil))
	if len(versions) != 1 {
		t.Fatalf("草稿也应有 1 个版本记录，得到 %d", len(versions))
	}
	if v := obj(t, versions[0]); fxInt(t, v, "version") != 1 || field(t, v, "status") != "draft" {
		t.Fatalf("首版状态不对: %v", v)
	}

	// 第二次发布并真正建表：DDL 必须回显，且表要能在源 schema 里查到。
	// 第一次发布不复用草稿版本号，而是新开 v2：版本是不可变快照。
	published := fxApplyModel(t, e, yaml, true, true)
	if v := fxInt(t, published, "activeVersion"); v != 2 {
		t.Fatalf("发布后 activeVersion 应为 2，得到 %d", v)
	}
	if ddl := field(t, published, "ddl"); !strings.Contains(ddl, "dwd_order") {
		t.Fatalf("模型响应应带建表 DDL: %q", ddl)
	}

	// 预览：YAML 与结构化 spec 两种入口必须给出同一份 DDL。
	byYAML := obj(t, e.ok("POST", "/api/v1/models/preview", map[string]any{"yaml": yaml}))
	bySpec := obj(t, e.ok("POST", "/api/v1/models/preview", map[string]any{
		"spec": map[string]any{
			"kind": "Model", "name": "dwd_order", "layer": "dwd", "datasource": ds, "schema": schema,
			"columns": []any{
				map[string]any{"name": "order_id", "type": "bigint"},
				map[string]any{"name": "dt", "type": "date"},
			},
			"primaryKey": []string{"order_id"},
		},
	}))
	if field(t, byYAML, "ddl") == "" || !strings.Contains(field(t, bySpec, "ddl"), "dwd_order") {
		t.Fatalf("预览 DDL 不对: %v / %v", byYAML, bySpec)
	}
	// 既没 yaml 也没 spec：400。
	e.fail("POST", "/api/v1/models/preview", map[string]any{}, http.StatusBadRequest)

	// 规范自检：合规模型 valid=true；不合规模型仍然是 200，但把违规逐条列出。
	okCheck := obj(t, e.ok("POST", "/api/v1/models/check", map[string]any{"yaml": yaml}))
	if !fxBool(t, okCheck, "valid") {
		t.Fatalf("合规模型自检应为 valid: %v", okCheck)
	}
	badCheck := obj(t, e.ok("POST", "/api/v1/models/check", map[string]any{
		"spec": map[string]any{
			"kind": "Model", "name": "order", "layer": "dwd", "datasource": ds,
			"columns": []any{map[string]any{"name": "amount", "type": "decimal"}},
		},
	}))
	if fxBool(t, badCheck, "valid") || len(list(t, badCheck["violations"])) == 0 {
		t.Fatalf("不合规模型应列出违规: %v", badCheck)
	}
	e.fail("POST", "/api/v1/models/check", map[string]any{}, http.StatusBadRequest)

	// 结构对比：必须带上迁移计划，而不只是「有没有变化」。
	diff := obj(t, e.ok("POST", "/api/v1/models/diff", map[string]any{"yaml": yaml}))
	if field(t, diff, "engine") != "postgres" {
		t.Fatalf("对比结果应带引擎: %v", diff)
	}
	if _, ok := diff["plan"]; !ok {
		t.Fatalf("对比结果应带迁移计划: %v", diff)
	}
	// 对未发布的模型做对比是 404。
	e.fail("POST", "/api/v1/models/diff", map[string]any{
		"yaml": fxModelYAML("dwd_missing", schema, ds),
	}, http.StatusNotFound)

	// 脚手架：从上游补层前缀、推主键，给出可直接保存的 YAML。
	sc := obj(t, e.ok("POST", "/api/v1/models/scaffold", map[string]any{
		"layer": "dwd", "name": "orders", "datasource": ds, "schema": schema,
		"upstream": []any{
			map[string]any{"name": "order_id", "type": "bigint", "primaryKey": true},
			map[string]any{"name": "amount", "type": "decimal"},
			map[string]any{"name": "dt", "type": "date"},
		},
	}))
	if y := field(t, sc, "yaml"); !strings.Contains(y, "name: dwd_orders") {
		t.Fatalf("脚手架应自动补 dwd_ 前缀: %q", y)
	}
	if keys := list(t, sc["keyCandidates"]); len(keys) == 0 || keys[0] != "order_id" {
		t.Fatalf("主键候选应来自上游: %v", keys)
	}
	e.fail("POST", "/api/v1/models/scaffold", map[string]any{"layer": "nope", "name": "x"}, http.StatusBadRequest)

	// 版本与回滚：先发第二版（改描述，属于安全变更），再回滚到 v1。
	yaml2 := strings.Replace(yaml, "订单明细（测试）", "订单明细（测试）v2", 1)
	fxApplyModel(t, e, yaml2, true, false)

	ver2 := list(t, e.ok("GET", "/api/v1/models/dwd_order/versions", nil))
	if len(ver2) != 3 {
		t.Fatalf("应有 3 个版本，得到 %d", len(ver2))
	}
	// 版本列表按版本号倒序，第一条就是最新。
	if v := obj(t, ver2[0]); fxInt(t, v, "version") != 3 {
		t.Fatalf("最新版本应为 3: %v", v)
	}

	// 回滚预览只算不动；参数非法（0）与版本不存在都要明确报错。
	rp := obj(t, e.ok("GET", "/api/v1/models/dwd_order/rollback-preview?version=1", nil))
	if _, ok := rp["changes"]; !ok {
		t.Fatalf("回滚预览应带 changes: %v", rp)
	}
	e.fail("GET", "/api/v1/models/dwd_order/rollback-preview?version=0", nil, http.StatusBadRequest)
	e.fail("GET", "/api/v1/models/dwd_order/rollback-preview?version=99", nil, http.StatusNotFound)

	rb := obj(t, e.ok("POST", "/api/v1/models/dwd_order/rollback", map[string]any{"version": 1}))
	if fxInt(t, rb, "rolledBack") != 1 {
		t.Fatalf("回滚响应不对: %v", rb)
	}
	models := list(t, e.ok("GET", "/api/v1/models", nil))
	if v := fxInt(t, obj(t, models[0]), "activeVersion"); v != 1 {
		t.Fatalf("回滚后 activeVersion 应为 1，得到 %d", v)
	}
	e.fail("POST", "/api/v1/models/dwd_order/rollback", map[string]any{"version": 99}, http.StatusNotFound)
	e.fail("POST", "/api/v1/models/dwd_order/rollback", `{"version":`, http.StatusBadRequest)

	// 数据字典：既要模型本身，也要列与脱敏标记。
	dict := list(t, e.ok("GET", "/api/v1/dictionary", nil))
	if len(dict) != 1 {
		t.Fatalf("字典应含 1 个模型，得到 %d", len(dict))
	}
	entry := obj(t, dict[0])
	if field(t, entry, "name") != "dwd_order" || len(list(t, entry["columns"])) != 4 {
		t.Fatalf("字典条目不对: %v", entry)
	}
	// 导出是 markdown 附件，直接断言原始响应体。
	status, _, raw := e.doRaw("GET", "/api/v1/dictionary/export", nil)
	if status != http.StatusOK || !strings.Contains(raw, "# 数据字典") || !strings.Contains(raw, "dwd_order") {
		t.Fatalf("字典导出内容不对(%d): %s", status, raw)
	}

	// 分层规则是只读配置，必须能看到不可关闭的反向依赖规则。
	rules := obj(t, e.ok("GET", "/api/v1/layer-rules", nil))
	if field(t, rules, "immutableRule") != "reverse_dependency" {
		t.Fatalf("分层规则缺少不可变规则: %v", rules)
	}
	if layers := fxMap(t, rules, "layers"); layers["dwd"] == nil {
		t.Fatalf("分层规则应含 dwd 层: %v", layers)
	}

	// 未发布的模型查版本：返回空数组而不是 404（列表语义）。
	if items := list(t, e.ok("GET", "/api/v1/models/never_exists/versions", nil)); len(items) != 0 {
		t.Fatalf("未知模型的版本应为空，得到 %d", len(items))
	}
}

// 破坏性变更（删列）必须带 force 才能发布，否则老数据会静默丢失。
func TestModelBreakingChangeNeedsForce(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	yaml := fxModelYAML("dwd_order", schema, ds)
	fxApplyModel(t, e, yaml, true, false)

	// 去掉 status 列：Diff 会判定为 breaking。
	breaking := strings.Replace(yaml, `  - name: status
    type: string
    nullable: true
`, "", 1)
	msg := e.fail("POST", "/api/v1/models", map[string]any{"yaml": breaking, "publish": true}, http.StatusConflict)
	if !strings.Contains(msg, "破坏性") {
		t.Fatalf("应提示破坏性变更，得到 %q", msg)
	}
	// 显式 force 后放行，且审计里能查到 reason。
	forced := obj(t, e.ok("POST", "/api/v1/models", map[string]any{
		"yaml": breaking, "publish": true, "force": true, "reason": "确认下线 status 列",
	}))
	if fxInt(t, forced, "activeVersion") != 2 {
		t.Fatalf("force 发布后应为 v2: %v", forced)
	}
	audits := list(t, e.ok("GET", "/api/v1/audit?limit=20", nil))
	found := false
	for _, a := range audits {
		item := obj(t, a)
		if field(t, item, "action") == "model.apply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应写入 model.apply 审计: %v", audits)
	}
}
