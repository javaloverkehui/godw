package semantic

import (
	"fmt"
	"sort"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0701 数据集定义。
//
// 数据集是「物理表」与「业务问题」之间的翻译层：
// 业务只需要选维度和度量，不需要知道 JOIN 怎么写。
// 但翻译层一旦出错（笛卡尔积、跨层引用），错的是全公司的报表，
// 所以这里的校验宁可严一点：没有 on 条件的 join 直接拒绝。

// Dataset 是数据集定义。
type Dataset struct {
	ID          string      `json:"id,omitempty"`
	WorkspaceID string      `json:"workspaceId,omitempty"`
	Name        string      `json:"name" yaml:"name"`
	Description string      `json:"description" yaml:"description"`
	BaseModel   string      `json:"baseModel" yaml:"baseModel"`
	Joins       []Join      `json:"joins,omitempty" yaml:"joins,omitempty"`
	Dimensions  []Dimension `json:"dimensions" yaml:"dimensions"`
	Measures    []Measure   `json:"measures" yaml:"measures"`
	Filters     []Filter    `json:"filters,omitempty" yaml:"filters,omitempty"`
}

// Join 是一次关联。Type 只允许 left/inner：right/full 会让主表行丢失或膨胀。
type Join struct {
	Model string   `json:"model" yaml:"model"`
	Alias string   `json:"alias,omitempty" yaml:"alias,omitempty"`
	Type  string   `json:"type" yaml:"type"`
	On    []JoinOn `json:"on" yaml:"on"`
}

// JoinOn 是一对关联条件。
type JoinOn struct {
	Left  string `json:"left" yaml:"left"`
	Right string `json:"right" yaml:"right"`
}

// Dimension 是维度。
type Dimension struct {
	Name       string `json:"name" yaml:"name"`
	Field      string `json:"field" yaml:"field"`
	Type       string `json:"type" yaml:"type"` // categorical | time | number
	Filterable bool   `json:"filterable,omitempty" yaml:"filterable,omitempty"`
	// TimeGrains 仅 time 类型有意义；为空时自动派生全部粒度。
	TimeGrains []string `json:"timeGrains,omitempty" yaml:"timeGrains,omitempty"`
}

// Measure 是度量。
type Measure struct {
	Name   string `json:"name" yaml:"name"`
	Field  string `json:"field" yaml:"field"`
	Agg    string `json:"agg" yaml:"agg"`
	Format string `json:"format,omitempty" yaml:"format,omitempty"` // amount | percent | integer
	Unit   string `json:"unit,omitempty" yaml:"unit,omitempty"`
	// ZeroWhenNull 表示 SUM 为 NULL 时输出 0（空结果展示更友好）。
	ZeroWhenNull bool `json:"zeroWhenNull,omitempty" yaml:"zeroWhenNull,omitempty"`
}

// Filter 是数据集级默认过滤（永远生效，例如只统计未删除订单）。
type Filter struct {
	Field string `json:"field" yaml:"field"`
	Op    string `json:"op" yaml:"op"`
	Value any    `json:"value" yaml:"value"`
}

// 允许的分层。ODS 是原始层，直接暴露给业务会让口径与性能都失控。
var datasetAllowedLayers = map[string]bool{"dwd": true, "dws": true, "ads": true, "dim": true}

var timeGrains = []string{"day", "week", "month", "quarter", "year"}

var measureAggs = map[string]bool{
	"sum": true, "count": true, "count_distinct": true,
	"avg": true, "max": true, "min": true,
}

var filterOps = map[string]bool{
	"=": true, "!=": true, ">": true, ">=": true, "<": true, "<=": true,
	"in": true, "not_in": true, "like": true, "is_null": true, "is_not_null": true,
}

// ValidateDataset 校验数据集结构。models 提供「模型名 → 分层」，用于拒绝引用 ODS。
func ValidateDataset(d Dataset, models map[string]model.ModelSpec) error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if !identRe.MatchString(d.Name) {
		add("name 必须是标识符，实际 %q", d.Name)
	}
	if d.BaseModel == "" {
		add("baseModel 不能为空")
	} else if m, ok := models[d.BaseModel]; !ok {
		add("baseModel %s 不存在或未发布", d.BaseModel)
	} else if !datasetAllowedLayers[m.Layer] {
		add("数据集只能引用 dwd/dws/ads/dim，%s 属于 %s 层", d.BaseModel, m.Layer)
	}

	seenAlias := map[string]bool{"": true}
	for i, j := range d.Joins {
		if j.Model == "" {
			add("joins[%d].model 不能为空", i)
			continue
		}
		m, ok := models[j.Model]
		if !ok {
			add("joins[%d] 引用的模型 %s 不存在或未发布", i, j.Model)
		} else if !datasetAllowedLayers[m.Layer] {
			add("joins[%d] 引用的 %s 属于 %s 层，数据集不允许引用 ODS", i, j.Model, m.Layer)
		}
		switch j.Type {
		case "left", "inner":
		case "":
			add("joins[%d].type 必须显式声明 left 或 inner", i)
		default:
			add("joins[%d].type 只支持 left/inner，实际 %q", i, j.Type)
		}
		// 没有 on 条件就是笛卡尔积，必须拒绝。
		if len(j.On) == 0 {
			add("joins[%d]（%s）缺少 on 条件，会退化为笛卡尔积", i, j.Model)
		}
		for k, on := range j.On {
			if strings.TrimSpace(on.Left) == "" || strings.TrimSpace(on.Right) == "" {
				add("joins[%d].on[%d] 两侧字段都不能为空", i, k)
			}
		}
		alias := j.Alias
		if alias != "" {
			if seenAlias[alias] {
				add("别名重复: %s", alias)
			}
			seenAlias[alias] = true
		}
	}

	if len(d.Dimensions) == 0 {
		add("dimensions 不能为空")
	}
	dimNames := map[string]bool{}
	for i, dim := range d.Dimensions {
		if !identRe.MatchString(dim.Name) {
			add("dimensions[%d].name 非法: %q", i, dim.Name)
		}
		if dimNames[dim.Name] {
			add("维度名重复: %s", dim.Name)
		}
		dimNames[dim.Name] = true
		switch dim.Type {
		case "categorical", "time", "number":
		case "":
			add("dimensions[%d].type 不能为空", i)
		default:
			add("dimensions[%d].type 只支持 categorical/time/number，实际 %q", i, dim.Type)
		}
		if dim.Type == "time" {
			for _, g := range dim.TimeGrains {
				if !containsStr(timeGrains, g) {
					add("dimensions[%d] 的时间粒度 %q 不支持", i, g)
				}
			}
		}
	}

	if len(d.Measures) == 0 {
		add("measures 不能为空")
	}
	for i, m := range d.Measures {
		if !identRe.MatchString(m.Name) {
			add("measures[%d].name 非法: %q", i, m.Name)
		}
		if !measureAggs[m.Agg] {
			add("measures[%d].agg 不支持: %q", i, m.Agg)
		}
		if m.Agg != "count" {
			if _, _, ferr := FieldRef(m.Field); ferr != nil {
				add("measures[%d].field 非法: %v", i, ferr)
			}
		}
		switch m.Format {
		case "", "amount", "percent", "integer", "decimal":
		default:
			add("measures[%d].format 不支持: %q", i, m.Format)
		}
	}

	for i, f := range d.Filters {
		if !filterOps[f.Op] {
			add("filters[%d].op 不支持: %q", i, f.Op)
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeInvalidArgument, "数据集定义不合法").WithMeta("problems", problems)
	}
	return nil
}

