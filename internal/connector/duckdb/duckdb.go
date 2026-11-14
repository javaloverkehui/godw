//go:build duckdb

// Package duckdb 实现嵌入式 DuckDB 连接器。
//
// 默认不编译：DuckDB 驱动依赖 CGO 与本地动态库，放进默认构建会让
// 「go build ./...」在没装 libduckdb 的机器上直接失败。
// 需要时显式开启：go build -tags duckdb。
package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	duckdb "github.com/marcboeker/go-duckdb"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

func init() {
	connector.Register("duckdb", New)
}

type Conn struct {
	db   *sql.DB
	path string
}

func New(ctx context.Context, spec connector.Spec) (connector.Connector, error) {
	path := spec.Get("path", ":memory:")
	// 只读副本场景下要显式加 access_mode，否则并发写会互相锁死。
	dsn := path
	if ro := spec.Get("read_only", ""); ro == "true" {
		dsn += "?access_mode=read_only"
	}
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDatasourceUnreachable, err, "打开 DuckDB 失败")
	}
	// DuckDB 单文件写入不支持高并发，连接数必须压住。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	return &Conn{db: db, path: path}, nil
}

func (c *Conn) Type() string { return "duckdb" }

// Capabilities：DuckDB 单机能力最全，但物化视图与 REPLACE PARTITION 不稳定，
// 这里按「实际验证过」声明，不为了好看全打 true。
func (c *Conn) Capabilities() connector.Capability {
	return connector.Capability{
		MergeUpsert: true, TransactionalDDL: true, WindowFunction: true,
		ApproxCountDistinct: true, JSONType: true, BulkLoad: true,
	}
}

func (c *Conn) Dialect() dialect.Dialect { return dialect.DuckDB{} }

func (c *Conn) Test(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.db.PingContext(ctx)
}

func (c *Conn) ListSchemas(ctx context.Context) ([]string, error) {
	// 只取当前库：DuckDB 的系统视图会把每个 attached 库的同名 schema 都列出来，
	// 不去重会对着 main 采三遍。
	rows, err := c.db.QueryContext(ctx,
		`SELECT DISTINCT schema_name FROM information_schema.schemata
		 WHERE catalog_name = current_database()
		   AND schema_name NOT IN ('information_schema','pg_catalog')
		 ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (c *Conn) ListTables(ctx context.Context, schema string) ([]connector.TableMeta, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT table_schema, table_name, table_type
		 FROM information_schema.tables
		 WHERE table_schema = COALESCE(NULLIF(?, ''), 'main')
		 ORDER BY table_name`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []connector.TableMeta{}
	for rows.Next() {
		var t connector.TableMeta
		var typ string
		if err := rows.Scan(&t.Schema, &t.Name, &typ); err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToUpper(typ), "VIEW") {
			t.Type = "view"
		} else {
			t.Type = "table"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (c *Conn) DescribeTable(ctx context.Context, schema, table string) (*connector.TableMeta, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT column_name, data_type, is_nullable, column_default, ordinal_position
		 FROM information_schema.columns
		 WHERE table_schema = COALESCE(NULLIF(?, ''), 'main') AND table_name = ?
		 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	meta := &connector.TableMeta{Schema: schema, Name: table, Type: "table"}
	for rows.Next() {
		var col connector.ColumnMeta
		// column_default 为 NULL 是常态（没有默认值的列），扫进 string 会报
		// 「converting NULL to string is unsupported」，整张表就被判采集失败。
		var nullable, def sql.NullString
		if err := rows.Scan(&col.Name, &col.Type, &nullable, &def, &col.Ordinal); err != nil {
			return nil, err
		}
		col.Nullable = nullable.Valid && strings.EqualFold(nullable.String, "YES")
		if def.Valid && def.String != "" {
			d := def.String
			col.Default = &d
		}
		meta.Columns = append(meta.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(meta.Columns) == 0 {
		return nil, errs.New(errs.CodeNotFound, fmt.Sprintf("表不存在: %s.%s", schema, table))
	}
	return meta, nil
}

func (c *Conn) Query(ctx context.Context, sqlText string, args ...any) (connector.Rows, error) {
	rows, err := c.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return &duckRows{rows: rows}, nil
}

func (c *Conn) Exec(ctx context.Context, sqlText string, args ...any) (connector.Result, error) {
	start := time.Now()
	res, err := c.db.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return connector.Result{}, err
	}
	n, _ := res.RowsAffected()
	return connector.Result{RowsAffected: n, Duration: time.Since(start)}, nil
}

func (c *Conn) Stream(ctx context.Context, sqlText string, batchSize int, fn func(connector.Rows) error) error {
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return err
	}
	defer rows.Close()
	return fn(rows)
}

func (c *Conn) Close() error { return c.db.Close() }

// WithTx 实现 connector.Transactor。DuckDB 支持事务，写入策略可走原子路径。
func (c *Conn) WithTx(ctx context.Context, fn func(exec func(string, ...any) (connector.Result, error)) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	exec := func(sqlText string, args ...any) (connector.Result, error) {
		start := time.Now()
		res, eerr := tx.ExecContext(ctx, sqlText, args...)
		if eerr != nil {
			return connector.Result{}, eerr
		}
		n, _ := res.RowsAffected()
		return connector.Result{RowsAffected: n, Duration: time.Since(start)}, nil
	}
	if err := fn(exec); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// duckRows 适配 database/sql.Rows。
type duckRows struct {
	rows    *sql.Rows
	columns []connector.ColumnMeta
}

func (r *duckRows) Columns() []connector.ColumnMeta {
	if r.columns != nil {
		return r.columns
	}
	names, err := r.rows.Columns()
	if err != nil {
		return nil
	}
	types, _ := r.rows.ColumnTypes()
	out := make([]connector.ColumnMeta, len(names))
	for i, n := range names {
		out[i] = connector.ColumnMeta{Name: n, Ordinal: i + 1}
		if types != nil && i < len(types) {
			out[i].Type = types[i].DatabaseTypeName()
			if nullable, ok := types[i].Nullable(); ok {
				out[i].Nullable = nullable
			}
		}
	}
	r.columns = out
	return out
}

func (r *duckRows) Next() bool { return r.rows.Next() }

func (r *duckRows) Scan(dest ...any) error { return scanRows(r.rows, dest...) }

// formatDecimal 输出精确的定点字符串，避免转 float64 丢精度。
func formatDecimal(d duckdb.Decimal) string {
	if d.Value == nil {
		return "0"
	}
	abs := new(big.Int).Abs(d.Value)
	s := abs.String()
	if sc := int(d.Scale); sc > 0 {
		for len(s) <= sc {
			s = "0" + s
		}
		s = s[:len(s)-sc] + "." + s[len(s)-sc:]
	}
	if d.Value.Sign() < 0 {
		return "-" + s
	}
	return s
}
func (r *duckRows) Err() error   { return r.rows.Err() }
func (r *duckRows) Close() error { return r.rows.Close() }

var _ = os.Getenv
