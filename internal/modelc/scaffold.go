package modelc

import (
	"fmt"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

// T0309 建模脚手架。目标不是「生成一份能跑的 YAML」，
// 而是「生成一份天生合规、改两行就能用的 YAML」：
// 审计列、分区、类型映射、主键候选全部预置好。

// ScaffoldOptions 是建表脚手架的输入。
type ScaffoldOptions struct {
	Layer      string
	Name       string
	SourceName string
	Datasource string
	Schema     string
	// Upstream 是上游模型的物理表结构；缺省时用标准模板。
	Upstream []model.ColumnSpec
	// PrimaryKey 显式指定主键；为空时自动推断候选。
	PrimaryKey []string
	// Engine 用于类型映射，默认 postgres。
	Engine string
	// BizDateColumn 上游的日期列名，用于推分区；为空时分区列取 dt。
	// 无论取哪一列，产物里都会有 dt（分层规范的必填列），不会出现「补了分区列、缺了 dt」的半成品。
	BizDateColumn string
}

// ScaffoldResult 是脚手架产物，包含可解释的推断过程。
type ScaffoldResult struct {
	Spec model.ModelSpec `json:"spec"`
	YAML string          `json:"yaml"`
	// Keys 是推断出的主键候选，供前端提示确认。
	KeyCandidates []string `json:"keyCandidates"`
	// Warnings 是需要人看一眼的地方。
	Warnings []string `json:"warnings"`
}

// 各层的标准列模板。顺序即建表顺序，方便人读。
var layerTemplates = map[string][]model.ColumnSpec{
	string(model.LayerODS): {
		{Name: "_godw_src", Type: dialect.LString, Nullable: false, Comment: "来源标识：库.表"},
		{Name: "_godw_load_at", Type: dialect.LDatetime, Nullable: false, Comment: "入库时间"},
	},
	string(model.LayerDWD): {},
	string(model.LayerDWS): {},
	string(model.LayerADS): {},
	string(model.LayerDIM): {},
}

// Scaffold 根据上游表结构生成模型骨架。
func Scaffold(opt ScaffoldOptions) (ScaffoldResult, error) {
	layer := strings.ToLower(strings.TrimSpace(opt.Layer))
	if !model.ValidLayer(layer) {
		return ScaffoldResult{}, fmt.Errorf("layer 必须为 ods/dwd/dws/ads/dim，实际 %q", opt.Layer)
	}
	name := strings.TrimSpace(opt.Name)
	if name == "" {
		name = opt.SourceName
	}
	if name == "" {
		return ScaffoldResult{}, fmt.Errorf("必须提供 name 或 source_name")
	}
	// 自动补层前缀，避免生成一个注定不合规的名字。
	if !strings.HasPrefix(name, layer+"_") {
		name = layer + "_" + strings.TrimPrefix(name, "ods_")
	}
	engine := opt.Engine
	if engine == "" {
		engine = "postgres"
	}

	spec := model.ModelSpec{
		Kind:       "Model",
		Name:       name,
		Layer:      layer,
		Datasource: opt.Datasource,
		Schema:     opt.Schema,
	}
	if spec.Datasource == "" {
		spec.Datasource = "pg_dw"
	}
	if spec.Schema == "" {
		spec.Schema = layer
	}

	var warnings []string

	// 列：上游列 + 层标准列，按引擎做类型归一。
	seen := map[string]bool{}
	// sourceKey 是源表 DDL 里声明的主键，来自元数据采集，不靠列名猜。
	var sourceKey []string
	for _, c := range opt.Upstream {
		nc := model.ColumnSpec{
			Name:     sanitizeIdent(c.Name),
			Type:     normalizeType(c.Type, engine),
			Nullable: c.Nullable,
			Comment:  c.Comment,
		}
		if seen[nc.Name] {
			warnings = append(warnings, "上游存在重名列，已跳过: "+nc.Name)
			continue
		}
		seen[nc.Name] = true
		// 列名归一后可能被改名，主键要跟着换成归一后的名字，否则 YAML 引用不到列。
		if c.PrimaryKey {
			sourceKey = append(sourceKey, nc.Name)
		}
		spec.Columns = append(spec.Columns, nc)
	}

	// 分区列与必填日期列。
	//
	// 分层规范对每一层都要求 dt 这一列（分区日期的统一叫法），所以脚手架必须保证它存在。
	// 这里曾经的做法是「优先用上游第一个日期列当分区，没有才补 dt」——结果遇到
	// 只有 created_at 的表，产出的模型缺 dt，用户看到的是一条
	// 「层 ods 缺少必填列 dt」的告警，而且照着建一定发布失败。
	// 脚手架应该兜住自己那部分：规则要求的列，直接给出来。
	bizCol := strings.TrimSpace(opt.BizDateColumn)
	if bizCol != "" && !seen[bizCol] {
		// 显式指定但上游没有这一列：仍然补出来，调用方明确要它当分区。
		spec.Columns = append([]model.ColumnSpec{{
			Name: bizCol, Type: dialect.LDate, Nullable: false, Comment: "分区日期",
		}}, spec.Columns...)
		seen[bizCol] = true
	}
	if bizCol == "" {
		bizCol = "dt"
	}
	if !seen["dt"] {
		spec.Columns = append([]model.ColumnSpec{{
			Name: "dt", Type: dialect.LDate, Nullable: false, Comment: "分区日期",
		}}, spec.Columns...)
		seen["dt"] = true
	}
	spec.PartitionBy = []string{bizCol}

	// 层标准列（ODS 审计列）。命名冲突时改名并提示。
	for _, t := range layerTemplates[layer] {
		if seen[t.Name] {
			continue
		}
		spec.Columns = append(spec.Columns, t)
		seen[t.Name] = true
	}

	// 主键优先级：显式指定 > 源表声明的主键 > 列名启发式。
	// 源表 DDL 已经写了 PRIMARY KEY，就不该让用户再回答一次。
	spec.PrimaryKey = opt.PrimaryKey
	if len(spec.PrimaryKey) == 0 {
		spec.PrimaryKey = sourceKey
	}
	if len(spec.PrimaryKey) == 0 {
		spec.PrimaryKey = guessKey(spec.Columns)
	}

	if layer == string(model.LayerODS) {
		// ODS 保原始，不加主键约束，避免源表重复键导致入库失败；
		// 也正因如此这里不告警——ODS 本来就不要求主键（规则里 RequireKey 为 false）。
		spec.PrimaryKey = nil
	} else {
		spec.UniqueKey = append([]string{}, spec.PrimaryKey...)
		if len(spec.PrimaryKey) == 0 {
			warnings = append(warnings, "未能推断主键，请在发布前手动指定 primary_key（否则无法幂等写入）")
		}
	}

	spec.Materialization = "table"
	if layer == string(model.LayerDWS) || layer == string(model.LayerADS) {
		spec.Tags = []string{"mart"}
	}

	// 用规范自检一遍，把问题作为 warning 抛出而不是失败——
	// 脚手架的职责是「给出可修改的起点」，不是替人挡路。
	for _, v := range DefaultRules().Check(spec) {
		warnings = append(warnings, v.Message)
	}
	for _, v := range ValidateTypes(spec, engine) {
		warnings = append(warnings, v.Message)
	}

	yamlText, err := MarshalSpec(spec)
	if err != nil {
		return ScaffoldResult{}, err
	}
	return ScaffoldResult{
		Spec:          spec,
		YAML:          yamlText,
		KeyCandidates: keyCandidates(sourceKey, spec.Columns),
		Warnings:      warnings,
	}, nil
}

// keyCandidates 把源表主键排在候选最前：那不是猜的，是源表已经声明的事实。
func keyCandidates(source []string, cols []model.ColumnSpec) []string {
	out := append([]string{}, source...)
	seen := make(map[string]bool, len(out))
	for _, k := range out {
		seen[k] = true
	}
	for _, k := range guessKeyCandidates(cols) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// guessKey 从列名推断主键：<实体>_id / id / <实体>_key，且非空。
func guessKey(cols []model.ColumnSpec) []string {
	for _, c := range cols {
		if c.Nullable {
			continue
		}
		n := strings.ToLower(c.Name)
		if n == "id" || strings.HasSuffix(n, "_id") || strings.HasSuffix(n, "_key") || n == "code" {
			return []string{c.Name}
		}
	}
	return nil
}

// guessKeyCandidates 返回所有可能做主键的列，供前端下拉选择。
func guessKeyCandidates(cols []model.ColumnSpec) []string {
	out := []string{}
	for _, c := range cols {
		n := strings.ToLower(c.Name)
		if strings.HasSuffix(n, "_id") || n == "id" || strings.HasSuffix(n, "_key") || n == "code" || strings.HasSuffix(n, "_no") {
			out = append(out, c.Name)
		}
	}
	return out
}

// normalizeType 把上游物理类型归一为逻辑类型，再按目标引擎确认可用。
func normalizeType(physical, engine string) string {
	return dialect.ReverseMap(engine, physical).String()
}

func sanitizeIdent(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9' && i > 0, r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteString("c" + string(r))
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "col"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "c_" + out
	}
	return out
}
