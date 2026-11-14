package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/testsupport"
)

// Open 的成功分支此前只测过 DSN 非法一侧：连接池能建起来、能 Ping 通
// 这条主路径没有用例。DSN 里带的 pool_* 参数解析错了，线上就是启动即挂。
func TestOpenConnectsRealDatabase(t *testing.T) {
	cfg := cfgFromTestDSN(t)
	db, err := repo.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("连接真实元数据库应成功: %v", err)
	}
	defer db.Close()
	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Open 返回的连接池必须可用: %v", err)
	}
}

// 迁移是幂等的：schema_migration 里已有的版本必须被跳过。
// 这条「跳过」分支没被覆盖，意味着「重复执行迁移」的实线行为没被测过。
func TestMigrateSkipsAppliedVersions(t *testing.T) {
	db := testsupport.DB(t)

	applied, err := db.Migrate(context.Background())
	if err != nil {
		t.Fatalf("重复迁移不应报错: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("schema 已是最新，不应再应用任何迁移: %v", applied)
	}
	version, n, err := db.Version(context.Background())
	if err != nil {
		t.Fatalf("读取迁移版本失败: %v", err)
	}
	if version == "" || n == 0 {
		t.Fatalf("迁移记录应非空，实际 version=%q count=%d", version, n)
	}
}

// schema_migration 的 version 列缺失（结构漂移）时，Migrate 必须抛错而不是
// 当成「没有已应用版本」继续跑一遍迁移——那会把线上数据再改一次。
func TestMigrateFailsWhenVersionColumnMissing(t *testing.T) {
	db := testsupport.DB(t)
	renameColumn(t, db, "schema_migration", "version", "version_drift")

	_, err := db.Migrate(context.Background())
	wantErr(t, err, "version")
}

// 迁移脚本本身失败时要连带把 SQL 片段带出来，否则排障只能靠猜。
// 这里用「同名的 domain 挡住建表」制造一个真实的脚本错误。
func TestMigrateReportsBrokenScript(t *testing.T) {
	db := testsupport.DB(t)
	execSQL(t, db, `DELETE FROM schema_migration WHERE version='0001_init'`)
	// app_user 表名会占用同名复合类型，先删表再建同名 domain，
	// 迁移脚本里的 CREATE TABLE IF NOT EXISTS app_user 就会因为类型重名失败。
	dropTable(t, db, "app_user")
	execSQL(t, db, `CREATE DOMAIN app_user AS text`)

	applied, err := db.Migrate(context.Background())
	wantErr(t, err, "迁移 0001_init.sql 失败")
	if !strings.Contains(err.Error(), "--- SQL ---") {
		t.Fatalf("脚本错误应带上出错的 SQL 片段，实际: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("第一条迁移就失败，已应用列表应为空，实际: %v", applied)
	}
}

// 脚本执行成功但记账失败时，下次启动会重复执行同一条迁移。
// 这里让 INSERT schema_migration 抛错，钉住「不记账就不算应用」。
func TestMigrateFailsWhenVersionInsertRejected(t *testing.T) {
	db := testsupport.DB(t)
	execSQL(t, db, `DELETE FROM schema_migration WHERE version='0006_backfill'`)
	failOn(t, db, "schema_migration", "INSERT", "")

	applied, err := db.Migrate(context.Background())
	wantErr(t, err, "注入故障: schema_migration insert")
	if len(applied) != 0 {
		t.Fatalf("记账失败不应把版本算进已应用列表: %v", applied)
	}
}

// schema_migration 建不出来（同名 type 占位）时，Migrate 必须立刻失败，
// 而不是继续去查一张不存在的表。
func TestMigrateFailsWhenMigrationTableCannotBeCreated(t *testing.T) {
	db := testsupport.DB(t)
	dropTable(t, db, "schema_migration")
	execSQL(t, db, `CREATE DOMAIN schema_migration AS text`)

	_, err := db.Migrate(context.Background())
	wantErr(t, err, "schema_migration")
}

// PendingMigrations 是 CLI --check 的唯一事实来源：查询失败必须报错，
// 否则「没有待应用迁移」会把线上 schema 落后掩盖掉。
func TestPendingMigrationsFailsWhenVersionQueryBroken(t *testing.T) {
	db := testsupport.DB(t)
	renameColumn(t, db, "schema_migration", "version", "version_drift")

	_, err := db.PendingMigrations(context.Background())
	wantErr(t, err, "version")
}

// 同理，记账表建不出来时不能返回「空 pending」。
func TestPendingMigrationsFailsWhenTableCannotBeCreated(t *testing.T) {
	db := testsupport.DB(t)
	dropTable(t, db, "schema_migration")
	execSQL(t, db, `CREATE DOMAIN schema_migration AS text`)

	_, err := db.PendingMigrations(context.Background())
	wantErr(t, err, "schema_migration")
}

// 迁移脚本里的失败语句很长时，错误信息必须截断：
// 一条建表语句动辄上千字，全量打印会把日志系统刷爆。
func TestMigrateTruncatesLongFailingStatement(t *testing.T) {
	db := testsupport.DB(t)
	// 只重放 0005，并让其中的建表语句撞上同名 domain。
	// alert_event 的建表语句本身超过 800 字（含列注释），正好覆盖截断分支。
	execSQL(t, db, `DELETE FROM schema_migration WHERE version='0005_execution_semantic'`)
	dropTable(t, db, "alert_event")
	execSQL(t, db, `CREATE DOMAIN alert_event AS text`)

	_, err := db.Migrate(context.Background())
	wantErr(t, err, "迁移 0005_execution_semantic.sql 失败")
	if !strings.HasSuffix(err.Error(), "...") {
		t.Fatalf("超长 SQL 应被截断并以 ... 结尾，实际: %v", err)
	}
}
