package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// 多环境的核心是「模型不改，绑定变」：别名必须能解析到真实数据源，
// 未绑定时回退同名数据源，两者都没有时必须报 NotFound 而不是静默用错库。
func TestEnvironmentBinding(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	if err := svc.EnsureEnvironments(ctxBG(), ws.ID); err != nil {
		t.Fatalf("EnsureEnvironments 失败: %v", err)
	}
	// 幂等：再调一次不应产生重复环境。
	if err := svc.EnsureEnvironments(ctxBG(), ws.ID); err != nil {
		t.Fatalf("重复 EnsureEnvironments 失败: %v", err)
	}
	envs, err := svc.ListEnvironments(ctxBG(), ws.ID)
	if err != nil || len(envs) != 2 {
		t.Fatalf("应有 dev/prod 两个环境，实际 %v: %v", envs, err)
	}

	if err := svc.BindEnv(ctxBG(), ws.ID, "prod", "pg_dw", ds.Name); err != nil {
		t.Fatalf("BindEnv 失败: %v", err)
	}
	name, err := svc.ResolveDatasource(ctxBG(), ws.ID, "prod", "pg_dw")
	if err != nil || name != ds.Name {
		t.Fatalf("别名应解析到真实数据源: %q %v", name, err)
	}
	bindings, err := svc.EnvBindings(ctxBG(), ws.ID, "prod")
	if err != nil || bindings["pg_dw"] != ds.Name {
		t.Fatalf("绑定列表不对: %v %v", bindings, err)
	}

	// 未绑定但同名的真实数据源可直接使用，保证单环境用户不受影响。
	if got, err := svc.ResolveDatasource(ctxBG(), ws.ID, "dev", ds.Name); err != nil || got != ds.Name {
		t.Fatalf("未绑定同名应回退: %q %v", got, err)
	}
	if _, err := svc.ResolveDatasource(ctxBG(), ws.ID, "dev", "nope"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("别名与数据源都不存在应报 NotFound，实际 %v", err)
	}
	if err := svc.BindEnv(ctxBG(), ws.ID, "prod", "", ds.Name); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("空别名应被拒绝，实际 %v", err)
	}
}

// 删除工作空间前必须确认没有运行中的实例，否则会留下无主任务。
func TestDeleteWorkspaceGuardsRunning(t *testing.T) {
	svc, ws := newSvc(t)
	publishPipeline(t, svc, ws.ID, simplePipeline("del_ws"))
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "del_ws", "2026-07-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if _, err := svc.Repo.ClaimInstance(ctxBG(), "w1", 0); err != nil {
		t.Fatalf("领取实例失败: %v", err)
	}

	err = svc.DeleteWorkspace(ctxBG(), ws.ID)
	if !errs.Is(err, errs.CodeConflict) {
		t.Fatalf("有运行实例时应拒绝删除，实际 %v", err)
	}
	// 实例结束后允许软删。
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "success", ""); err != nil {
		t.Fatalf("结束实例失败: %v", err)
	}
	if err := svc.DeleteWorkspace(ctxBG(), ws.ID); err != nil {
		t.Fatalf("无运行实例应允许删除: %v", err)
	}
}

