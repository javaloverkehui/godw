package dialect

import (
	"fmt"
	"sort"
	"strings"
)

// T0305 跨引擎类型映射矩阵。
//
// 设计取舍：逻辑类型是「业务能理解的语义」，物理类型是「引擎能存的字节」。
// 两者之间必然存在有损映射（json → String、decimal(38,10) → Decimal(20,4)），
// 平台的态度是「明确告知并要人确认」，而不是静默截断。
// 因此每个映射都记录 Lossy 标记，编译时未声明 allowLossy 一律报错。

// 扩展逻辑类型：在 dialect 基础类型之外，补上精度型与容器型。
const (
	LDecimalPS = "decimal(p,s)"
	LArray     = "array"
	LMap       = "map"
)

// TypeSpec 描述一个逻辑类型的完整定义，包含精度参数。
type TypeSpec struct {
	Logical   string `json:"logical"`
	Precision int    `json:"precision,omitempty"`
	Scale     int    `json:"scale,omitempty"`
	Elem      string `json:"elem,omitempty"`
}

// String 返回带参数的字面形式，例如 decimal(18,2)。
func (t TypeSpec) String() string {
	switch t.Logical {
	case LDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", t.Precision, t.Scale)
		}
		return "decimal"
	case LArray:
		if t.Elem != "" {
			return "array<" + t.Elem + ">"
		}
		return "array"
	case LMap:
		return "map"
	}
	return t.Logical
}

// TypeMapping 是矩阵中的一个单元格。
type TypeMapping struct {
	Physical string `json:"physical"`
	// Lossy 表示该映射会丢失信息（精度、结构、时区）。
	Lossy bool `json:"lossy"`
	// Note 说明损失点，用于报错信息。
	Note string `json:"note,omitempty"`
	// NeedsParam 表示物理类型必须带精度参数。
	NeedsParam bool `json:"needsParam,omitempty"`
}

// 矩阵定义：engine → logical → mapping。
// 新增引擎时必须补全所有逻辑类型，测试 TestMatrixComplete 会强制检查。
var matrix = map[string]map[string]TypeMapping{
	"postgres": {
		LString:   {Physical: "varchar(255)"},
		LText:     {Physical: "text"},
		LInt:      {Physical: "integer"},
		LBigInt:   {Physical: "bigint"},
		LDouble:   {Physical: "double precision"},
		LDecimal:  {Physical: "numeric(%d,%d)", NeedsParam: true},
		LBool:     {Physical: "boolean"},
		LDate:     {Physical: "date"},
		LDatetime: {Physical: "timestamptz"},
		LJSON:     {Physical: "jsonb"},
		LArray:    {Physical: "%s[]"},
		LMap:      {Physical: "jsonb", Lossy: true, Note: "PostgreSQL 无原生 map 类型，退化为 jsonb"},
	},
	"mysql": {
		LString:   {Physical: "varchar(255)"},
		LText:     {Physical: "text"},
		LInt:      {Physical: "int"},
		LBigInt:   {Physical: "bigint"},
		LDouble:   {Physical: "double"},
		LDecimal:  {Physical: "decimal(%d,%d)", NeedsParam: true},
		LBool:     {Physical: "tinyint(1)"},
		LDate:     {Physical: "date"},
		LDatetime: {Physical: "datetime(3)", Lossy: true, Note: "MySQL DATETIME 不带时区，跨时区场景会偏移"},
		LJSON:     {Physical: "json"},
		LArray:    {Physical: "json", Lossy: true, Note: "MySQL 无数组类型，退化为 json"},
		LMap:      {Physical: "json", Lossy: true, Note: "MySQL 无 map 类型，退化为 json"},
	},
	"clickhouse": {
		LString:   {Physical: "String"},
		LText:     {Physical: "String"},
		LInt:      {Physical: "Int32"},
		LBigInt:   {Physical: "Int64"},
		LDouble:   {Physical: "Float64"},
		LDecimal:  {Physical: "Decimal(%d,%d)", NeedsParam: true},
		LBool:     {Physical: "UInt8", Lossy: true, Note: "ClickHouse 用 UInt8 表示布尔，读取时需转换"},
		LDate:     {Physical: "Date"},
		LDatetime: {Physical: "DateTime64(3)"},
		LJSON:     {Physical: "String", Lossy: true, Note: "ClickHouse 的 JSON 以 String 存储，失去结构化查询能力"},
		LArray:    {Physical: "Array(%s)"},
		LMap:      {Physical: "Map(String, String)", Lossy: true, Note: "map 固定为 String→String"},
	},
	"duckdb": {
		LString:   {Physical: "VARCHAR"},
		LText:     {Physical: "VARCHAR"},
		LInt:      {Physical: "INTEGER"},
		LBigInt:   {Physical: "BIGINT"},
		LDouble:   {Physical: "DOUBLE"},
		LDecimal:  {Physical: "DECIMAL(%d,%d)", NeedsParam: true},
		LBool:     {Physical: "BOOLEAN"},
		LDate:     {Physical: "DATE"},
		LDatetime: {Physical: "TIMESTAMP WITH TIME ZONE"},
		LJSON:     {Physical: "JSON"},
		LArray:    {Physical: "%s[]"},
		LMap:      {Physical: "MAP(VARCHAR, VARCHAR)", Lossy: true, Note: "DuckDB map 键固定为 VARCHAR"},
	},
}

