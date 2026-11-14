package repo

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB 包装元数据库连接池。业务查询全部经由它，便于统一注入超时与追踪。
type DB struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, cfg config.DBConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "元数据库 DSN 无效")
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 10 * time.Minute
	poolCfg.ConnConfig.ConnectTimeout = 10 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "无法连接元数据库 "+cfg.Redacted())
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errs.Wrap(errs.CodeUnavailable, err, "元数据库不可达 "+cfg.Redacted())
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() { db.Pool.Close() }

func (db *DB) Ping(ctx context.Context) error { return db.Pool.Ping(ctx) }

// Migrate 按文件名顺序执行未应用的迁移。每条迁移在独立事务里执行，
// 失败即停止并保留已成功的版本，可重复执行（幂等）。
func (db *DB) Migrate(ctx context.Context) (applied []string, err error) {
	_, err = db.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return nil, err
	}

	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	for _, name := range entries {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migration WHERE version=$1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			return applied, err
		}
		up := extractUp(string(raw))
		if err := db.execScript(ctx, up); err != nil {
			return applied, fmt.Errorf("迁移 %s 失败: %w", name, err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO schema_migration(version) VALUES($1)`, version); err != nil {
			return applied, err
		}
		applied = append(applied, version)
	}
	return applied, nil
}

// Version 返回已应用的最大迁移版本，/readyz 用它判断 schema 是否最新。
func (db *DB) Version(ctx context.Context) (string, int, error) {
	var v string
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),''), COUNT(*) FROM schema_migration`).Scan(&v, &n); err != nil {
		return "", 0, err
	}
	return v, n, nil
}

// execScript 在单事务内按分号拆分执行。迁移脚本里禁止出现函数体等含分号的语句，
// 需要时改用 StatementBegin 块（见 extractUp 的说明）。
func (db *DB) execScript(ctx context.Context, script string) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range splitStatements(script) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w\n--- SQL ---\n%s", err, truncate(stmt, 800))
		}
	}
	return tx.Commit(ctx)
}

func extractUp(script string) string {
	i := strings.Index(script, "-- +goose Up")
	if i < 0 {
		return script
	}
	rest := script[i+len("-- +goose Up"):]
	if j := strings.Index(rest, "-- +goose Down"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// splitStatements 按分号切分，跳过单引号与注释中的分号。
func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inLineComment := false
	for i := 0; i < len(script); i++ {
		c := script[i]
		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if !inSingle && c == '-' && i+1 < len(script) && script[i+1] == '-' {
			inLineComment = true
			cur.WriteByte(c)
			continue
		}
		if c == '\'' {
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		}
		if c == ';' && !inSingle {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// WithTx 提供元数据操作的原子性。
func (db *DB) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// PendingMigrations 返回尚未应用的迁移版本，供 CLI 的 --check 使用。
func (db *DB) PendingMigrations(ctx context.Context) ([]string, error) {
	_, err := db.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return nil, err
	}
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	var pending []string
	for _, name := range entries {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migration WHERE version=$1)`, version).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			pending = append(pending, version)
		}
	}
	return pending, nil
}
