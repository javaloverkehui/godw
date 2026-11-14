package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	connector.Register("postgres", New)
}

type Conn struct {
	pool *pgxpool.Pool
	spec connector.Spec
}

func New(ctx context.Context, spec connector.Spec) (connector.Connector, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		spec.Get("user", "postgres"), spec.Secret["password"],
		spec.Get("host", "127.0.0.1"), spec.GetInt("port", 5432),
		spec.Get("database", "postgres"), spec.Get("sslmode", "disable"))

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDatasourceAuth, err, "DSN 解析失败")
	}
	cfg.MaxConns = int32(spec.GetInt("max_conns", 10))
	cfg.MinConns = int32(spec.GetInt("min_conns", 1))
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{pool: pool, spec: spec}, nil
}

func (c *Conn) Type() string { return "postgres" }

func (c *Conn) Capabilities() connector.Capability {
	return connector.Capability{
		MergeUpsert: true, TransactionalDDL: true, MaterializedView: true,
		WindowFunction: true, ApproxCountDistinct: true, JSONType: true, BulkLoad: true,
	}
}

func (c *Conn) Dialect() dialect.Dialect { return dialect.Postgres{} }

func (c *Conn) Test(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.pool.Ping(ctx)
}

func (c *Conn) ListSchemas(ctx context.Context) ([]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('pg_catalog','information_schema','pg_toast')
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
	const q = `
SELECT c.relname,
       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table'
                      WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view'
                      ELSE 'other' END,
       COALESCE(obj_description(c.oid), ''),
       GREATEST(c.reltuples, 0)::bigint,
       pg_total_relation_size(c.oid)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r','v','m','p')
ORDER BY c.relname`
	rows, err := c.pool.Query(ctx, q, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []connector.TableMeta{}
	for rows.Next() {
		var t connector.TableMeta
		if err := rows.Scan(&t.Name, &t.Type, &t.Comment, &t.RowCount, &t.SizeBytes); err != nil {
			return nil, err
		}
		t.Schema = schema
		out = append(out, t)
	}
	return out, rows.Err()
}

func (c *Conn) DescribeTable(ctx context.Context, schema, table string) (*connector.TableMeta, error) {
	const q = `
SELECT column_name, data_type, is_nullable = 'YES', column_default,
       COALESCE(col_description(format('%I.%I', table_schema, table_name)::regclass::oid, ordinal_position), ''),
       ordinal_position
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`
	rows, err := c.pool.Query(ctx, q, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &connector.TableMeta{Schema: schema, Name: table, Type: "table"}
	for rows.Next() {
		var col connector.ColumnMeta
		var def *string
		if err := rows.Scan(&col.Name, &col.Type, &col.Nullable, &def, &col.Comment, &col.Ordinal); err != nil {
			return nil, err
		}
		col.Default = def
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

func (c *Conn) Query(ctx context.Context, sql string, args ...any) (connector.Rows, error) {
	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgRows{rows: rows, cols: toColumns(rows.FieldDescriptions())}, nil
}

func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (connector.Result, error) {
	start := time.Now()
	tag, err := c.pool.Exec(ctx, sql, args...)
	if err != nil {
		return connector.Result{Duration: time.Since(start)}, err
	}
	return connector.Result{RowsAffected: tag.RowsAffected(), Duration: time.Since(start)}, nil
}

func (c *Conn) Stream(ctx context.Context, sql string, batchSize int, fn func(connector.Rows) error) error {
	rows, err := c.Query(ctx, sql)
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

// WithTx 在单事务内执行多条语句，任一步失败整体回滚。
func (c *Conn) WithTx(ctx context.Context, fn func(exec func(string, ...any) (connector.Result, error)) error) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	exec := func(sqlText string, args ...any) (connector.Result, error) {
		start := time.Now()
		tag, err := tx.Exec(ctx, sqlText, args...)
		if err != nil {
			return connector.Result{Duration: time.Since(start)}, err
		}
		return connector.Result{RowsAffected: tag.RowsAffected(), Duration: time.Since(start)}, nil
	}
	if err := fn(exec); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func (c *Conn) Close() error {
	c.pool.Close()
	return nil
}

type pgRows struct {
	rows pgx.Rows
	cols []connector.ColumnMeta
}

func toColumns(fds []pgconn.FieldDescription) []connector.ColumnMeta {
	out := make([]connector.ColumnMeta, len(fds))
	for i, fd := range fds {
		out[i] = connector.ColumnMeta{
			Name:    fd.Name,
			Type:    fmt.Sprint(fd.DataTypeOID),
			Ordinal: i + 1,
		}
	}
	return out
}

func (r *pgRows) Columns() []connector.ColumnMeta { return r.cols }
func (r *pgRows) Next() bool                      { return r.rows.Next() }
func (r *pgRows) Scan(dest ...any) error          { return r.rows.Scan(dest...) }
func (r *pgRows) Err() error                      { return r.rows.Err() }
func (r *pgRows) Close() error                    { r.rows.Close(); return nil }