// 各引擎 decimal 的最大精度与标度，超出即判为有损。
var decimalLimits = map[string][2]int{
	"postgres":   {38, 20},
	"mysql":      {65, 30},
	"clickhouse": {76, 38},
	"duckdb":     {38, 20},
}

// MapType 把逻辑类型翻译成引擎物理类型。
// 有损时返回 Lossy=true，调用方决定是否放行。
func MapType(engine string, t TypeSpec) (TypeMapping, error) {
	m, ok := matrix[engine]
	if !ok {
		return TypeMapping{}, fmt.Errorf("未知引擎 %q，已支持：%s", engine, strings.Join(MatrixEngines(), ", "))
	}
	base, ok := m[t.Logical]
	if !ok {
		return TypeMapping{}, fmt.Errorf("引擎 %s 不支持逻辑类型 %q", engine, t.Logical)
	}
	out := base

	if t.Logical == LDecimal {
		lim := decimalLimits[engine]
		p, s := t.Precision, t.Scale
		if p == 0 {
			p, s = 20, 4 // 默认精度，与现有编译器保持一致
		}
		if p > lim[0] || s > lim[1] || s > p {
			out.Lossy = true
			out.Note = fmt.Sprintf("decimal(%d,%d) 超出 %s 上限 (%d,%d)，将截断", p, s, engine, lim[0], lim[1])
			if p > lim[0] {
				p = lim[0]
			}
			if s > lim[1] {
				s = lim[1]
			}
		}
		out.Physical = fmt.Sprintf(base.Physical, p, s)
		return out, nil
	}

	if t.Logical == LArray {
		elem := t.Elem
		if elem == "" {
			elem = LString
		}
		sub, err := MapType(engine, TypeSpec{Logical: elem})
		if err != nil {
			return TypeMapping{}, fmt.Errorf("数组元素类型: %w", err)
		}
		if sub.Lossy {
			out.Lossy = true
			out.Note = "数组元素有损: " + sub.Note
		}
		// 不是所有引擎的数组物理类型都带元素占位符：MySQL 直接退化为 json，
		// 无条件 Sprintf 会拼出 "json%!(EXTRA string=...)" 这种非法类型。
		if strings.Contains(base.Physical, "%s") {
			out.Physical = fmt.Sprintf(base.Physical, sub.Physical)
		} else {
			out.Physical = base.Physical
		}
		return out, nil
	}

	return out, nil
}

