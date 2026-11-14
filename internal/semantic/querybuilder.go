package semantic

import (
	"fmt"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0703 查询构建器。
//
// 目标：把「维度 + 度量 + 筛选」翻译成一条确定、可读、可注入防护的 SQL。
// 三条硬规则：
//   1. 所有标识符必须来自白名单（维度/度量定义），不接受任意表名；
//   2. 筛选值全部参数化（$n / ?），只有标识符拼字符串；
//   3. 无 GROUP BY 的聚合必须返回一行——SUM 为 NULL 时按配置转 0。

// QueryRequest 是查询请求。
type QueryRequest struct {
	Dataset    string   `json:"dataset"`
	Dimensions []string `json:"dimensions"`
	Measures   []string `json:"measures"`
	// Granularity 作用于第一个 time 类型维度。
	Granularity string   `json:"granularity,omitempty"`
	Filters     []Filter `json:"filters,omitempty"`
	OrderBy     string   `json:"orderBy,omitempty"`
	Desc        bool     `json:"desc,omitempty"`
	Limit       int      `json:"limit,omitempty"`
	Offset      int      `json:"offset,omitempty"`
	// RLS 是服务端注入的行级权限条件（已渲染），不接受外部传入。
	RLS []string `json:"-"`
}

// BuiltQuery 是编译产物。
type BuiltQuery struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
	// Columns 是结果列名，顺序与 SELECT 一致。
	Columns []string `json:"columns"`
	// Comment 是 SQL 注释，标明来源数据集，便于 DB 侧排查。
	Comment string `json:"comment"`
}

// 排序方向与空值处理是常见踩坑点，这里统一处理而不是交给调用方。
const maxLimit = 10000

// BuildQuery 编译数据集查询。
func BuildQuery(d Dataset, req QueryRequest, models map[string]model.ModelSpec, dl dialect.Dialect) (BuiltQuery, error) {
	if d.Name == "" {
		return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "数据集不能为空")
	}
	fromClause, err := d.FromClause(models, dl)
	if err != nil {
		return BuiltQuery{}, err
	}

	dims := map[string]Dimension{}
	for _, dim := range d.Dimensions {
		dims[dim.Name] = dim
	}
	meas := map[string]Measure{}
	for _, m := range d.Measures {
		meas[m.Name] = m
	}

	var selectParts []string
	var cols []string
	var groupParts []string

	for _, name := range req.Dimensions {
		dim, ok := dims[name]
		if !ok {
			return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "未知维度: "+name)
		}
		alias, col, ferr := FieldRef(dim.Field)
		if ferr != nil {
			return BuiltQuery{}, ferr
		}
		expr := qualifyCol(alias, col, dl)
		// 时间维度按粒度截断；非时间维度忽略粒度。
		if dim.Type == "time" && req.Granularity != "" {
			if !containsStr(DimensionGrains(dim), req.Granularity) {
				return BuiltQuery{}, errs.New(errs.CodeInvalidArgument,
					fmt.Sprintf("维度 %s 不支持粒度 %s", name, req.Granularity))
			}
			expr = dl.DateTrunc(req.Granularity, expr)
		}
		selectParts = append(selectParts, expr+" AS "+dl.QuoteIdent(name))
		groupParts = append(groupParts, expr)
		cols = append(cols, name)
	}

	if len(req.Measures) == 0 {
		return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "至少选择一个度量")
	}
	for _, name := range req.Measures {
		m, ok := meas[name]
		if !ok {
			return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "未知度量: "+name)
		}
		alias, col, ferr := FieldRef(m.Field)
		if ferr != nil {
			return BuiltQuery{}, ferr
		}
		inner := "*"
		if m.Agg != "count" || m.Field != "" {
			inner = qualifyCol(alias, col, dl)
		}
		var expr string
		switch m.Agg {
		case "count_distinct":
			expr = fmt.Sprintf("COUNT(DISTINCT %s)", inner)
		case "count":
			expr = fmt.Sprintf("COUNT(%s)", inner)
		default:
			expr = strings.ToUpper(m.Agg) + "(" + inner + ")"
		}
		if m.ZeroWhenNull {
			// 空结果展示 0 比展示 NULL 更符合业务预期。
			switch dl.Name() {
			case "mysql":
				expr = fmt.Sprintf("IFNULL(%s, 0)", expr)
			default:
				expr = fmt.Sprintf("COALESCE(%s, 0)", expr)
			}
		}
		selectParts = append(selectParts, expr+" AS "+dl.QuoteIdent(name))
		cols = append(cols, name)
	}

	// WHERE：数据集默认过滤 + 请求过滤 + 行级权限。
	var whereParts []string
	args := []any{}
	argN := 0
	addArg := func(v any) string {
		argN++
		args = append(args, v)
		return dl.Placeholder(argN)
	}

	for _, f := range d.Filters {
		cond, cargs, cerr := buildFilter(f, dl, nil)
		if cerr != nil {
			return BuiltQuery{}, cerr
		}
		for _, a := range cargs {
			argN++
			args = append(args, a)
		}
		whereParts = append(whereParts, cond)
	}
	for _, f := range req.Filters {
		// 只有 filterable 的维度才能被外部筛选，防止「顺手拿敏感列做枚举」。
		if dim, ok := dims[f.Field]; ok && !dim.Filterable {
			return BuiltQuery{}, errs.New(errs.CodeInvalidArgument,
				"维度 "+f.Field+" 未开启筛选，请先在数据集定义中设置 filterable: true")
		}
		if _, ok := dims[f.Field]; !ok {
			if _, isMeasure := meas[f.Field]; !isMeasure {
				return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "筛选字段不在数据集内: "+f.Field)
			}
		}
		cond, cargs, cerr := buildFilter(f, dl, addArg)
		if cerr != nil {
			return BuiltQuery{}, cerr
		}
		if len(cargs) == 0 {
			whereParts = append(whereParts, cond)
		}
	}
	whereParts = append(whereParts, req.RLS...)

	sql := "SELECT " + strings.Join(selectParts, ", ") + "\nFROM " + fromClause
	if len(whereParts) > 0 {
		sql += "\nWHERE " + strings.Join(whereParts, " AND ")
	}
	if len(groupParts) > 0 {
		sql += "\nGROUP BY " + strings.Join(groupParts, ", ")
	}

	// ORDER BY：只允许对输出列排序，避免用排序探测未选中的列。
	if req.OrderBy != "" {
		if !containsStr(cols, req.OrderBy) {
			return BuiltQuery{}, errs.New(errs.CodeInvalidArgument, "orderBy 必须是已选中的列: "+req.OrderBy)
		}
		dir := "ASC"
		if req.Desc {
			dir = "DESC"
		}
		sql += "\nORDER BY " + dl.QuoteIdent(req.OrderBy) + " " + dir
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 1000
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	sql += "\n" + dl.LimitOffset(int64(limit), int64(req.Offset))

	comment := fmt.Sprintf("/* dataset=%s v%d */", d.Name, d.Version)
	return BuiltQuery{
		SQL:     comment + "\n" + sql,
		Args:    args,
		Columns: cols,
		Comment: comment,
	}, nil
}

