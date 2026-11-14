package modelc

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate 做与引擎无关的语义校验。错误一次性收集，便于用户一次改完。
func Validate(spec model.ModelSpec) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if spec.Kind != "" && spec.Kind != "Model" {
		add("kind 必须为 Model，实际 %q", spec.Kind)
	}
	if spec.Name == "" {
		add("name 不能为空")
	} else if !identRe.MatchString(spec.Name) {
		add("name 只能包含字母、数字、下划线，实际 %q", spec.Name)
	}
	if !model.ValidLayer(spec.Layer) {
		add("layer 必须为 ods/dwd/dws/ads/dim 之一，实际 %q", spec.Layer)
	}
	if spec.Datasource == "" {
		add("datasource 不能为空")
	}
	if len(spec.Columns) == 0 {
		add("columns 不能为空")
	}

	seen := map[string]bool{}
	for i, c := range spec.Columns {
		if c.Name == "" {
			add("columns[%d].name 不能为空", i)
			continue
		}
		if !identRe.MatchString(c.Name) {
			add("列名 %q 非法", c.Name)
		}
		if seen[strings.ToLower(c.Name)] {
			add("列名重复: %s", c.Name)
		}
		seen[strings.ToLower(c.Name)] = true
		if c.Type == "" {
			add("列 %s 缺少 type", c.Name)
		} else if _, err := parseTypeSpec(c.Type); err != nil {
			add("列 %s 的类型 %q 不受支持: %v", c.Name, c.Type, err)
		}
	}

	for _, k := range spec.PrimaryKey {
		if !seen[strings.ToLower(k)] {
			add("primary_key 引用了不存在的列: %s", k)
		}
	}
	for _, k := range spec.UniqueKey {
		if !seen[strings.ToLower(k)] {
			add("unique_key 引用了不存在的列: %s", k)
		}
	}
	for _, k := range spec.PartitionBy {
		if !seen[strings.ToLower(k)] {
			add("partition_by 引用了不存在的列: %s", k)
		}
	}
	for _, idx := range spec.Indexes {
		if len(idx.Columns) == 0 {
			add("indexes 存在空列定义")
			continue
		}
		for _, c := range idx.Columns {
			if !seen[strings.ToLower(c)] {
				add("索引引用了不存在的列: %s", c)
			}
		}
	}
	for _, c := range spec.BucketBy {
		if !seen[strings.ToLower(c)] {
			add("bucket_by 引用了不存在的列: %s", c)
		}
	}
	if spec.Buckets < 0 {
		add("buckets 不能为负数")
	}
	if !ValidMat(spec.Materialization) {
		add("materialization 必须为 table/view/mv/incremental，实际 %q", spec.Materialization)
	}
	if spec.TTL != "" {
		if _, err := ParseTTL(spec.TTL); err != nil {
			add("%v", err)
		}
	}
	if (spec.Materialization == "view" || spec.Materialization == "mv") && strings.TrimSpace(spec.Query) == "" {
		add("%s 必须提供 query", spec.Materialization)
	}
	for _, v := range ValidateStorage(spec) {
		problems = append(problems, v.Message)
	}

	if len(problems) > 0 {
		return errs.New(errs.CodeModelInvalid, "模型定义不合法").WithMeta("problems", problems)
	}
	return nil
}

// ValidateTypes 校验逻辑类型在目标引擎上是否可用，并在有损映射时要求显式确认。
// 与 Validate 分开：类型映射依赖具体引擎，而 Validate 是引擎无关的。
func ValidateTypes(spec model.ModelSpec, engine string) []RuleViolation {
	var out []RuleViolation
	for _, c := range spec.Columns {
		ts, err := parseTypeSpec(c.Type)
		if err != nil {
			out = append(out, RuleViolation{Rule: "type_syntax", Field: "columns." + c.Name, Message: err.Error()})
			continue
		}
		m, err := dialect.MapType(engine, ts)
		if err != nil {
			out = append(out, RuleViolation{Rule: "type_unsupported", Field: "columns." + c.Name, Message: err.Error()})
			continue
		}
		// _godw_ 前缀是平台自己注入的审计列（_godw_src / _godw_load_at），
		// 写入口径由平台固定为 UTC，取舍是平台的既成事实，不该每建一张表就让用户
		// 点一次 allow_lossy；用户自己声明的列照旧必须显式确认。
		if m.Lossy && !spec.AllowLossy && !isPlatformColumn(c.Name) {
			out = append(out, RuleViolation{
				Rule: "lossy_type", Field: "columns." + c.Name,
				Message: fmt.Sprintf("列 %s 的类型 %s 映射到 %s 会丢失信息（%s）；确认可接受后设置 allow_lossy: true",
					c.Name, ts.String(), m.Physical, m.Note),
			})
		}
	}
	return out
}

