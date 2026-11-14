package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

func init() {
	connector.Register("mysql", New)
}

type Conn struct{ db *sql.DB }

func New(ctx context.Context, spec connector.Spec) (connector.Connector, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s",
		spec.Get("user", "root"), spec.Secret["password"],
		spec.Get("host", "127.0.0.1"), spec.GetInt("port", 3306),
		spec.Get("database", ""))

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDatasourceAuth, err, "MySQL DSN 无效")
	}
	db.SetMaxOpenConns(spec.GetInt("max_conns", 10))
	db.SetMaxIdleConns(spec.GetInt("min_conns", 2))
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Conn{db: db}, nil
}

func (c *Conn) Type() string { return "mysql" }

func (c *Conn) Capabilities() connector.Capability {
	return connector.Capability{
		MergeUpsert: true, TransactionalDDL: false, WindowFunction: true,
		ApproxCountDistinct: true, JSONType: true,
	}
}

func (c *Conn) Dialect() dialect.Dialect { return dialect.MySQL{} }

func (c *Conn) Test(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.db.PingContext(ctx)
}

func (c *Conn) ListSchemas(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema','performance_schema','mysql','sys') ORDER BY schema_name`)
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
	const q = `SELECT table_name, table_type, COALESCE(table_comment,''),
		COALESCE(table_rows,0), COALESCE(data_length+index_length,0)
		FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name`
	rows, err := c.db.QueryContext(ctx, q, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []connector.TableMeta{}
	for rows.Next() {
		var t connector.TableMeta
		var typ string
		if err := rows.Scan(&t.Name, &typ, &t.Comment, &t.RowCount, &t.SizeBytes); err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToUpper(typ), "VIEW") {
			t.Type = "view"
		} else {
			t.Type = "table"
		}
		t.Schema = schema
		out = append(out, t)
	}
	return out, rows.Err()
}

func (c *Conn) DescribeTable(ctx context.Context, schema, table string) (*connector.TableMeta, error) {
	const q = `SELECT column_name, column_type, is_nullable='YES', column_default, column_comment, ordinal_position
		FROM information_schema.columns WHERE table_schema=? AND table_name=? ORDER BY ordinal_position`
	rows, err := c.db.QueryContext(ctx, q, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &connector.TableMeta{Schema: schema, Name: table, Type: "table"}
	for rows.Next() {
		var col connector.ColumnMeta
		var def sql.NullString
		if err := rows.Scan(&col.Name, &col.Type, &col.Nullable, &def, &col.Comment, &col.Ordinal); err != nil {
			return nil, err
		}
		if def.Valid {
			col.Default = &def.String
		}
		t.Columns = append(t.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.Columns) == 0 {
		return nil, errs.New(errs.CodeNotFound, "表不存在: "+schema+"."+table)
	}
	return t, nil
}

func (c *Conn) Query(ctx context.Context, sqlText string, args ...any) (connector.Rows, error) {
	rows, err := c.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	cols, err := columnsOf(rows)
	if err != nil {
		rows.Close()
		return nil, err
	}
	return &sqlRows{rows: rows, cols: cols}, nil
}

func (c *Conn) Exec(ctx context.Context, sqlText string, args ...any) (connector.Result, error) {
	start := time.Now()
	res, err := c.db.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return connector.Result{Duration: time.Since(start)}, err
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

	cols := rows.Columns()
	var batch [][]any
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := fn(connector.NewMemRows(cols, batch))
		batch = nil
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		batch = append(batch, vals)
		if batchSize > 0 && len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// WithTx 在单事务内执行多条语句。
func (c *Conn) WithTx(ctx context.Context, fn func(exec func(string, ...any) (connector.Result, error)) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	exec := func(sqlText string, args ...any) (connector.Result, error) {
		start := time.Now()
		res, err := tx.ExecContext(ctx, sqlText, args...)
		if err != nil {
			return connector.Result{Duration: time.Since(start)}, err
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

func (c *Conn) Close() error { return c.db.Close() }

func columnsOf(rows *sql.Rows) ([]connector.ColumnMeta, error) {
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	out := make([]connector.ColumnMeta, len(types))
	for i, t := range types {
		nullable, _ := t.Nullable()
		out[i] = connector.ColumnMeta{Name: t.Name(), Type: t.DatabaseTypeName(), Nullable: nullable, Ordinal: i + 1}
	}
	return out, nil
}

type sqlRows struct {
	rows *sql.Rows
	cols []connector.ColumnMeta
}

func (r *sqlRows) Columns() []connector.ColumnMeta { return r.cols }
func (r *sqlRows) Next() bool                      { return r.rows.Next() }
func (r *sqlRows) Scan(dest ...any) error          { return r.rows.Scan(dest...) }
func (r *sqlRows) Err() error                      { return r.rows.Err() }
func (r *sqlRows) Close() error                    { return r.rows.Close() }