// buildFilter 生成单个筛选条件。
// next 为 nil 时把值内联为字面量（用于数据集默认过滤，值来自定义文件而非用户输入）；
// 非 nil 时走参数化。
func buildFilter(f Filter, dl dialect.Dialect, next func(any) string) (string, []any, error) {
	alias, col, err := FieldRef(f.Field)
	if err != nil {
		return "", nil, err
	}
	if !filterOps[f.Op] {
		return "", nil, errs.New(errs.CodeInvalidArgument, "不支持的筛选操作符: "+f.Op)
	}
	ref := qualifyCol(alias, col, dl)

	// 空值判断不需要值。
	switch f.Op {
	case "is_null":
		return ref + " IS NULL", nil, nil
	case "is_not_null":
		return ref + " IS NOT NULL", nil, nil
	}

	if f.Op == "in" || f.Op == "not_in" {
		list, ok := f.Value.([]any)
		if !ok || len(list) == 0 {
			return "", nil, errs.New(errs.CodeInvalidArgument, f.Op+" 需要一个非空数组")
		}
		ph := make([]string, 0, len(list))
		var args []any
		for _, v := range list {
			if next != nil {
				ph = append(ph, next(v))
			} else {
				ph = append(ph, literalOf(v))
				args = append(args, v)
			}
		}
		kw := "IN"
		if f.Op == "not_in" {
			kw = "NOT IN"
		}
		return ref + " " + kw + " (" + strings.Join(ph, ", ") + ")", args, nil
	}

	if f.Op == "like" {
		s, ok := f.Value.(string)
		if !ok {
			return "", nil, errs.New(errs.CodeInvalidArgument, "like 需要字符串值")
		}
		if next != nil {
			return ref + " LIKE " + next(s), nil, nil
		}
		return ref + " LIKE " + literalOf(s), []any{s}, nil
	}

	if f.Value == nil {
		return "", nil, errs.New(errs.CodeInvalidArgument, "操作符 "+f.Op+" 需要值")
	}
	if next != nil {
		return ref + " " + f.Op + " " + next(f.Value), nil, nil
	}
	return ref + " " + f.Op + " " + literalOf(f.Value), []any{f.Value}, nil
}

// literalOf 只在「值来自服务端配置」时使用；用户输入一律走参数化。
func literalOf(v any) string {
	switch t := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return "NULL"
	default:
		return fmt.Sprint(t)
	}
}

// Explain 返回人类可读的查询计划摘要，供前端「查看将执行什么」。
func Explain(q BuiltQuery) string {
	var b strings.Builder
	b.WriteString(q.Comment + "\n")
	b.WriteString(q.SQL)
	if len(q.Args) > 0 {
		b.WriteString(fmt.Sprintf("\n-- 参数 %d 个: %v", len(q.Args), q.Args))
	}
	return b.String()
}
