package sqlx

import (
	"fmt"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0406 六种写入策略。
//
// 幂等是硬要求：同一 bizdate 重跑两次，结果必须一致。
// 因此每个策略都要么自带幂等语义（merge/overwrite_partition/full_refresh），
// 要么在语句序列里显式清理（delete_insert/overwrite），不存在「追加了事」的默认路径。

// Caps 是写入相关能力。为避免 sqlx 依赖 connector，这里用局部结构承接，
// 由调用方从 connector.Capability 转换。
type Caps struct {
	MergeUpsert      bool
	InsertOverwrite  bool
	ReplacePartition bool
	Transactional    bool
}

// WriteMode 是写入策略。与 model.WriteMode 保持一致，但这里定义执行侧的完整集合。
type WriteMode string

const (
	Append             WriteMode = "append"
	Overwrite          WriteMode = "overwrite"
	Merge              WriteMode = "merge"
	SCD2               WriteMode = "scd2"
	FullRefresh        WriteMode = "full_refresh"
	DeleteInsert       WriteMode = "delete_insert"
	OverwritePartition WriteMode = "overwrite_partition"
)

// ValidWriteMode 校验取值。
func ValidWriteMode(m string) bool {
	switch WriteMode(m) {
	case Append, Overwrite, Merge, SCD2, FullRefresh, DeleteInsert, OverwritePartition:
		return true
	}
	return false
}

// WriteSpec 是生成写入语句所需的全部信息。
type WriteSpec struct {
	Mode    WriteMode
	Dialect dialect.Dialect
	Caps    Caps

	// Table 是目标表（已带 schema）。
	Table string
	// Columns 是目标列，顺序与 Select 输出一致。
	Columns []string
	// Keys 是幂等键（merge/scd2/delete_insert 必需）。
	Keys []string
	// Select 是数据来源查询（不含结尾分号）。
	Select string
	// PartitionColumn / PartitionValue 用于分区覆写。
	PartitionColumn string
	PartitionValue  any
	// SCD2 专用：业务日期列与当前标记列。
	SCD2 SCD2Options
}

// SCD2Options 是缓慢变化维的列约定。
type SCD2Options struct {
	StartCol string // 默认 valid_from
	EndCol   string // 默认 valid_to
	CurrCol  string // 默认 is_current
	BizDate  string // 业务日期字面量，如 '2025-11-10'
}

// WritePlan 是语句序列 + 选型理由。语句按顺序执行，任一失败即整体失败。
type WritePlan struct {
	Statements []string `json:"statements"`
	// Path 说明最终走了哪条实现路径，便于排查「为什么这条 SQL 长这样」。
	Path string `json:"path"`
	// InTx 表示必须在事务内执行，否则可能产生中间态。
	InTx bool `json:"inTx"`
	// Warn 是能力降级提示，例如「CH 无原生 MERGE，改用 DELETE+INSERT」。
	Warn []string `json:"warn,omitempty"`
}

// PlanWrite 生成写入计划。能力不足时返回 ErrUnsupported 而不是静默降级到全量覆盖。
func PlanWrite(s WriteSpec) (WritePlan, error) {
	if s.Table == "" || len(s.Columns) == 0 || strings.TrimSpace(s.Select) == "" {
		return WritePlan{}, errs.New(errs.CodeInvalidArgument, "写入计划缺少 table/columns/select")
	}
	q := s.Dialect.QuoteIdent
	insert := fmt.Sprintf("INSERT INTO %s (%s)\n%s", s.Table, joinIdents(s.Columns, q), strings.TrimSpace(s.Select))

	switch s.Mode {
	case Append:
		return WritePlan{Statements: []string{insert}, Path: "append"}, nil

	case Overwrite:
		// TRUNCATE 在部分引擎不可回滚，因此要求事务能力；没有事务就退化为
		// 「先写临时表再原子换名」，比裸 TRUNCATE 安全。
		if s.Caps.Transactional {
			return WritePlan{
				Statements: []string{"TRUNCATE TABLE " + s.Table, insert},
				Path:       "truncate+insert", InTx: true,
			}, nil
		}
		return planSwap(s, insert)

	case FullRefresh:
		return planSwap(s, insert)

	case DeleteInsert:
		if len(s.Keys) == 0 {
			return WritePlan{}, errs.New(errs.CodeModelInvalid, "delete_insert 需要幂等键")
		}
		del := fmt.Sprintf("DELETE FROM %s WHERE (%s) IN (SELECT %s FROM (\n%s\n) godw_src)",
			s.Table, joinIdents(s.Keys, q), joinIdents(s.Keys, q), strings.TrimSpace(s.Select))
		return WritePlan{
			Statements: []string{del, insert},
			Path:       "delete+insert", InTx: true,
		}, nil

	case Merge:
		return planMerge(s, insert)

	case SCD2:
		return planSCD2(s)

	case OverwritePartition:
		return planOverwritePartition(s, insert)
	}
	return WritePlan{}, errs.New(errs.CodeInvalidArgument, "不支持的写入策略: "+string(s.Mode))
}

// planMerge 按引擎选择最优的原生 upsert 实现。
func planMerge(s WriteSpec, insert string) (WritePlan, error) {
	if len(s.Keys) == 0 {
		return WritePlan{}, errs.New(errs.CodeModelInvalid, "merge 需要主键或唯一键")
	}
	q := s.Dialect.QuoteIdent
	keyList := joinIdents(s.Keys, q)
	keySet := map[string]bool{}
	for _, k := range s.Keys {
		keySet[strings.ToLower(k)] = true
	}
	updates := make([]string, 0, len(s.Columns))
	for _, c := range s.Columns {
		if keySet[strings.ToLower(c)] {
			continue
		}
		updates = append(updates, q(c))
	}
	if len(updates) == 0 {
		return WritePlan{}, errs.New(errs.CodeModelInvalid, "merge 的模型除主键外没有可更新列")
	}

	switch s.Dialect.Name() {
	case "postgres", "duckdb":
		// PG/duckdb 的 ON CONFLICT 是原子操作，单语句完成，无需事务。
		var sets []string
		for _, u := range updates {
			sets = append(sets, u+" = EXCLUDED."+u)
		}
		return WritePlan{
			Statements: []string{fmt.Sprintf("%s\nON CONFLICT (%s) DO UPDATE SET %s", insert, keyList, strings.Join(sets, ", "))},
			Path:       "on_conflict",
		}, nil

	case "mysql":
		var sets []string
		for _, u := range updates {
			sets = append(sets, fmt.Sprintf("%s = VALUES(%s)", u, u))
		}
		return WritePlan{
			Statements: []string{fmt.Sprintf("%s\nON DUPLICATE KEY UPDATE %s", insert, strings.Join(sets, ", "))},
			Path:       "on_duplicate_key",
		}, nil

	case "clickhouse":
		// 选型理由：CH 的 ReplacingMergeTree 只在合并时去重，查询必须带 FINAL，
		// 且无法保证「合并何时发生」，对账场景不可接受。
		// 因此这里用 DELETE + INSERT 保证强一致；大表可显式改用 ReplacingMergeTree。
		del := fmt.Sprintf("ALTER TABLE %s DELETE WHERE (%s) IN (SELECT %s FROM (\n%s\n) godw_src)",
			s.Table, keyList, keyList, strings.TrimSpace(s.Select))
		plan := WritePlan{
			Statements: []string{del, insert},
			Path:       "alter_delete+insert",
			Warn:       []string{"ClickHouse 无事务，DELETE 与 INSERT 之间存在短暂空窗；对账敏感场景建议改用 ReplacingMergeTree + FINAL"},
		}
		return plan, nil
	}
	return WritePlan{}, errs.New(errs.CodeInvalidArgument, "引擎 "+s.Dialect.Name()+" 不支持 merge")
}

// planOverwritePartition 三档优先级：INSERT OVERWRITE → REPLACE PARTITION → DELETE+INSERT。
func planOverwritePartition(s WriteSpec, insert string) (WritePlan, error) {
	if s.PartitionColumn == "" || s.PartitionValue == nil {
		return WritePlan{}, errs.New(errs.CodeInvalidArgument, "overwrite_partition 缺少分区列或分区值")
	}
	q := s.Dialect.QuoteIdent
	partCol := q(s.PartitionColumn)
	partVal := Literal(s.PartitionValue)

	switch {
	case s.Caps.InsertOverwrite:
		return WritePlan{
			Statements: []string{fmt.Sprintf("INSERT OVERWRITE TABLE %s (%s)\n%s",
				s.Table, joinIdents(s.Columns, q), strings.TrimSpace(s.Select))},
			Path: "insert_overwrite",
		}, nil

	case s.Caps.ReplacePartition:
		// REPLACE PARTITION 要求数据先落到临时表，再整块替换。
		tmp := tmpName(s.Table, q)
		return WritePlan{
			Statements: []string{
				"DROP TABLE IF EXISTS " + tmp,
				fmt.Sprintf("CREATE TABLE %s AS %s", tmp, s.Table), // 复制结构，不带数据
				fmt.Sprintf("INSERT INTO %s (%s)\n%s", tmp, joinIdents(s.Columns, q), strings.TrimSpace(s.Select)),
				fmt.Sprintf("ALTER TABLE %s REPLACE PARTITION %s FROM %s", s.Table, partVal, tmp),
				"DROP TABLE IF EXISTS " + tmp,
			},
			Path: "replace_partition",
			Warn: []string{"REPLACE PARTITION 依赖临时表，失败时需手动清理 " + tmp},
		}, nil

	case s.Caps.Transactional:
		del := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", s.Table, partCol, partVal)
		if s.Dialect.Name() == "duckdb" {
			// DuckDB 的唯一索引在事务内不回收已删除的键：同一事务里 DELETE 后
			// 立刻 INSERT 相同主键会报 Duplicate key（官方 known index limitations）。
			// 这里退化为两个自动提交事务，牺牲原子性换取「重跑能成功」；
			// 真正的原子替换需要 REPLACE PARTITION，DuckDB 尚不支持。
			return WritePlan{
				Statements: []string{del, insert},
				Path:       "delete+insert(两段提交)",
				Warn:       []string{"DuckDB 事务内无法删除并重建相同主键，已改为两次提交；失败时该分区会短暂为空"},
			}, nil
		}
		return WritePlan{Statements: []string{del, insert}, Path: "delete+insert", InTx: true}, nil
	}
	return WritePlan{}, errs.New(errs.CodeInvalidArgument,
		s.Dialect.Name()+" 既不支持 INSERT OVERWRITE 也不支持 REPLACE PARTITION，且无事务能力；"+
			"请改用 merge 或 delete_insert").
		WithMeta("engine", s.Dialect.Name())
}

// tmpName 在完整表名后追加后缀，保留原有的引号与 schema 限定。
// 直接拼接会得到 `dw`.`tbl`__tmp 这种语法错误的标识符。
func tmpName(table string, q func(string) string) string {
	if table == "" {
		return q("godw_tmp")
	}
	last := table[len(table)-1]
	switch last {
	case '`', '"', ']':
		return table[:len(table)-1] + "__godw_tmp" + string(last)
	}
	return table + "__godw_tmp"
}

// planSCD2 关链 + 开链，必须在事务内，否则会出现「两条 current」。
func planSCD2(s WriteSpec) (WritePlan, error) {
	if !s.Caps.Transactional {
		return WritePlan{}, errs.New(errs.CodeInvalidArgument,
			"scd2 必须在事务内执行，当前引擎不支持事务；请改用 merge 维护维表")
	}
	if len(s.Keys) == 0 {
		return WritePlan{}, errs.New(errs.CodeModelInvalid, "scd2 需要业务主键")
	}
	opt := s.SCD2
	if opt.StartCol == "" {
		opt.StartCol = "valid_from"
	}
	if opt.EndCol == "" {
		opt.EndCol = "valid_to"
	}
	if opt.CurrCol == "" {
		opt.CurrCol = "is_current"
	}
	if opt.BizDate == "" {
		return WritePlan{}, errs.New(errs.CodeInvalidArgument, "scd2 缺少业务日期")
	}
	q := s.Dialect.QuoteIdent
	keyList := joinIdents(s.Keys, q)

	// 关链：只关「值真的变了」的当前版本，避免无变化也产生新版本。
	changed := make([]string, 0, len(s.Columns))
	keySet := map[string]bool{}
	for _, k := range s.Keys {
		keySet[strings.ToLower(k)] = true
	}
	for _, c := range s.Columns {
		if keySet[strings.ToLower(c)] || strings.EqualFold(c, opt.StartCol) ||
			strings.EqualFold(c, opt.EndCol) || strings.EqualFold(c, opt.CurrCol) {
			continue
		}
		changed = append(changed, fmt.Sprintf("t.%s IS DISTINCT FROM src.%s", q(c), q(c)))
	}
	cond := "TRUE"
	if len(changed) > 0 {
		cond = strings.Join(changed, " OR ")
	}
	closeStmt := fmt.Sprintf(
		"UPDATE %s t SET %s = %s, %s = 0 WHERE t.%s = 1 AND (%s) IN (SELECT %s FROM (\n%s\n) x) AND (%s)",
		s.Table, q(opt.EndCol), Literal(opt.BizDate), q(opt.CurrCol), q(opt.CurrCol),
		keyList, keyList, strings.TrimSpace(s.Select), cond)

	// 开链：新版本从今天生效。
	cols := append([]string{}, s.Columns...)
	sel := []string{}
	for _, c := range s.Columns {
		sel = append(sel, q(c))
	}
	cols = append(cols, opt.StartCol, opt.EndCol, opt.CurrCol)
	sel = append(sel, Literal(opt.BizDate), "NULL", "1")
	insert := fmt.Sprintf("INSERT INTO %s (%s)\nSELECT %s FROM (\n%s\n) src",
		s.Table, joinIdents(cols, q), strings.Join(sel, ", "), strings.TrimSpace(s.Select))

	return WritePlan{
		Statements: []string{closeStmt, insert},
		Path:       "scd2",
		InTx:       true,
	}, nil
}

// planSwap 用「建新表 → 灌数 → 原子换名」实现全量刷新。
// 比 TRUNCATE 安全：换名失败时旧表仍在。
func planSwap(s WriteSpec, insert string) (WritePlan, error) {
	bak := s.Table + "__godw_old"
	tmp := s.Table + "__godw_new"
	return WritePlan{
		Statements: []string{
			"DROP TABLE IF EXISTS " + tmp,
			fmt.Sprintf("CREATE TABLE %s AS %s", tmp, s.Table),
			strings.Replace(insert, "INSERT INTO "+s.Table, "INSERT INTO "+tmp, 1),
			"DROP TABLE IF EXISTS " + bak,
			"ALTER TABLE " + s.Table + " RENAME TO " + unqualified(bak),
			"ALTER TABLE " + tmp + " RENAME TO " + unqualified(s.Table),
			"DROP TABLE IF EXISTS " + bak,
		},
		Path: "swap",
		Warn: []string{"全量刷新在大表上会翻倍占用存储；请确认磁盘余量"},
	}, nil
}

func unqualified(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[i+1:]
	}
	return table
}

func joinIdents(cols []string, quote func(string) string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quote(c)
	}
	return strings.Join(out, ", ")
}

// Literal 生成 SQL 字面量。仅供写入策略内部使用，值来自服务端推导而非用户输入。
func Literal(v any) string {
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

var _ = dialect.Postgres{} // 保证 dialect 包在本文件被显式引用，避免循环依赖误判
