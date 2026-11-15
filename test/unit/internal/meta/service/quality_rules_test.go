package service_test

import (
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

// 质量规则从管道 YAML 搬进库之后的核心契约：
// 规则挂在模型上、整套替换、id 稳定、默认启用。
func TestQualityRulesLifecycle(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_q", "dw", "dwd_q"))

	rules, err := svc.QualityRules(ctxBG(), ws.ID, "dwd_q")
	if err != nil || len(rules) != 0 {
		t.Fatalf("新模型应没有规则: %+v %v", rules, err)
	}
	if _, err := svc.QualityRules(ctxBG(), ws.ID, ""); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 model 应报参数错误，实际 %v", err)
	}

	// 规则指向不存在的模型是死配置，保存时就该拒绝。
	if _, err := svc.SaveQualityRules(ctxBG(), ws.ID, "tester", "dwd_ghost",
		[]model.QualityRule{{Name: "x", Kind: "not_null", Column: "id"}}); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("模型不存在应 NotFound，实际 %v", err)
	}

	saved, err := svc.SaveQualityRules(ctxBG(), ws.ID, "tester", "dwd_q", []model.QualityRule{
		{Name: "id_not_null", Kind: "not_null", Column: "id"},
		{Name: "amount_range", Kind: "range", Column: "amount", Severity: "warn",
			Config: map[string]any{"min": float64(0)}},
	})
	if err != nil {
		t.Fatalf("保存规则失败: %v", err)
	}
	if len(saved) != 2 || saved[0].Name != "amount_range" || saved[1].Name != "id_not_null" {
		t.Fatalf("规则应按名称排序返回: %+v", saved)
	}
	// 没显式传 enabled 的规则必须默认启用；severity 缺省为 error。
	if saved[1].Enabled == nil || !*saved[1].Enabled {
		t.Fatalf("未传 enabled 应默认启用: %+v", saved[1])
	}
	if saved[1].Severity != "error" {
		t.Fatalf("severity 缺省应为 error: %+v", saved[1])
	}
	if saved[0].Config["min"] != float64(0) {
		t.Fatalf("config 应原样往返: %+v", saved[0].Config)
	}

	// 文档站按模型展示规则：库里的规则必须出现在对应模型下，
	// 否则「规则搬进库」等于从文档里消失。
	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读取文档站失败: %v", err)
	}
	var documented []string
	for _, e := range page.Entries {
		if e.Name == "dwd_q" {
			for _, q := range e.Quality {
				documented = append(documented, q.Name)
			}
		}
	}
	if len(documented) != 2 {
		t.Fatalf("文档站应展示模型上的 2 条规则: %+v", documented)
	}

	// 整表替换：改阈值 + 删掉另一条，保留的规则 id 不变（结果与豁免靠它关联）。
	keepID := saved[0].ID
	saved, err = svc.SaveQualityRules(ctxBG(), ws.ID, "tester", "dwd_q", []model.QualityRule{
		{Name: "amount_range", Kind: "range", Column: "amount", Config: map[string]any{"min": float64(100)}},
	})
	if err != nil {
		t.Fatalf("更新规则失败: %v", err)
	}
	if len(saved) != 1 || saved[0].ID != keepID {
		t.Fatalf("同名规则应复用 id 并删除多余规则: %+v", saved)
	}
	if saved[0].Config["min"] != float64(100) {
		t.Fatalf("阈值应更新: %+v", saved[0].Config)
	}

	// 规则定义自身不合格时拒绝：缺列、重名、未知类型。
	cases := []struct {
		name  string
		rules []model.QualityRule
	}{
		{"缺列", []model.QualityRule{{Name: "no_col", Kind: "not_null"}}},
		{"重名", []model.QualityRule{{Name: "dup", Kind: "row_count"}, {Name: "dup", Kind: "row_count"}}},
		{"未知类型", []model.QualityRule{{Name: "weird", Kind: "vibes", Column: "id"}}},
	}
	for _, c := range cases {
		if _, err := svc.SaveQualityRules(ctxBG(), ws.ID, "tester", "dwd_q", c.rules); !errs.Is(err, errs.CodeInvalidArgument) {
			t.Fatalf("%s 应被拒绝，实际 %v", c.name, err)
		}
	}
}

// 建模规范按工作区存：命名前缀可改，但上游白名单与必填列只能收紧。
func TestModelingRulesWorkspaceOverride(t *testing.T) {
	svc, ws := newSvc(t)

	defaults, _, err := svc.ModelingRules(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("读取默认规范失败: %v", err)
	}
	if len(defaults.Layers) != 5 {
		t.Fatalf("默认应覆盖 5 层: %+v", defaults.Layers)
	}

	saved, err := svc.SaveModelingRules(ctxBG(), ws.ID, "tester", modelc.Rules{
		Layers: map[string]modelc.LayerRule{
			"dwd": {NamePattern: `^dwx_[a-z][a-z0-9_]*$`},
		},
	})
	if err != nil {
		t.Fatalf("保存规范失败: %v", err)
	}
	spec := modelSpec("pg", "dwx_order", "dw", "dwx_order")
	if vs := saved.Check(spec); len(vs) != 0 {
		t.Fatalf("自定义前缀应通过: %+v", vs)
	}
	if vs := saved.Check(modelSpec("pg", "dwd_order", "dw", "dwd_order")); len(vs) == 0 {
		t.Fatalf("旧前缀不应再匹配")
	}
	// 必填列是收紧项：即便调用方只提交了命名正则，dt 与主键要求也保留。
	dwd := saved.Layers["dwd"]
	if len(dwd.RequiredColumns) == 0 {
		t.Fatalf("必填列不能因覆盖而丢失: %+v", dwd)
	}
	if !dwd.RequireKey {
		t.Fatalf("DWD 的主键要求不能被关掉: %+v", dwd)
	}

	// 放宽上游白名单必须拒绝。
	if _, err := svc.SaveModelingRules(ctxBG(), ws.ID, "tester", modelc.Rules{
		Layers: map[string]modelc.LayerRule{
			"dwd": {AllowUpstream: []string{"ads"}},
		},
	}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("放宽上游应被拒绝，实际 %v", err)
	}

	// 重置后回到内置默认。
	if err := svc.ResetModelingRules(ctxBG(), ws.ID, "tester"); err != nil {
		t.Fatalf("重置规范失败: %v", err)
	}
	again, _, err := svc.ModelingRules(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("重读规范失败: %v", err)
	}
	if again.Layers["dwd"].NamePattern != defaults.Layers["dwd"].NamePattern {
		t.Fatalf("重置后应回到默认: %+v", again.Layers["dwd"])
	}
}
