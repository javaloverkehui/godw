package modelc

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 分层不是文档里的口号，而是编译期必须通过的闸门。
// 本文件把「表名前缀、跨层引用、必填列」三件事做成可配置规则，
// 默认值不可关闭「禁止反向依赖」，只允许工作空间级收紧。

// LayerRule 是一层的约束。零值表示使用默认规则。
type LayerRule struct {
	Layer string `json:"layer" yaml:"layer"`
	// NamePattern 为空时使用内置 ^(layer)_[a-z][a-z0-9_]*$。
	NamePattern string `json:"namePattern,omitempty" yaml:"name_pattern,omitempty"`
	// AllowUpstream 允许引用的上游层。空表示使用默认表。
	AllowUpstream []string `json:"allowUpstream,omitempty" yaml:"allow_upstream,omitempty"`
	// RequiredColumns 该层必须存在的列。
	RequiredColumns []string `json:"requiredColumns,omitempty" yaml:"required_columns,omitempty"`
	// RequireKey 要求声明主键或唯一键（DWD/DWS 的粒度声明）。
	RequireKey bool `json:"requireKey,omitempty" yaml:"require_key,omitempty"`
}

// Rules 是整个工作空间的建模规范。
type Rules struct {
	// Layers 覆盖默认层规则，key 为层名。
	Layers map[string]LayerRule `json:"layers,omitempty" yaml:"layers,omitempty"`
}

// DefaultRules 返回内置规范。业务可覆盖，但「禁止反向依赖」在代码里硬编码。
func DefaultRules() Rules {
	return Rules{Layers: map[string]LayerRule{
		// ODS 是接入层，物理表由外部系统提供，命名与列都不由平台决定。
		//
		// 这里曾经强制「ods_ 前缀 + _godw_src/_godw_load_at 审计列」，
		// 结果是接一张已有的 order 表都要先建视图改名——为了规范而制造工作量，
		// 用户的第一反应是关掉校验，规范反而失效。所以 ODS 只要求：
		// 小写标识符（避免引号地狱）+ 有 dt（分区与增量都靠它）。
		// 审计列作为推荐约定写进文档，不做硬性拦截。
		string(model.LayerODS): {
			Layer:           string(model.LayerODS),
			NamePattern:     "^[a-z_][a-z0-9_]*$",
			AllowUpstream:   nil, // ODS 只允许读数据源原始表，不引用其它模型
			RequiredColumns: []string{"dt"},
		},
		string(model.LayerDIM): {
			Layer:           string(model.LayerDIM),
			AllowUpstream:   []string{string(model.LayerODS), string(model.LayerDWD), string(model.LayerDIM)},
			RequiredColumns: []string{"dt"},
			RequireKey:      true,
		},
		string(model.LayerDWD): {
			Layer:           string(model.LayerDWD),
			AllowUpstream:   []string{string(model.LayerODS), string(model.LayerDWD), string(model.LayerDIM)},
			RequiredColumns: []string{"dt"},
			RequireKey:      true,
		},
		string(model.LayerDWS): {
			Layer:           string(model.LayerDWS),
			AllowUpstream:   []string{string(model.LayerDWD), string(model.LayerDWS), string(model.LayerDIM)},
			RequiredColumns: []string{"dt"},
			RequireKey:      true,
		},
		string(model.LayerADS): {
			Layer:           string(model.LayerADS),
			AllowUpstream:   []string{string(model.LayerDWD), string(model.LayerDWS), string(model.LayerADS), string(model.LayerDIM)},
			RequiredColumns: []string{"dt"},
			RequireKey:      true,
		},
	}}
}

// layerOrder 给层一个「层级序」，用于判断是否反向依赖。
// 数字越大越靠下游：ods(0) < dim(1) < dwd(2) < dws(3) < ads(4)。
var layerOrder = map[string]int{
	string(model.LayerODS): 0, string(model.LayerDIM): 1, string(model.LayerDWD): 2,
	string(model.LayerDWS): 3, string(model.LayerADS): 4,
}