// DimensionGrains 返回一个时间维度可用的粒度。未显式声明时给全集。
func DimensionGrains(dim Dimension) []string {
	if dim.Type != "time" {
		return nil
	}
	if len(dim.TimeGrains) == 0 {
		return append([]string{}, timeGrains...)
	}
	return dim.TimeGrains
}

// FieldRef 解析维度/度量的字段引用，支持别名限定（o.amount）。
// 返回值已做标识符白名单校验，可直接用于拼 SQL（仍会走方言 QuoteIdent）。
func FieldRef(field string) (alias, column string, err error) {
	parts := strings.Split(field, ".")
	switch len(parts) {
	case 1:
		if !identRe.MatchString(parts[0]) {
			return "", "", errs.New(errs.CodeInvalidArgument, "字段名非法: "+field)
		}
		return "", parts[0], nil
	case 2:
		if !identRe.MatchString(parts[0]) || !identRe.MatchString(parts[1]) {
			return "", "", errs.New(errs.CodeInvalidArgument, "字段引用非法: "+field)
		}
		return parts[0], parts[1], nil
	}
	return "", "", errs.New(errs.CodeInvalidArgument, "字段引用非法: "+field)
}

// ReferencedModels 返回数据集引用的全部模型名，用于血缘与影响分析。
func (d Dataset) ReferencedModels() []string {
	out := []string{}
	if d.BaseModel != "" {
		out = append(out, d.BaseModel)
	}
	for _, j := range d.Joins {
		if j.Model != "" {
			out = append(out, j.Model)
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// TableRef 是渲染后的表引用（带别名）。
type TableRef struct {
	Model string
	Alias string
}

// FromClause 生成 FROM + JOIN。所有标识符走方言引用。
func (d Dataset) FromClause(models map[string]model.ModelSpec, dl dialect.Dialect) (string, error) {
	base, ok := models[d.BaseModel]
	if !ok {
		return "", errs.New(errs.CodeInvalidArgument, "基础模型不存在: "+d.BaseModel)
	}
	var b strings.Builder
	b.WriteString(tableOf(base, dl))
	if d.BaseModel != "" {
		// 基础表用模型名做别名，保证维度里写 dwd_order.amount 也能解析。
		b.WriteString(" AS " + dl.QuoteIdent(aliasOf(base, d.BaseModel)))
	}
	for _, j := range d.Joins {
		m, ok := models[j.Model]
		if !ok {
			return "", errs.New(errs.CodeInvalidArgument, "关联模型不存在: "+j.Model)
		}
		b.WriteString("\n" + strings.ToUpper(j.Type) + " JOIN " + tableOf(m, dl) +
			" AS " + dl.QuoteIdent(aliasOf(m, j.Alias)))
		conds := make([]string, 0, len(j.On))
		for _, on := range j.On {
			lAlias, lCol, err := FieldRef(on.Left)
			if err != nil {
				return "", err
			}
			rAlias, rCol, err := FieldRef(on.Right)
			if err != nil {
				return "", err
			}
			conds = append(conds, qualifyCol(lAlias, lCol, dl)+" = "+qualifyCol(rAlias, rCol, dl))
		}
		b.WriteString(" ON " + strings.Join(conds, " AND "))
	}
	return b.String(), nil
}

func tableOf(spec model.ModelSpec, d dialect.Dialect) string {
	if spec.Schema == "" {
		return d.QuoteIdent(spec.TableName())
	}
	return d.QuoteIdent(spec.Schema) + "." + d.QuoteIdent(spec.TableName())
}

func aliasOf(spec model.ModelSpec, alias string) string {
	if alias != "" {
		return alias
	}
	return spec.Name
}

func qualifyCol(alias, col string, d dialect.Dialect) string {
	if alias == "" {
		return d.QuoteIdent(col)
	}
	return d.QuoteIdent(alias) + "." + d.QuoteIdent(col)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