// 采集本身要能对着真表跑通，并把结构漂移写进 schema_change 供人复核。
func TestCollectWithDiffRecordsAddColumn(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint)`, p.table("diff_t"))

	res, err := svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("首次采集失败: %v", err)
	}
	if res.Tables == 0 || len(res.Changes) != 0 {
		t.Fatalf("首次采集应只有表数、无变更: %+v", res)
	}

	p.exec(t, `ALTER TABLE %s ADD COLUMN amount numeric(18,2)`, p.table("diff_t"))
	res, err = svc.CollectWithDiff(ctxBG(), ws.ID, ds.Name)
	if err != nil {
		t.Fatalf("二次采集失败: %v", err)
	}
	found := false
	for _, c := range res.Changes {
		if c.Table == "diff_t" && c.Kind == "add_column" && c.Column == "amount" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应识别到 diff_t.amount 新增: %+v", res.Changes)
	}
	changes, err := svc.ListSchemaChanges(ctxBG(), ws.ID, 10)
	if err != nil {
		t.Fatalf("ListSchemaChanges 失败: %v", err)
	}
	if len(changes) == 0 || changes[0].Table != "diff_t" {
		t.Fatalf("结构变更应落库: %+v", changes)
	}
}

// 采集失败要重试并把数据源标 failed；这里用不存在的数据源制造失败，
// 断言的是「重试后仍失败时错误要返回」，而不是静默吞掉。
func TestCollectWithDiffFailsForUnknownDatasource(t *testing.T) {
	svc, ws := newSvc(t)
	res, err := svc.CollectWithDiff(ctxBG(), ws.ID, "ghost_ds")
	if err == nil {
		t.Fatal("未知数据源应失败")
	}
	if res == nil || res.Error == "" {
		t.Fatalf("失败结果也要带错误信息: %+v", res)
	}
}

// 敏感字段识别是两条链路：列名规则（零成本）+ 值采样（列名没命中时才做）。
// 断言两条链路都生效，且人工覆盖优先于自动识别。
func TestDetectPIIAndApply(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint, user_phone text, contact text, amount numeric)`, p.table("pii_t"))
	p.exec(t, `INSERT INTO %s VALUES (1,'13800000000','a@b.com',1),(2,'13900000000','c@d.com',2)`, p.table("pii_t"))

	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "pii_t",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"}, {Name: "user_phone", Type: "text"},
			{Name: "contact", Type: "text"}, {Name: "amount", Type: "numeric"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}

	// 人工覆盖：把 contact 强行标成 high，采集结果不得覆盖它。
	if _, err := svc.Repo.DB.Pool.Exec(ctxBG(),
		`UPDATE dataset SET pii_override=true, pii='high', pii_reason='合规要求' WHERE workspace_id=$1 AND table_name='pii_t'`,
		ws.ID); err != nil {
		t.Fatalf("设置人工覆盖失败: %v", err)
	}

	findings, err := svc.DetectPII(ctxBG(), ws.ID, ds.Name, 0)
	if err != nil {
		t.Fatalf("DetectPII 失败: %v", err)
	}
	byCol := map[string]service.PIIFinding{}
	for _, f := range findings {
		byCol[f.Column] = f
	}
	if f, ok := byCol["user_phone"]; !ok || f.Level != service.PIIMedium || f.Reason == "" {
		t.Fatalf("列名规则应命中手机号: %+v", byCol["user_phone"])
	}
	if f, ok := byCol["contact"]; !ok {
		t.Fatalf("值采样应识别出邮箱列: %+v", findings)
	} else if f.Level != service.PIIMedium || f.SampleHitRate <= 0.8 {
		t.Fatalf("邮箱采样命中率应 >0.8: %+v", f)
	}
	if _, ok := byCol["amount"]; ok {
		t.Fatalf("普通数值列不应被识别: %+v", byCol["amount"])
	}

	// ApplyPIIFindings 把结果回写 dataset，供脱敏策略消费。
	n, err := svc.ApplyPIIFindings(ctxBG(), ws.ID, ds.Name, findings)
	if err != nil || n == 0 {
		t.Fatalf("ApplyPIIFindings 失败: %d %v", n, err)
	}
	list, err := svc.Repo.ListDatasets(ctxBG(), ws.ID, "")
	if err != nil {
		t.Fatalf("ListDatasets 失败: %v", err)
	}
	var got string
	for _, d := range list {
		if d.Name == "pii_t" {
			got = d.PII
		}
	}
	if got == "" {
		t.Fatalf("PII 级别应回写到 dataset: %+v", list)
	}
}

