package modelc_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

func ruleSpec(layer, name string) model.ModelSpec {
	return model.ModelSpec{
		Name:  name,
		Layer: layer,
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"},
			{Name: "dt", Type: "date"},
		},
		PrimaryKey: []string{"id"},
	}
}

func violations(spec model.ModelSpec) map[string]int {
	out := map[string]int{}
	for _, v := range modelc.Check(spec) {
		out[v.Rule]++
	}
	return out
}

// 建模规范是发布闸门：命名、必填列、分区类型、粒度声明缺一不可。
// 这里覆盖每层规则的正例与反例，避免「某个层悄悄放宽」。
func TestRulesCheck(t *testing.T) {
	t.Run("各层正例", func(t *testing.T) {
		for _, layer := range []string{"ods", "dim", "dwd", "dws", "ads"} {
			spec := ruleSpec(layer, layer+"_order")
			if layer == "ods" {
				spec.PrimaryKey = nil
			}
			if got := modelc.Check(spec); len(got) != 0 {
				t.Fatalf("%s 正例不应违规: %+v", layer, got)
			}
		}
	})

	t.Run("层名非法直接跳过", func(t *testing.T) {
		if got := modelc.Check(model.ModelSpec{Name: "x", Layer: "mid"}); len(got) != 0 {
			t.Fatalf("非法层应由 Validate 处理: %+v", got)
		}
	})

	cases := []struct {
		name string
		spec model.ModelSpec
		want string
		min  int
	}{
		{"名称缺层前缀", ruleSpec("dwd", "order"), "layer_name", 2},
		{"物理表名不合规", func() model.ModelSpec {
			s := ruleSpec("dwd", "dwd_order")
			s.Table = "order_raw"
			return s
		}(), "layer_name", 1},
		{"缺必填列", func() model.ModelSpec {
			s := ruleSpec("dwd", "dwd_order")
			s.Columns = []model.ColumnSpec{{Name: "id", Type: "bigint"}}
			return s
		}(), "required_column", 1},
		{"dt 类型非法", func() model.ModelSpec {
			s := ruleSpec("dwd", "dwd_order")
			s.Columns[1].Type = "string"
			return s
		}(), "dt_type", 1},
		{"DWD 缺主键", func() model.ModelSpec {
			s := ruleSpec("dwd", "dwd_order")
			s.PrimaryKey = nil
			return s
		}(), "require_key", 1},
		{"DWD 有唯一键即可", func() model.ModelSpec {
			s := ruleSpec("dwd", "dwd_order")
			s.PrimaryKey = nil
			s.UniqueKey = []string{"id"}
			return s
		}(), "", 0},
		{"ODS 不要求主键", func() model.ModelSpec {
			s := ruleSpec("ods", "order")
			s.PrimaryKey = nil
			return s
		}(), "", 0},
		{"ODS 名称必须小写", ruleSpec("ods", "ODS_ORDER"), "layer_name", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := violations(tc.spec)
			if tc.min == 0 {
				if len(got) != 0 {
					t.Fatalf("不应违规: %+v", got)
				}
				return
			}
			if got[tc.want] < tc.min {
				t.Fatalf("期望至少 %d 条 %s，实际 %+v", tc.min, tc.want, got)
			}
		})
	}
}

// 自定义规则：正则写错必须明确报错，而不是静默放行所有表名。
func TestRulesCustom(t *testing.T) {
	bad := modelc.Rules{Layers: map[string]modelc.LayerRule{
		"dwd": {Layer: "dwd", NamePattern: "[", RequiredColumns: []string{"dt"}, RequireKey: true},
	}}
	got := bad.Check(ruleSpec("dwd", "dwd_order"))
	if len(got) != 1 || got[0].Rule != "name_pattern" {
		t.Fatalf("非法正则应报 name_pattern: %+v", got)
	}

	// 未配置的层回落到内置规则。
	empty := modelc.Rules{}
	spec := ruleSpec("dwd", "dwd_order")
	spec.PrimaryKey = nil
	if got := empty.Check(spec); len(got) == 0 || got[0].Rule != "require_key" {
		t.Fatalf("缺省规则未生效: %+v", got)
	}

	// 自定义必填列与命名规则要真正生效。
	custom := modelc.Rules{Layers: map[string]modelc.LayerRule{
		"dwd": {Layer: "dwd", NamePattern: "^fact_", RequiredColumns: []string{"dt", "tenant_id"}, RequireKey: true},
	}}
	got = custom.Check(ruleSpec("dwd", "dwd_order"))
	rules := map[string]int{}
	for _, v := range got {
		rules[v.Rule]++
	}
	if rules["layer_name"] == 0 || rules["required_column"] == 0 {
		t.Fatalf("自定义规则未生效: %+v", got)
	}
}