// RuleViolation 是一条规范违规。Path 指向模型字段，便于前端定位。
type RuleViolation struct {
	Rule    string `json:"rule"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Check 用默认规范校验单个模型。
func Check(spec model.ModelSpec) []RuleViolation { return DefaultRules().Check(spec) }

// Check 校验模型是否符合规范。返回空切片表示通过。
func (r Rules) Check(spec model.ModelSpec) []RuleViolation {
	var out []RuleViolation
	add := func(rule, field, format string, args ...any) {
		out = append(out, RuleViolation{Rule: rule, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	layer, ok := layerNorm(spec.Layer)
	if !ok {
		// layer 合法性由 Validate 负责，这里不重复报错。
		return out
	}
	rule, has := r.Layers[layer]
	if !has {
		rule = DefaultRules().Layers[layer]
	}

	// 1) 表名/模型名前缀。
	pattern := rule.NamePattern
	if pattern == "" {
		pattern = "^" + layer + "_[a-z][a-z0-9_]*$"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		add("name_pattern", "name", "层 %s 的名称正则非法: %v", layer, err)
	} else {
		for _, name := range []string{spec.Name, spec.TableName()} {
			if name == "" {
				continue
			}
			if !re.MatchString(name) {
				add("layer_name", "name", "层 %s 的表名必须匹配 %s，实际 %q", layer, pattern, name)
			}
		}
	}

	// 2) 必填列。
	cols := map[string]bool{}
	for _, c := range spec.Columns {
		cols[strings.ToLower(c.Name)] = true
	}
	for _, need := range rule.RequiredColumns {
		if !cols[strings.ToLower(need)] {
			add("required_column", "columns", "层 %s 缺少必填列 %s", layer, need)
		}
	}
	// dt 必须是日期类型，否则分区裁剪会失效。
	if dt, ok := findColumn(spec, "dt"); ok {
		if dt.Type != "date" && dt.Type != "datetime" {
			add("dt_type", "columns", "列 dt 必须为 date 或 datetime，实际 %q", dt.Type)
		}
	}

	// 3) 粒度声明。
	if rule.RequireKey && len(spec.PrimaryKey) == 0 && len(spec.UniqueKey) == 0 {
		add("require_key", "primary_key", "层 %s 必须声明 primary_key 或 unique_key（用于幂等写入）", layer)
	}
	return out
}

// CheckRefs 校验模型引用的上游表是否合规。
// upstream 是「上游模型名 → 层」的映射；无法识别的名字视为物理表引用，直接拒绝，
// 因为平台不能确认它的层，放行等于绕过分层约束。
func (r Rules) CheckRefs(spec model.ModelSpec, refs []string, upstream map[string]string) []RuleViolation {
	var out []RuleViolation
	layer, ok := layerNorm(spec.Layer)
	if !ok {
		return out
	}
	rule, has := r.Layers[layer]
	if !has {
		rule = DefaultRules().Layers[layer]
	}

	allow := map[string]bool{}
	for _, l := range rule.AllowUpstream {
		allow[l] = true
	}

	// 无论配置怎么写，反向依赖（下游读上游之外的更高层）永远拒绝。
	for _, ref := range refs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		upLayer, known := upstream[ref]
		if !known {
			// 允许显式声明为「数据源原始表」的引用：约定 ref 里带 "." 视为物理表。
			if strings.Contains(ref, ".") && layer == string(model.LayerODS) {
				continue
			}
			out = append(out, RuleViolation{
				Rule: "unknown_ref", Field: "sql",
				Message: fmt.Sprintf("引用的 %q 不是已发布的模型，无法判定分层；请先发布上游模型", ref),
			})
			continue
		}
		if upLayer == layer {
			continue
		}
		if layerOrder[upLayer] > layerOrder[layer] {
			out = append(out, RuleViolation{
				Rule: "reverse_dependency", Field: "sql",
				Message: fmt.Sprintf("层 %s 不能引用下游层 %s 的模型 %s", layer, upLayer, ref),
			})
			continue
		}
		if len(allow) > 0 && !allow[upLayer] {
			out = append(out, RuleViolation{
				Rule: "layer_ref", Field: "sql",
				Message: fmt.Sprintf("层 %s 只允许引用 %s，%s 属于 %s",
					layer, strings.Join(sortedKeys(allow), "/"), ref, upLayer),
			})
		}
	}
	return out
}

// Enforce 把违规转成带元数据的错误，供 API 直接返回。
func Enforce(violations []RuleViolation) error {
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(violations))
	for _, v := range violations {
		msgs = append(msgs, fmt.Sprintf("[%s] %s", v.Rule, v.Message))
	}
	return errs.New(errs.CodeModelInvalid, "建模规范校验未通过").
		WithMeta("violations", violations).
		WithMeta("problemCount", len(violations)).
		WithMeta("problems", msgs)
}

// CoveredLayers 返回规范覆盖的层，排序输出。
func (r Rules) CoveredLayers() []string {
	out := make([]string, 0, len(r.Layers))
	for k := range r.Layers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func layerNorm(l string) (string, bool) {
	l = strings.ToLower(strings.TrimSpace(l))
	if _, ok := layerOrder[l]; ok {
		return l, true
	}
	return l, false
}

func findColumn(spec model.ModelSpec, name string) (model.ColumnSpec, bool) {
	for _, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return model.ColumnSpec{}, false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
