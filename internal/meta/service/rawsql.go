package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/sqlx"
	"github.com/jackc/pgx/v5/pgtype"
)

// 自助查询（原始 SQL 模式）。
//
// 与数据集查询的分工：
//   - 数据集模式：语义层编译 SQL，行级策略自动注入，列级策略按基表匹配；
//   - 原始模式：用户自己写 SQL，行级策略**无法**注入（不知道目标表），
//     列级策略只能按输出列名匹配。
//
// 这个差异必须写进响应和界面，否则就是给自己埋一个「看起来有 RLS、实际能绕过」的坑。

const rawSQLTimeout = 15 * time.Second

const (
	rawSQLDefaultLimit = 500
)

// RawSQLResult 是一条自助查询的结果。
type RawSQLResult struct {
	Columns   []string          `json:"columns"`
	Rows      [][]any           `json:"rows"`
	RowCount  int               `json:"rowCount"`
	Truncated bool              `json:"truncated"`
	Limit     int               `json:"limit"`
	ElapsedMs int64             `json:"elapsedMs"`
	Masks     map[string]string `json:"masks,omitempty"`
	Notes     []string          `json:"notes"`
}

// RawSQLQuery 执行一条只读的自助查询。
func (s *Service) RawSQLQuery(ctx context.Context, workspaceID, datasource, sqlText, user, role string, limit int) (*RawSQLResult, error) {
	// 先做静态校验，再去拿连接：非法 SQL 不该占用连接池。
	if err := sqlx.ValidateReadOnlySQL(sqlText); err != nil {
		return nil, err
	}
	if datasource == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "需要指定数据源")
	}
	if limit <= 0 {
		limit = rawSQLDefaultLimit
	}
	if limit > sqlx.MaxRawSQLLimit {
		limit = sqlx.MaxRawSQLLimit
	}

	conn, err := s.connectorFor(ctx, workspaceID, datasource)
	if err != nil {
		return nil, err
	}

	// 超时靠 context 取消。驱动会把取消传到数据库侧（pgx/mysql 都会发 cancel 请求），
	// 所以这里不需要再依赖 statement_timeout。
	qctx, cancel := context.WithTimeout(ctx, rawSQLTimeout)
	defer cancel()

	start := time.Now()
	rows, err := conn.Query(qctx, sqlText)
	if err != nil {
		if qctx.Err() != nil {
			return nil, errs.New(errs.CodeExecutionFailed,
				fmt.Sprintf("查询超过 %d 秒已取消，请缩小范围或加 LIMIT", int(rawSQLTimeout.Seconds())))
		}
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "SQL 执行失败")
	}
	defer rows.Close()

	cols := rows.Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}

	data := make([][]any, 0, 64)
	truncated := false
	for rows.Next() {
		if len(data) >= limit {
			// 多取一行才发现超限：说明结果确实还有，标成截断而不是「刚好这么多」。
			truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, errs.Wrap(errs.CodeExecutionFailed, err, "读取结果失败")
		}
		for i := range vals {
			vals[i] = normalizeRawValue(vals[i])
		}
		data = append(data, vals)
	}
	if err := rows.Err(); err != nil && !truncated {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "读取结果失败")
	}

	// 列级脱敏：按输出列名匹配工作区内的全部列策略。
	// 这是原始模式的降级方案——同名列会被一并脱敏，宁可多脱不可漏脱。
	masks := map[string]string{}
	if policies, perr := s.Repo.ListColumnPolicies(ctx, workspaceID, ""); perr == nil && len(policies) > 0 {
		all := ColumnMasks(policies, "")
		MaskRows(names, data, all)
		for _, c := range names {
			if m, ok := all[strings.ToLower(c)]; ok && m != "" && m != "none" {
				masks[c] = m
			}
		}
	}

	notes := []string{
		fmt.Sprintf("只读模式：仅 SELECT / WITH，单语句，超时 %d 秒，最多返回 %d 行。",
			int(rawSQLTimeout.Seconds()), limit),
		"行级策略未注入：原始 SQL 模式无法判断目标表。需要行级隔离请用数据集查询。",
	}
	if len(masks) > 0 {
		notes = append(notes, fmt.Sprintf(
			"列级脱敏按输出列名匹配，命中 %d 列（同名列可能被一并脱敏）。", len(masks)))
	}

	return &RawSQLResult{
		Columns:   names,
		Rows:      data,
		RowCount:  len(data),
		Truncated: truncated,
		Limit:     limit,
		ElapsedMs: time.Since(start).Milliseconds(),
		Masks:     masks,
		Notes:     notes,
	}, nil
}

// normalizeRawValue 把驱动层的原始类型转成前端可读的形式。
//
// 不处理会得到三种「看起来是乱码」的结果：
//   - []byte 走 JSON 会变 base64；
//   - time.Time 会变带时区的 RFC3339，和界面其它时间不是一种写法；
//   - pgx 的 NUMERIC 是结构体，JSON 会序列化成 {"Int":..,"Exp":..}。
//
// 这与 toFloatSafe 的坑是同一个来源：pgx 默认不做类型推断。
func normalizeRawValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.Format("2006-01-02 15:04:05")
	case pgtype.Numeric:
		if f, err := t.Float64Value(); err == nil && f.Valid {
			return f.Float64
		}
		return fmt.Sprint(t)
	}
	return v
}
