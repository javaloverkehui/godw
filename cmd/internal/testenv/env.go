// Package testenv 为 cmd/ 下的命令测试提供「独占 schema + 临时配置」夹具。
//
// 命令层测试要跑真实 PostgreSQL：这些命令的价值就在把参数翻译成 SQL 与文件操作，
// 用 mock 只能验证「调用顺序」，验证不了「跑得通」。因此每个用例：
//
//  1. 建一个独占 schema，并让它成为连接的默认 search_path（通过 PGOPTIONS 注入，
//     config 层不感知，与生产行为一致）；
//  2. 写一份临时配置文件，命令只用 --config 指向它；
//  3. 用例结束删除 schema、清理临时目录。
//
// 连不上库时整包跳过（t.Skip），没有 PG 的机器不会红。
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/obs/log"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

// Env 是一个用例的隔离环境。
type Env struct {
	// Config 是写入配置文件的配置对象。
	Config config.Config
	// ConfigPath 是临时配置文件路径，命令用 --config 指向它。
	ConfigPath string
	// Schema 是本用例独占的元数据 schema。
	Schema string
	// Dir 是本用例的临时目录（工作目录、导出目录都在其中）。
	Dir string
}

// New 建 schema、写配置，并在用例结束时清理。
// migrate 为 true 时先把正式迁移跑进该 schema。
// mutate 可在写文件前改配置（例如换端口、改导出目录）。
func New(t *testing.T, migrate bool, mutate ...func(*config.Config)) *Env {
	t.Helper()

	cfg := config.Default()
	cfg.Env = "test"
	cfg.DB.Password = "godw"
	cfg.DB.MaxConns = 4
	cfg.DB.MinConns = 0
	cfg.Auth.JWTSecret = "test-secret-at-least-16-bytes"
	cfg.Auth.BootstrapAdmin = ""
	cfg.Auth.BootstrapPass = ""
	cfg.Scheduler.Enabled = false
	cfg.Log.Level = "error"
	cfg.Log.Format = "text"
	cfg.Server.ReadTimeoutSec = 5
	cfg.Server.WriteTimeoutSec = 5

	dir := t.TempDir()
	cfg.Executor.WorkDir = filepath.Join(dir, "work")
	cfg.Semantic.ExportDir = filepath.Join(dir, "exports")
	if err := os.MkdirAll(cfg.Executor.WorkDir, 0o755); err != nil {
		t.Fatalf("创建 work 目录失败: %v", err)
	}
	for _, fn := range mutate {
		fn(&cfg)
	}

	schema := "t_" + randHex(6)
	admin, err := pgx.Connect(context.Background(), testsupport.DSN())
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
	})
	if _, err := admin.Exec(context.Background(),
		`CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}

	// 本包所有连接（含 repo.Open 与数据源连接器）都通过 PGOPTIONS 落到独占 schema。
	// 不带 public：否则未建表的用例会「借到」public 里同名的表，断言就失去意义了。
	t.Setenv("PGOPTIONS", "-c search_path="+schema)

	path := filepath.Join(dir, "config.yaml")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化配置失败: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}

	e := &Env{Config: cfg, ConfigPath: path, Schema: schema, Dir: dir}
	if migrate {
		db := e.DB(t)
		if _, err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("执行迁移失败: %v", err)
		}
	}
	return e
}

// DB 打开指向独占 schema 的元数据库，用例结束自动关闭。
func (e *Env) DB(t *testing.T) *repo.DB {
	t.Helper()
	db, err := repo.Open(context.Background(), e.Config.DB)
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// Service 构造一份 service 用于准备夹具数据（如接入数据源、写入数据集）。
func (e *Env) Service(t *testing.T) *service.Service {
	t.Helper()
	db := e.DB(t)
	svc, err := service.New(db, e.Config, log.New(e.Config.Log))
	if err != nil {
		t.Fatalf("初始化 service 失败: %v", err)
	}
	return svc
}

// Exec 在本用例 schema 内执行 SQL（search_path 已指向该 schema）。
func (e *Env) Exec(t *testing.T, stmts ...string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testsupport.DSN())
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	defer conn.Close(context.Background())
	for _, s := range stmts {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("执行 SQL 失败: %v\n%s", err, s)
		}
	}
}

// Dataset 在元数据库里登记一张「已采集」的表，供 profile / init 等命令使用。
func (e *Env) Dataset(t *testing.T, dsName, schema, table string, cols []string) {
	t.Helper()
	svc := e.Service(t)
	ctx := context.Background()
	ws, err := svc.Repo.EnsureWorkspace(ctx, "default")
	if err != nil {
		t.Fatalf("建工作空间失败: %v", err)
	}
	ds, _, err := svc.Repo.DatasourceByName(ctx, ws.ID, dsName)
	if err != nil {
		t.Fatalf("查数据源失败: %v", err)
	}
	columns := make([]model.ColumnSpec, 0, len(cols))
	for _, c := range cols {
		columns = append(columns, model.ColumnSpec{Name: c, Type: "int4", Nullable: true})
	}
	if err := svc.Repo.UpsertDataset(ctx, model.Dataset{
		DatasourceID: ds.ID, Schema: schema, Name: table, Type: "table", Columns: columns,
	}, ws.ID); err != nil {
		t.Fatalf("登记数据集失败: %v", err)
	}
}

// Datasource 接入一个指向测试库的数据源（命令里的 pg_dw 默认名）。
func (e *Env) Datasource(t *testing.T, name string) {
	t.Helper()
	svc := e.Service(t)
	ctx := context.Background()
	ws, err := svc.Repo.EnsureWorkspace(ctx, "default")
	if err != nil {
		t.Fatalf("建工作空间失败: %v", err)
	}
	if _, _, err := svc.Repo.DatasourceByName(ctx, ws.ID, name); err == nil {
		return
	}
	if _, err := svc.CreateDatasource(ctx, ws.ID, name, "postgres", map[string]any{
		"host": e.Config.DB.Host, "port": e.Config.DB.Port,
		"database": e.Config.DB.Database, "user": e.Config.DB.User,
	}, map[string]string{"password": e.Config.DB.Password}); err != nil {
		t.Fatalf("接入数据源失败: %v", err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
