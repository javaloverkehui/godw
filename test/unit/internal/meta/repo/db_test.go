package repo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

// Open 的错误分支必须区分「DSN 写错」和「库连不上」：前者是配置问题，
// 后者要提示运维看服务，错误码不能混。
func TestOpenErrors(t *testing.T) {
	ctx := context.Background()

	badDSN := config.DBConfig{Host: "127.0.0.1", Port: 5432, User: "godw", Database: "godw", SSLMode: "not-a-mode"}
	if _, err := repo.Open(ctx, badDSN); err == nil {
		t.Fatal("非法 sslmode 应报错")
	}

	// 端口 1 上不会有 PG，应返回「不可达」而不是超时挂死。
	unreachable := config.DBConfig{Host: "127.0.0.1", Port: 1, User: "godw", Database: "godw", SSLMode: "disable", MaxConns: 1}
	_, err := repo.Open(ctx, unreachable)
	if err == nil {
		t.Fatal("不可达的库应报错")
	}
	if ae := errs.From(err); ae.Code != errs.CodeUnavailable {
		t.Fatalf("应归类为不可用，得到 %v", ae.Code)
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("错误信息不应泄露密码: %v", err)
	}
}

func TestOpenPingVersion(t *testing.T) {
	db := testsupport.Shared(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
	version, n, err := db.Version(ctx)
	if err != nil {
		t.Fatalf("Version 失败: %v", err)
	}
	if version == "" || n == 0 {
		t.Fatalf("迁移后应有版本: %q %d", version, n)
	}
	pending, err := db.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations 失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("新库不应有未应用迁移: %v", pending)
	}
}

// 迁移清空后 PendingMigrations 要能列出全部版本，CLI 的 --check 依赖它。
func TestPendingMigrationsAfterReset(t *testing.T) {
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `DELETE FROM schema_migration`); err != nil {
		t.Fatalf("清空迁移记录失败: %v", err)
	}
	pending, err := db.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations 失败: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("清空后应列出待应用迁移")
	}
	if _, _, err := db.Version(ctx); err != nil {
		t.Fatalf("空迁移表时 Version 也应可用: %v", err)
	}
	// 重新迁移一次，恢复共享夹具，避免影响同包其他用例。
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("重新迁移失败: %v", err)
	}
}

// WithTx 的回滚必须真的撤销写入，否则「先扣配额再建实例」这类流程会出现脏数据。
func TestWithTxCommitAndRollback(t *testing.T) {
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	ctx := context.Background()

	if err := db.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace(id, name) VALUES('ws_tx','tx')`)
		return err
	}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace WHERE id='ws_tx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("提交后应有记录: n=%d err=%v", n, err)
	}

	sentinel := errors.New("boom")
	err := db.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace(id, name) VALUES('ws_tx2','tx2')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应原样返回业务错误，得到 %v", err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace WHERE id='ws_tx2'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("回滚后不应有记录: n=%d err=%v", n, err)
	}
}