// isPlatformColumn 判断是否为平台注入的列（_godw_ 前缀）。
func isPlatformColumn(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "_godw_")
}

// parseTypeSpec 把十进制类型字符串解析成结构化 TypeSpec。
func parseTypeSpec(raw string) (dialect.TypeSpec, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return dialect.TypeSpec{}, fmt.Errorf("类型不能为空")
	}
	if strings.HasPrefix(s, "decimal(") && strings.HasSuffix(s, ")") {
		inner := s[len("decimal(") : len(s)-1]
		parts := strings.Split(inner, ",")
		if len(parts) == 2 {
			p, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			sc, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 != nil || err2 != nil || p <= 0 || sc < 0 || sc > p {
				return dialect.TypeSpec{}, fmt.Errorf("decimal 精度格式非法: %s", raw)
			}
			return dialect.TypeSpec{Logical: dialect.LDecimal, Precision: p, Scale: sc}, nil
		}
	}
	if strings.HasPrefix(s, "array<") && strings.HasSuffix(s, ">") {
		elem := s[len("array<") : len(s)-1]
		if !validLogical(elem) && !strings.HasPrefix(elem, "decimal(") {
			return dialect.TypeSpec{}, fmt.Errorf("数组元素类型不受支持: %s", elem)
		}
		return dialect.TypeSpec{Logical: dialect.LArray, Elem: elem}, nil
	}
	if s == "map" || strings.HasPrefix(s, "map<") {
		return dialect.TypeSpec{Logical: dialect.LMap}, nil
	}
	if s == "decimal" {
		return dialect.TypeSpec{Logical: dialect.LDecimal}, nil
	}
	if !validLogical(s) {
		return dialect.TypeSpec{}, fmt.Errorf("不受支持的类型: %s", raw)
	}
	return dialect.TypeSpec{Logical: s}, nil
}

func validLogical(t string) bool {
	switch t {
	case dialect.LString, dialect.LText, dialect.LInt, dialect.LBigInt, dialect.LDecimal,
		dialect.LDouble, dialect.LBool, dialect.LDate, dialect.LDatetime, dialect.LJSON:
		return true
	}
	return false
}

// Compile 生成建表 DDL。输出是纯函数结果：同一 spec + 同一方言必然得到同一份 SQL，
// 快照存库后即可保证历史重跑不漂移。
func Compile(spec model.ModelSpec, d dialect.Dialect) (string, error) {
	if err := Validate(spec); err != nil {
		return "", err
	}
	table := qualify(spec.Schema, spec.TableName(), d)
	var b strings.Builder

	// 目标 schema 不存在时建表会失败，先幂等创建。
	// 注意各引擎的措辞不同：ClickHouse 没有 SCHEMA 关键字（只有 DATABASE），
	// 写成 CREATE SCHEMA 会在真实集群上直接语法错误；MySQL 的 SCHEMA 等同 DATABASE，跳过。
	if spec.Schema != "" {
		switch d.Name() {
		case "postgres", "duckdb":
			b.WriteString("CREATE SCHEMA IF NOT EXISTS " + d.QuoteIdent(spec.Schema) + ";\n")
		case "clickhouse":
			b.WriteString("CREATE DATABASE IF NOT EXISTS " + d.QuoteIdent(spec.Schema) + ";\n")
		}
	}

	switch d.Name() {
	case "clickhouse":
		b.WriteString("CREATE TABLE IF NOT EXISTS " + table + " (\n")
		for i, c := range spec.Columns {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString("  " + d.QuoteIdent(c.Name) + " " + chType(c, d))
			if c.Comment != "" {
				b.WriteString(" COMMENT " + strLit(c.Comment))
			}
		}
		b.WriteString("\n) ENGINE = MergeTree()")
		order := spec.PrimaryKey
		if len(order) == 0 {
			order = spec.UniqueKey
		}
		if len(order) == 0 && len(spec.Columns) > 0 {
			order = []string{spec.Columns[0].Name}
		}
		b.WriteString(" ORDER BY (" + identList(order, d) + ")")
		if len(spec.PartitionBy) > 0 {
			parts := make([]string, 0, len(spec.PartitionBy))
			for _, raw := range spec.PartitionBy {
				ps, err := ParsePartition(raw)
				if err != nil {
					return "", err
				}
				sql, err := CompilePartition(ps, d)
				if err != nil {
					return "", err
				}
				parts = append(parts, sql)
			}
			b.WriteString(" PARTITION BY (" + strings.Join(parts, ", ") + ")")
		}
		if ttl := mustTTL(spec.TTL); ttl.Raw != "" && len(spec.PartitionBy) > 0 {
			ps, _ := ParsePartition(spec.PartitionBy[0])
			if clause := CompileTTL(ttl, ps.Column, d); clause != "" {
				b.WriteString(" " + clause)
			}
		}
		b.WriteString(";\n")
	default:
		b.WriteString("CREATE TABLE IF NOT EXISTS " + table + " (\n")
		for i, c := range spec.Columns {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString("  " + d.QuoteIdent(c.Name) + " " + physType(c.Type, d))
			if !c.Nullable {
				b.WriteString(" NOT NULL")
			}
			if c.Default != "" {
				b.WriteString(" DEFAULT " + c.Default)
			}
		}
		if len(spec.PrimaryKey) > 0 {
			b.WriteString(",\n  CONSTRAINT " + d.QuoteIdent("pk_"+spec.Name) +
				" PRIMARY KEY (" + identList(spec.PrimaryKey, d) + ")")
		}
		b.WriteString("\n);\n")

		if d.Name() == "postgres" {
			if spec.Description != "" {
				b.WriteString("COMMENT ON TABLE " + table + " IS " + strLit(spec.Description) + ";\n")
			}
			for _, c := range spec.Columns {
				if c.Comment != "" {
					b.WriteString("COMMENT ON COLUMN " + table + "." + d.QuoteIdent(c.Name) + " IS " + strLit(c.Comment) + ";\n")
				}
			}
		}
	}

	// 索引：ClickHouse 由 ORDER BY 承担，不额外建 B-Tree。
	if d.Name() != "clickhouse" {
		for _, idx := range spec.Indexes {
			name := idx.Name
			if name == "" {
				name = "idx_" + spec.Name + "_" + strings.Join(idx.Columns, "_")
			}
			b.WriteString("CREATE ")
			if idx.Unique {
				b.WriteString("UNIQUE ")
			}
			b.WriteString("INDEX IF NOT EXISTS " + d.QuoteIdent(name) + " ON " + table +
				" (" + identList(idx.Columns, d) + ");\n")
		}
	}
	return b.String(), nil
}

