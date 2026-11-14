// Package testsupport 提供单元测试所需的真实依赖夹具。
//
// 元数据库相关的用例（repo / service / http）都要连真实的 PostgreSQL：
// 这些代码的价值几乎全在 SQL 上，用 mock 断言 SQL 字符串只能证明「写出来的
// 是同一串字符」，证明不了「这条 SQL 在库里跑得通」。因此这里统一做三件事：
//
//  1. 为每个测试包建一个独占 schema，互不干扰，可以并行跑；
//  2. 在该 schema 内执行正式迁移，保证测试对着真实表结构跑；
//  3. 提供 Reset 截断数据，用例之间不共享状态。
//
// 连不上库时整包跳过（返回码 0），这样没有 PG 的机器不会红；CI 与本机开发
// 环境都通过 GODW_TEST_PG 指定 DSN。
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultDSN 与本机 / CI 的 postgres 服务保持一致。
const DefaultDSN = "postgres://godw:godw@127.0.0.1:5432/godw?sslmode=disable"

// DSN 返回测试库连接串。优先 GODW_TEST_PG，便于 CI 指向临时服务。
func DSN() string {
	if v := os.Getenv("GODW_TEST_PG"); v != "" {
		return v
	}
	return DefaultDSN
}

// Config 返回一份可直接构造 service 的配置：密钥、工作目录、导出目录都指向临时位置。
func Config(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Env = "test"
	cfg.DB.Host = "127.0.0.1"
	cfg.Auth.JWTSecret = "test-secret-at-least-16-bytes"
	cfg.Auth.TokenTTLMin = 60
	cfg.Auth.BootstrapAdmin = "admin@test.local"
	cfg.Auth.BootstrapPass = "test-pass"
	cfg.Executor.WorkDir = t.TempDir()
	cfg.Semantic.ExportDir = t.TempDir()
	cfg.Semantic.CacheTTLSec = 0
	cfg.Scheduler.Enabled = false
	cfg.Log.Level = "error"
	cfg.Log.Format = "text"
	return cfg
}

// sharedDB 是 TestMain 建立、整包共用的一份库。
var sharedDB *repo.DB

// Main 供测试包的 TestMain 调用：建 schema、跑迁移、跑用例、清理。
//
//	func TestMain(m *testing.M) { os.Exit(testsupport.Main(m)) }
//
// 注意 Main 内部会执行 m.Run()，所以用例不能等它的返回值——需要共享库时
// 用 Shared(t)，它读到的是 Main 在跑用例之前建好的那一份。
func Main(m *testing.M) int {
	db, cleanup, err := newSchemaDB(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过：测试库不可用（%v）\n", err)
		return 0
	}
	sharedDB = db
	code := m.Run()
	cleanup()
	return code
}

// Shared 返回 TestMain 建立的整包共享库。没调 Main 或库不可用时跳过。
func Shared(t *testing.T) *repo.DB {
	t.Helper()
	if sharedDB == nil {
		t.Skip("测试库不可用")
	}
	return sharedDB
}

// DB 返回本用例独占的库，用例结束自动删除 schema。
// 需要跨用例共享数据时用 Main。
func DB(t *testing.T) *repo.DB {
	t.Helper()
	db, cleanup, err := newSchemaDB(context.Background())
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	t.Cleanup(cleanup)
	return db
}

// Reset 截断当前 schema 的全部业务表，保留 schema_migration。
func Reset(t *testing.T, db *repo.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := db.Pool.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename <> 'schema_migration'`)
	if err != nil {
		t.Fatalf("查询表清单失败: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("读取表名失败: %v", err)
		}
		names = append(names, pgx.Identifier{n}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表清单失败: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("schema 内没有表，迁移可能没跑起来")
	}
	if _, err := db.Pool.Exec(ctx, "TRUNCATE TABLE "+strings.Join(names, ", ")+" CASCADE"); err != nil {
		t.Fatalf("清空表失败: %v", err)
	}
}

// Schema 返回当前连接使用的 schema，便于自定义 SQL 断言。
func Schema(t *testing.T, db *repo.DB) string {
	t.Helper()
	var s string
	if err := db.Pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&s); err != nil {
		t.Fatalf("读取 current_schema 失败: %v", err)
	}
	return s
}

// Probe 建一次库并立刻清理，用于自检夹具本身是否可用（连库、迁移、清理）。
func Probe(ctx context.Context) error {
	db, cleanup, err := newSchemaDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	var n int
	return db.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migration`).Scan(&n)
}

func newSchemaDB(ctx context.Context) (*repo.DB, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, DSN())
	if err != nil {
		return nil, nil, err
	}
	schema := "t_" + randHex(6)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		return nil, nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(DSN())
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
		return nil, nil, err
	}
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 0
	// 所有连接统一走本包独占 schema，业务 SQL 里的非限定表名自然落到这里。
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
		return nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
		return nil, nil, err
	}

	db := &repo.DB{Pool: pool}
	if _, err := db.Migrate(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
		return nil, nil, err
	}

	cleanup := func() {
		pool.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = admin.Exec(cctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(cctx)
	}
	return db, cleanup, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