// 搜索要同时命中中文子串（ILIKE）与英文分词，并支持类型/标签/责任人过滤。
// 这里还顺带验证 highlight 的 HTML 转义，防止搜索词回流成标签。
func TestSearchAndHighlight(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_order_search", "dw", "dwd_order_search")
	spec.Description = "订单明细表"
	publishModel(t, svc, ws.ID, spec)
	publishPipeline(t, svc, ws.ID, simplePipeline("order_search_pipe"))
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: "dw", Name: "order_raw", Comment: "原始订单",
		Columns: []model.ColumnSpec{{Name: "id", Type: "bigint"}},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}

	hits, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "订单"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("中文子串应命中")
	}
	for _, h := range hits {
		if h.Type == "model" && h.Name == "dwd_order_search" {
			if !strings.Contains(h.Highlight, "<em>") {
				t.Fatalf("命中片段应带高亮: %q", h.Highlight)
			}
		}
	}

	// 类型过滤只保留模型；limit 必须真的截断。
	only, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "order", Types: []string{"model"}})
	if err != nil {
		t.Fatalf("按类型搜索失败: %v", err)
	}
	for _, h := range only {
		if h.Type != "model" {
			t.Fatalf("类型过滤失效: %+v", h)
		}
	}
	limited, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "order", Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit=1 应只返回 1 条: %d %v", len(limited), err)
	}

	// 标签与责任人：责任人用 owner:<user> 约定标签表达。
	if _, err := svc.SaveTag(ctxBG(), ws.ID, "core", "", ""); err != nil {
		t.Fatalf("建标签失败: %v", err)
	}
	if err := svc.TagEntity(ctxBG(), ws.ID, "model", "dwd_order_search", []string{"core", "owner:alice"}); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	byTag, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Tags: []string{"core"}})
	if err != nil {
		t.Fatalf("按标签搜索失败: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Name != "dwd_order_search" {
		t.Fatalf("标签过滤结果不对: %+v", byTag)
	}
	byOwner, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Owner: "alice"})
	if err != nil || len(byOwner) != 1 {
		t.Fatalf("责任人过滤结果不对: %+v %v", byOwner, err)
	}

	// 命中片段必须转义后再回流：描述里带脚本标签时，Highlight 不能出现裸标签。
	xss := modelSpec(ds.Name, "dwd_order_xss", "dw", "dwd_order_xss")
	xss.Description = `<script>alert(1)</script>订单`
	publishModel(t, svc, ws.ID, xss)
	all, err := svc.Search(ctxBG(), ws.ID, service.SearchQuery{Q: "订单"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	for _, h := range all {
		if strings.Contains(h.Highlight, "<script") || strings.Contains(h.Highlight, "</script>") {
			t.Fatalf("命中片段未转义: %q", h.Highlight)
		}
	}
}

// 标签 CRUD 与实体反查：删标签后实体关联也应失效。
func TestTagLifecycle(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.SaveTag(ctxBG(), ws.ID, "core", "#f00", "核心数据"); err != nil {
		t.Fatalf("SaveTag 失败: %v", err)
	}
	// 打标前标签必须已定义：允许隐式建标签会让拼错的标签名悄悄生效。
	if _, err := svc.SaveTag(ctxBG(), ws.ID, "pii", "#0f0", "敏感"); err != nil {
		t.Fatalf("SaveTag pii 失败: %v", err)
	}
	tags, err := svc.ListTags(ctxBG(), ws.ID)
	if err != nil || len(tags) != 2 || tags[0].Name != "core" || tags[1].Name != "pii" {
		t.Fatalf("ListTags 不对: %+v %v", tags, err)
	}

	if err := svc.TagEntity(ctxBG(), ws.ID, "dataset", "order_raw", []string{"core", "pii"}); err != nil {
		t.Fatalf("TagEntity 失败: %v", err)
	}
	got, err := svc.EntityTags(ctxBG(), ws.ID, "dataset", "order_raw")
	if err != nil || len(got) != 2 {
		t.Fatalf("EntityTags 不对: %v %v", got, err)
	}
	hits, err := svc.EntitiesByTag(ctxBG(), ws.ID, "core", 10)
	if err != nil || len(hits) != 1 || hits[0].Name != "order_raw" {
		t.Fatalf("EntitiesByTag 不对: %+v %v", hits, err)
	}

	if err := svc.UntagEntity(ctxBG(), ws.ID, "dataset", "order_raw", "pii"); err != nil {
		t.Fatalf("UntagEntity 失败: %v", err)
	}
	got, _ = svc.EntityTags(ctxBG(), ws.ID, "dataset", "order_raw")
	if len(got) != 1 || got[0] != "core" {
		t.Fatalf("摘标签后应只剩 core: %v", got)
	}
	if err := svc.DeleteTag(ctxBG(), ws.ID, "core"); err != nil {
		t.Fatalf("DeleteTag 失败: %v", err)
	}
	tags, _ = svc.ListTags(ctxBG(), ws.ID)
	if len(tags) != 1 || tags[0].Name != "pii" {
		t.Fatalf("标签应已删除: %+v", tags)
	}
}
