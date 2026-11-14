package testsupport

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/repo"
)

// TestMain 让整个包跑在正式夹具流程上：建独占 schema、执行迁移、跑用例、清理。
// 这样 Main 本身与 Shared 的共享语义才真正被执行，而不是被跳过。
func TestMain(m *testing.M) {
	os.Exit(Main(m))
}

// Config 必须给出可直接启动服务的配置，且临时目录真实存在。
func TestConfig(t *testing.T) {
	cfg := Config(t)
	if cfg.Env != "test" || cfg.Auth.JWTSecret == "" || cfg.Auth.TokenTTLMin != 60 {
		t.Fatalf("Config 基础字段异常: %+v", cfg.Auth)
	}
	if cfg.Scheduler.Enabled {
		t.Fatal("测试环境不应启动调度循环")
	}
	if cfg.Log.Level != "error" || cfg.Log.Format != "text" {
		t.Fatalf("日志配置异常: %+v", cfg.Log)
	}
	for name, dir := range map[string]string{
		"executor.work_dir":   cfg.Executor.WorkDir,
		"semantic.export_dir": cfg.Semantic.ExportDir,
	} {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			t.Fatalf("%s 应是存在的目录: %q err=%v", name, dir, err)
		}
	}
	if cfg.Executor.WorkDir == cfg.Semantic.ExportDir {
		t.Fatal("工作目录与导出目录应各自独立")
	}
}

// Main 建好的共享库必须可用，且 Schema 与连接实际 search_path 一致。
func TestSharedSchema(t *testing.T) {
	db := Shared(t)
	schema := Schema(t, db)
	if !regexp.MustCompile(`^t_[0-9a-f]{12}$`).MatchString(schema) {
		t.Fatalf("schema 命名异常: %q", schema)
	}

	var current string
	if err := db.Pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != schema {
		t.Fatalf("Schema() = %q，实际连接在 %q", schema, current)
	}

	// 迁移必须已经在共享库里跑过。
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migration`).Scan(&n); err != nil {
		t.Fatalf("共享库未执行迁移: %v", err)
	}
}

// 每次 DB(t) 都要拿到独占 schema，用例之间互不影响。
func TestDBIsolation(t *testing.T) {
	a := DB(t)
	b := DB(t)
	if Schema(t, a) == Schema(t, b) {
		t.Fatal("DB(t) 应为每个用例创建独立 schema")
	}
	for _, db := range []*repo.DB{a, b} {
		if _, err := db.Pool.Exec(context.Background(), `CREATE TABLE solo (id int)`); err != nil {
			t.Fatalf("独占 schema 内建表失败: %v", err)
		}
	}
}

// Reset 截断业务表但保留迁移记录，否则下一个用例会认为库还没迁移过。
func TestReset(t *testing.T) {
	db := DB(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE reset_demo (id int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO reset_demo VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}

	Reset(t, db)

	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM reset_demo`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Reset 后仍有 %d 行", n)
	}
	// schema_migration 必须保留：它记录迁移版本，被截断会导致重复迁移。
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migration`).Scan(&n); err != nil {
		t.Fatalf("Reset 误删了迁移记录: %v", err)
	}
}

// DSN 优先环境变量，便于 CI 指向临时数据库实例。
func TestDSN(t *testing.T) {
	if got := DSN(); got == "" {
		t.Fatal("DSN 不能为空")
	}
	custom := "postgres://u:p@127.0.0.1:5999/db?sslmode=disable"
	t.Setenv("GODW_TEST_PG", custom)
	if got := DSN(); got != custom {
		t.Fatalf("环境变量未生效: %q", got)
	}
}

// 共享库不可用时 Shared 必须 Skip，而不是返回 nil 让用例 panic。
func TestSharedSkipsWithoutMain(t *testing.T) {
	old := sharedDB
	sharedDB = nil
	t.Cleanup(func() { sharedDB = old })

	t.Run("skip", func(t *testing.T) {
		Shared(t)
		t.Fatal("不应执行到这里")
	})
}

// 连不上库时 DB 必须 Skip 而不是失败；CI 无 PG 时整包保持绿色。
func TestDBSkipsWhenUnavailable(t *testing.T) {
	t.Setenv("GODW_TEST_PG", "postgres://bad:bad@127.0.0.1:1/nope?sslmode=disable")
	t.Run("skip", func(t *testing.T) {
		DB(t)
		t.Fatal("不应执行到这里")
	})
}

// Probe 失败路径：上下文已取消时应在连接阶段直接返回错误。
func TestProbeContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Probe(ctx); err == nil {
		t.Fatal("上下文取消应导致 Probe 失败")
	}
}

// 连接成功但无权建 schema 时，必须在建 schema 阶段失败并关闭管理连接。
func TestProbeCreateSchemaDenied(t *testing.T) {
	t.Setenv("GODW_TEST_PG", DSN()+"&options=-c%20default_transaction_read_only%3Don")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Probe(ctx); err == nil {
		t.Fatal("只读会话下建 schema 应失败")
	}
}

// Main 的失败分支：库不可用时返回 0（退出码成功），让「没装 PG」的机器不红。
func TestMainUnavailableReturnsZero(t *testing.T) {
	t.Setenv("GODW_TEST_PG", "postgres://bad:bad@127.0.0.1:1/nope?sslmode=disable")
	// 用一个不会真正跑用例的 M：这里只验证建库失败时提前返回 0。
	if code := Main(&testing.M{}); code != 0 {
		t.Fatalf("库不可用时 Main 应返回 0，实际 %d", code)
	}
}

// 防止测试包悄悄改变默认 DSN 常量（例如把密码写错导致全量跳过）。
func TestDefaultDSNShape(t *testing.T) {
	for _, part := range []string{"postgres://", "127.0.0.1:5432", "sslmode=disable"} {
		if !strings.Contains(DefaultDSN, part) {
			t.Fatalf("DefaultDSN 缺少 %q: %s", part, DefaultDSN)
		}
	}
}

// 建 schema 成功后迁移失败：把 statement_timeout 设到 5ms，迁移里的大表 DDL 必然超时。
// 这条用例覆盖「半成品库必须被清理、错误向上返回」的收尾分支。
func TestProbeMigrateFailure(t *testing.T) {
	t.Setenv("GODW_TEST_PG", DSN()+"&options=-c%20statement_timeout%3D5")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Probe(ctx); err == nil {
		t.Fatal("迁移阶段超时应返回错误")
	}
}
