package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

// CheckModel 是设计器保存前的最后一道自检：规范、类型、变更影响一次返回。
func TestCheckModel(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_check", "dw", "dwd_check")

	res, err := svc.CheckModel(ctxBG(), ws.ID, spec)
	if err != nil {
		t.Fatalf("CheckModel 失败: %v", err)
	}
	if !res.Valid || len(res.Violations) != 0 {
		t.Fatalf("合法模型不应有违规: %+v", res)
	}
	if res.Diff != nil {
		t.Fatalf("未发布过的模型不应有 diff: %+v", res.Diff)
	}

	// 类型不受支持必须报 type_syntax，而不是等到建表才炸。
	bad := modelSpec(ds.Name, "dwd_check_bad", "dw", "dwd_check_bad")
	bad.Columns[1].Type = "varchar(10)"
	res, err = svc.CheckModel(ctxBG(), ws.ID, bad)
	if err != nil {
		t.Fatalf("CheckModel 失败: %v", err)
	}
	if res.Valid {
		t.Fatalf("非法类型应判为不合法: %+v", res)
	}
	ok := false
	for _, v := range res.Violations {
		if v.Rule == "type_syntax" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("应给出 type_syntax 违规: %+v", res.Violations)
	}

	// 与已发布版本对比：删列是破坏性变更，CheckModel 必须把 diff 带回来。
	publishModel(t, svc, ws.ID, spec)
	next := spec
	next.Columns = next.Columns[:2]
	res, err = svc.CheckModel(ctxBG(), ws.ID, next)
	if err != nil {
		t.Fatalf("CheckModel 失败: %v", err)
	}
	if res.Diff == nil || !res.Diff.HasBreaking() {
		t.Fatalf("应识别出破坏性变更: %+v", res.Diff)
	}
}

// 脚手架的价值是「不用凭空手写列」：源表列要归一成合法类型，主键候选要说清楚。
func TestScaffoldModel(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	if _, err := svc.ScaffoldModel(ctxBG(), ws.ID, modelc.ScaffoldOptions{Layer: "dwd"}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 datasource 应报参数错误，实际 %v", err)
	}
	res, err := svc.ScaffoldModel(ctxBG(), ws.ID, modelc.ScaffoldOptions{
		Layer: "dwd", Name: "order", Datasource: ds.Name, Schema: "dw",
		Upstream: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint", PrimaryKey: true},
			{Name: "dt", Type: "date"},
			{Name: "amount", Type: "numeric(18,2)"},
		},
	})
	if err != nil {
		t.Fatalf("ScaffoldModel 失败: %v", err)
	}
	// 自动补层前缀，避免生成一个注定不合规的名字。
	if res.Spec.Name != "dwd_order" || res.Spec.Layer != "dwd" {
		t.Fatalf("模型名应补层前缀: %+v", res.Spec)
	}
	if !hasStr(res.KeyCandidates, "order_id") {
		t.Fatalf("源表主键应成为候选: %+v", res.KeyCandidates)
	}
	if !strings.Contains(res.YAML, "dwd_order") {
		t.Fatalf("应输出可保存的 YAML: %s", res.YAML)
	}
	if _, err := svc.ScaffoldModel(ctxBG(), ws.ID, modelc.ScaffoldOptions{
		Layer: "bad", Name: "x", Datasource: ds.Name,
	}); err == nil {
		t.Fatal("非法分层应报错")
	}
}

// 结构对比与回滚预览：迁移方案要能被前端直接展示，回滚必须指出版本不存在。
func TestModelDiffAndRollbackPreview(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	v1 := modelSpec(ds.Name, "dwd_ver", "dw", "dwd_ver")
	publishModel(t, svc, ws.ID, v1)
	v2 := v1
	v2.Columns = append(v2.Columns, model.ColumnSpec{Name: "extra", Type: "string", Nullable: true})
	publishModel(t, svc, ws.ID, v2)

	// 当前发布版本是 v2，拿 v1 来比对：应当是「删列」这类破坏性变更。
	out, err := svc.ModelDiff(ctxBG(), ws.ID, v1)
	if err != nil {
		t.Fatalf("ModelDiff 失败: %v", err)
	}
	if out["engine"] != "postgres" {
		t.Fatalf("引擎应来自数据源: %+v", out["engine"])
	}
	diff, ok := out["diff"].(modelc.DiffResult)
	if !ok {
		t.Fatalf("diff 类型不对: %T", out["diff"])
	}
	if len(diff.Changes) == 0 {
		t.Fatalf("同版本比对不应为空: %+v", diff)
	}
	plan, ok := out["plan"].(modelc.MigrationPlan)
	if !ok || len(plan.Statements)+len(plan.Manual) == 0 {
		t.Fatalf("应给出迁移方案: %T", out["plan"])
	}

	// 回滚到 v1：从当前（v2，多一列）回滚意味着删列，属于破坏性变更。
	rb, err := svc.RollbackPreview(ctxBG(), ws.ID, "dwd_ver", 1)
	if err != nil {
		t.Fatalf("RollbackPreview 失败: %v", err)
	}
	if !rb.HasBreaking() {
		t.Fatalf("回滚删列应标记破坏性: %+v", rb)
	}
	if _, err := svc.RollbackPreview(ctxBG(), ws.ID, "dwd_ver", 99); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的版本应 NotFound，实际 %v", err)
	}
	if _, err := svc.RollbackPreview(ctxBG(), ws.ID, "ghost", 1); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的模型应 NotFound，实际 %v", err)
	}
}

// 数据字典要带上下游与脱敏标记，Markdown 要能直接入库。
func TestDictionaryAndMarkdown(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_dict", "dw", "dwd_dict")
	spec.Owner = "alice"
	spec.Tags = []string{"core"}
	spec.Description = "订单明细"
	publishModel(t, svc, ws.ID, spec)
	if err := svc.Repo.ReplaceLineage(ctxBG(), ws.ID, "dwd_dict", []string{"ods_src"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_dict", ColumnName: "amount", Classification: "confidential", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}

	entries, err := svc.Dictionary(ctxBG(), ws.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("字典应有一条: %+v %v", entries, err)
	}
	e := entries[0]
	if e.Name != "dwd_dict" || e.Owner != "alice" || e.Description != "订单明细" {
		t.Fatalf("基础字段不对: %+v", e)
	}
	if !hasStr(e.Upstream, "ods_src") {
		t.Fatalf("应带上游: %+v", e.Upstream)
	}
	var masked bool
	for _, c := range e.Columns {
		if c.Name == "amount" {
			if c.Classification != "confidential" || c.Mask != "full" {
				t.Fatalf("列分级/脱敏应写进字典: %+v", c)
			}
			if c.Physical == "" {
				t.Fatalf("应给出物理类型: %+v", c)
			}
			masked = true
		}
	}
	if !masked {
		t.Fatalf("缺少 amount 列: %+v", e.Columns)
	}

	md, err := svc.DictionaryMarkdown(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("DictionaryMarkdown 失败: %v", err)
	}
	if !strings.HasPrefix(md, "# 数据字典") || !strings.Contains(md, "dwd_dict") {
		t.Fatalf("Markdown 内容不对: %s", md)
	}

	rules, err := svc.LayerRules(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读取分层规范失败: %v", err)
	}
	if rules["immutableRule"] != "reverse_dependency" {
		t.Fatalf("应返回不可变规则说明: %+v", rules)
	}
	layers, _ := rules["layers"].(map[string]any)
	if len(layers) != 5 {
		t.Fatalf("默认应有 5 个分层规则: %+v", layers)
	}
}