// CompileAll 生成多语句脚本，供 CLI apply 使用。
func CompileAll(specs []model.ModelSpec, d dialect.Dialect) (string, error) {
	// 按分层排序：ODS → DIM → DWD → DWS → ADS，保证建表顺序符合依赖直觉。
	rank := map[string]int{"ods": 0, "dim": 1, "dwd": 2, "dws": 3, "ads": 4}
	sorted := make([]model.ModelSpec, len(specs))
	copy(sorted, specs)
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Layer] < rank[sorted[j].Layer] })

	var b strings.Builder
	for _, s := range sorted {
		ddl, err := Compile(s, d)
		if err != nil {
			return "", err
		}
		b.WriteString("-- " + s.Layer + "." + s.Name + "\n")
		b.WriteString(ddl)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func chType(c model.ColumnSpec, d dialect.Dialect) string {
	ts, err := parseTypeSpec(c.Type)
	if err != nil {
		ts = dialect.TypeSpec{Logical: dialect.LString}
	}
	m, err := dialect.MapType(d.Name(), ts)
	phys := m.Physical
	if err != nil || phys == "" {
		phys = d.CastType(c.Type)
	}
	if c.Nullable {
		return "Nullable(" + phys + ")"
	}
	return phys
}

func mustTTL(raw string) TTL {
	t, err := ParseTTL(raw)
	if err != nil {
		return TTL{}
	}
	return t
}

// physType 优先走类型矩阵（支持 decimal(p,s) / array<T> / map），
// 矩阵不认识时回退方言的默认映射，保证旧模型仍能编译。
func physType(logical string, d dialect.Dialect) string {
	ts, err := parseTypeSpec(logical)
	if err != nil {
		return d.CastType(logical)
	}
	m, err := dialect.MapType(d.Name(), ts)
	if err != nil || m.Physical == "" {
		return d.CastType(ts.Logical)
	}
	return m.Physical
}

func qualify(schema, table string, d dialect.Dialect) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

func identList(cols []string, d dialect.Dialect) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = d.QuoteIdent(c)
	}
	return strings.Join(out, ", ")
}

func strLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ValidateViolations 以结构化形式返回校验问题，供设计器逐字段定位。
// 与 Validate 共用同一套判定逻辑，避免「接口说合法、发布又失败」。
func ValidateViolations(spec model.ModelSpec) []RuleViolation {
	err := Validate(spec)
	if err == nil {
		return nil
	}
	ae, ok := err.(*errs.AppError)
	if !ok {
		return []RuleViolation{{Rule: "invalid", Field: "", Message: err.Error()}}
	}
	problems, _ := ae.Meta["problems"].([]string)
	out := make([]RuleViolation, 0, len(problems))
	for _, p := range problems {
		out = append(out, RuleViolation{Rule: "schema", Field: "spec", Message: p})
	}
	return out
}