// ReverseMap 把引擎物理类型还原为逻辑类型，用于元数据采集与 ODS 自动建模。
// 匹配规则按前缀/包含做宽松匹配：引擎类型带参数（varchar(64)）也能识别。
func ReverseMap(engine, physical string) TypeSpec {
	p := strings.ToLower(strings.TrimSpace(physical))
	// 去掉参数，保留主体用于判定。
	body := p
	if i := strings.IndexAny(body, "(<"); i > 0 {
		body = body[:i]
	}

	switch engine {
	case "clickhouse":
		switch {
		case strings.HasPrefix(p, "nullable("):
			return ReverseMap(engine, strings.TrimSuffix(strings.TrimPrefix(p, "nullable("), ")"))
		case strings.HasPrefix(p, "decimal"):
			return TypeSpec{Logical: LDecimal, Precision: 20, Scale: 4}
		case strings.HasPrefix(p, "array"):
			return TypeSpec{Logical: LArray, Elem: LString}
		case strings.HasPrefix(p, "map"):
			return TypeSpec{Logical: LMap}
		case body == "string" || body == "fixedstring":
			return TypeSpec{Logical: LString}
		case strings.HasPrefix(body, "int") || strings.HasPrefix(body, "uint"):
			if strings.Contains(body, "64") || strings.Contains(body, "128") || strings.Contains(body, "256") {
				return TypeSpec{Logical: LBigInt}
			}
			return TypeSpec{Logical: LInt}
		case strings.HasPrefix(body, "float"):
			return TypeSpec{Logical: LDouble}
		case body == "date" || body == "date32":
			return TypeSpec{Logical: LDate}
		case strings.HasPrefix(body, "datetime"):
			return TypeSpec{Logical: LDatetime}
		}
	case "mysql":
		switch {
		case strings.HasPrefix(body, "decimal") || strings.HasPrefix(body, "numeric"):
			return TypeSpec{Logical: LDecimal, Precision: 20, Scale: 4}
		case body == "json":
			return TypeSpec{Logical: LJSON}
		case strings.HasPrefix(body, "tinyint") || body == "bool" || body == "boolean":
			return TypeSpec{Logical: LBool}
		case strings.HasPrefix(body, "bigint"):
			return TypeSpec{Logical: LBigInt}
		case strings.HasPrefix(body, "int") || strings.HasPrefix(body, "mediumint") || strings.HasPrefix(body, "smallint"):
			return TypeSpec{Logical: LInt}
		case strings.HasPrefix(body, "double") || strings.HasPrefix(body, "float") || strings.HasPrefix(body, "real"):
			return TypeSpec{Logical: LDouble}
		case body == "date":
			return TypeSpec{Logical: LDate}
		case strings.HasPrefix(body, "datetime") || strings.HasPrefix(body, "timestamp"):
			return TypeSpec{Logical: LDatetime}
		case strings.HasPrefix(body, "text") || strings.HasPrefix(body, "longtext") || strings.HasPrefix(body, "mediumtext"):
			return TypeSpec{Logical: LText}
		}
	default: // postgres / duckdb
		switch {
		case strings.HasPrefix(body, "numeric") || strings.HasPrefix(body, "decimal"):
			return TypeSpec{Logical: LDecimal, Precision: 20, Scale: 4}
		case body == "json" || body == "jsonb":
			return TypeSpec{Logical: LJSON}
		case body == "bool" || body == "boolean":
			return TypeSpec{Logical: LBool}
		case body == "bigint" || body == "int8":
			return TypeSpec{Logical: LBigInt}
		case body == "int" || body == "integer" || body == "int4" || body == "smallint":
			return TypeSpec{Logical: LInt}
		case body == "double precision" || body == "double" || body == "float8" || body == "real":
			return TypeSpec{Logical: LDouble}
		case body == "date":
			return TypeSpec{Logical: LDate}
		case strings.HasPrefix(body, "timestamp"):
			return TypeSpec{Logical: LDatetime}
		case body == "text" || body == "varchar" || body == "character varying" || body == "char":
			return TypeSpec{Logical: LString}
		}
		if strings.HasSuffix(p, "[]") {
			return TypeSpec{Logical: LArray, Elem: LString}
		}
	}
	return TypeSpec{Logical: LString}
}

// MatrixEngines 返回矩阵覆盖的引擎名，排序输出。
func MatrixEngines() []string {
	out := make([]string, 0, len(matrix))
	for k := range matrix {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MatrixLogicalTypes 返回矩阵覆盖的逻辑类型，排序输出。
func MatrixLogicalTypes() []string {
	out := []string{LString, LText, LInt, LBigInt, LDouble, LDecimal, LBool, LDate, LDatetime, LJSON, LArray, LMap}
	return out
}

// Matrix 导出完整矩阵，供文档生成与前端展示。
func Matrix() map[string]map[string]TypeMapping {
	out := make(map[string]map[string]TypeMapping, len(matrix))
	for eng, m := range matrix {
		cp := make(map[string]TypeMapping, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[eng] = cp
	}
	return out
}