// 引用校验：反向依赖是硬编码禁令，任何配置都不能绕开；
// 未识别的引用必须拒绝（平台无法判定它的层）。
func TestRulesCheckRefs(t *testing.T) {
	r := modelc.DefaultRules()
	upstream := map[string]string{
		"ods_order": "ods",
		"dwd_order": "dwd",
		"dws_sales": "dws",
		"dim_user":  "dim",
	}
	cases := []struct {
		name  string
		spec  model.ModelSpec
		refs  []string
		want  string
		count int
	}{
		{"同层", ruleSpec("dwd", "dwd_a"), []string{"dwd_order"}, "", 0},
		{"引用上游", ruleSpec("dwd", "dwd_a"), []string{"ods_order", "dim_user"}, "", 0},
		{"反向依赖", ruleSpec("dwd", "dwd_a"), []string{"dws_sales"}, "reverse_dependency", 1},
		{"跨层不允许", ruleSpec("dws", "dws_a"), []string{"ods_order"}, "layer_ref", 1},
		{"未知引用", ruleSpec("dwd", "dwd_a"), []string{"unknown_table"}, "unknown_ref", 1},
		{"空引用跳过", ruleSpec("dwd", "dwd_a"), []string{"  "}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.CheckRefs(tc.spec, tc.refs, upstream)
			if tc.count == 0 {
				if len(got) != 0 {
					t.Fatalf("不应违规: %+v", got)
				}
				return
			}
			if len(got) != tc.count || got[0].Rule != tc.want {
				t.Fatalf("期望 %s，实际 %+v", tc.want, got)
			}
			if got[0].Field != "sql" || got[0].Message == "" {
				t.Fatalf("违规结构错误: %+v", got[0])
			}
		})
	}

	t.Run("ODS 可引用带点的物理表", func(t *testing.T) {
		spec := ruleSpec("ods", "order")
		spec.PrimaryKey = nil
		if got := r.CheckRefs(spec, []string{"pg_main.public.order"}, upstream); len(got) != 0 {
			t.Fatalf("ODS 物理表引用应放行: %+v", got)
		}
	})
	t.Run("非 ODS 的带点引用仍拒绝", func(t *testing.T) {
		got := r.CheckRefs(ruleSpec("dwd", "dwd_a"), []string{"pg_main.public.order"}, upstream)
		if len(got) != 1 || got[0].Rule != "unknown_ref" {
			t.Fatalf("非 ODS 不应放行物理表引用: %+v", got)
		}
	})
	t.Run("非法层跳过", func(t *testing.T) {
		if got := r.CheckRefs(model.ModelSpec{Layer: "mid"}, []string{"dws_sales"}, upstream); len(got) != 0 {
			t.Fatalf("非法层应跳过: %+v", got)
		}
	})
	t.Run("自定义层规则回落到内置", func(t *testing.T) {
		empty := modelc.Rules{}
		got := empty.CheckRefs(ruleSpec("dws", "dws_a"), []string{"ods_order"}, upstream)
		if len(got) != 1 || got[0].Rule != "layer_ref" {
			t.Fatalf("缺省规则未生效: %+v", got)
		}
	})
	t.Run("自定义允许上游", func(t *testing.T) {
		custom := modelc.Rules{Layers: map[string]modelc.LayerRule{
			"dwd": {Layer: "dwd", AllowUpstream: []string{"ods"}},
		}}
		if got := custom.CheckRefs(ruleSpec("dwd", "dwd_a"), []string{"dim_user"}, upstream); len(got) != 1 {
			t.Fatalf("dim 不在允许列表应拒绝: %+v", got)
		}
	})
}

// Enforce 是 API 直接返回的错误：必须带逐条问题与数量，前端才能渲染。
func TestEnforce(t *testing.T) {
	if err := modelc.Enforce(nil); err != nil {
		t.Fatalf("无违规应返回 nil: %v", err)
	}
	list := []modelc.RuleViolation{
		{Rule: "layer_name", Field: "name", Message: "表名不合规"},
		{Rule: "require_key", Field: "primary_key", Message: "缺主键"},
	}
	err := modelc.Enforce(list)
	if !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("错误码错误: %v", err)
	}
	ae := err.(*errs.AppError)
	if ae.Meta["problemCount"].(int) != 2 {
		t.Fatalf("问题数错误: %+v", ae.Meta)
	}
	msgs, _ := ae.Meta["problems"].([]string)
	if len(msgs) != 2 || !strings.Contains(msgs[0], "[layer_name]") {
		t.Fatalf("问题文案错误: %+v", msgs)
	}
	if !strings.Contains(err.Error(), "表名不合规") {
		t.Fatalf("错误信息应包含细节: %v", err)
	}
}

func TestCoveredLayers(t *testing.T) {
	got := modelc.DefaultRules().CoveredLayers()
	if strings.Join(got, ",") != "ads,dim,dwd,dws,ods" {
		t.Fatalf("覆盖层错误: %v", got)
	}
	empty := modelc.Rules{}
	if got := empty.CoveredLayers(); len(got) != 0 {
		t.Fatalf("空规则应返回空切片: %v", got)
	}
}
