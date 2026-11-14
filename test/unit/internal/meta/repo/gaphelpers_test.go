package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5/pgconn"
)

// 这一组用例专门覆盖「真实库上才会走到的错误分支」：唯一键冲突、外键缺失、
// 坏 JSONB、第二条语句失败、扫描类型不匹配等。
//
// 这些分支在仓储层表现为 `return err`，用假实现只能证明「写了 return」，
// 用真库则必须先把库弄坏。下面几个 helper 就是「弄坏」的手段，
// 每个都在独占 schema 里执行（testsupport.DB），不会污染其他用例。
//
// 每个 helper 都对应一种真实故障：
//   - 删表/改列名 —— 迁移没跑、表被误删、版本不匹配；
//   - 改列类型   —— 结构漂移，Go 侧 Scan 目标与库中类型对不上；
//   - 触发器报错 —— 写入侧约束（唯一键、外键、检查约束）拒绝；
//   - 触发器返回 NULL —— 写入被静默丢弃（如 partial index / 规则改写）。

// isolatedRepo 建一个独占 schema 的仓储。所有破坏性构造都必须在独占 schema 里做，
// 否则后续用例会因为列被改名而连锁失败。
func isolatedRepo(t *testing.T) (*repo.Repo, *repo.DB) {
	t.Helper()
	db := testsupport.DB(t)
	return repo.New(db), db
}

// dropTable 模拟「表被误删 / 迁移漏跑」。CASCADE 是为了连带删掉外键依赖。
func dropTable(t *testing.T, db *repo.DB, table string) {
	t.Helper()
	execSQL(t, db, `DROP TABLE IF EXISTS `+table+` CASCADE`)
}

// renameColumn 让后续引用该列的语句报 42703（未定义列），
// 用来覆盖「前一条语句成功、后一条失败」的分支——关连接池做不到这一点，
// 因为那样第一条语句就先失败了。
func renameColumn(t *testing.T, db *repo.DB, table, from, to string) {
	t.Helper()
	execSQL(t, db, `ALTER TABLE `+table+` RENAME COLUMN `+from+` TO `+to)
}

// dropChecks 摘掉表上的 CHECK 约束。改列类型前必须摘，
// 否则 `status IN (...)` 这类约束会因为整数与字符串无法比较而挡住 ALTER。
func dropChecks(t *testing.T, db *repo.DB, table string) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Pool.Query(ctx,
		`SELECT conname FROM pg_constraint WHERE conrelid=$1::regclass AND contype='c'`, table)
	if err != nil {
		t.Fatalf("查询 %s 的 CHECK 约束失败: %v", table, err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("读取约束名失败: %v", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历约束失败: %v", err)
	}
	for _, n := range names {
		execSQL(t, db, `ALTER TABLE `+table+` DROP CONSTRAINT "`+n+`"`)
	}
}

// alterColumn 改列类型（先摘 CHECK、去默认值，避免约束与默认表达式的类型校验挡住）。
// 改完类型后 Go 侧 Scan 目标就对不上了，这是最接近「结构漂移」的故障形态。
func alterColumn(t *testing.T, db *repo.DB, table, column, newType, using string) {
	t.Helper()
	dropChecks(t, db, table)
	execSQL(t, db, `ALTER TABLE `+table+` ALTER COLUMN `+column+` DROP DEFAULT`)
	execSQL(t, db, `ALTER TABLE `+table+` ALTER COLUMN `+column+` TYPE `+newType+` USING `+using)
}

// textColumn 把非文本列改成文本。仓储把它扫进 int64/bool/[]string/time.Time 时
// 会报类型不匹配——这是最接近「结构漂移」的故障形态。
//
// 反向（文本列改成整数）没有用：pgx 会先把整数格式化成文本再交给 *string，
// 扫描不会失败，故障就被「友好地」吞掉了。
func textColumn(t *testing.T, db *repo.DB, table, column string) {
	t.Helper()
	alterColumn(t, db, table, column, "text", column+"::text")
}

// nullify 把列改成可空并清空数据，模拟上游写了 NULL，
// 用来覆盖「扫进 string / time.Time 时遇到 NULL」的分支。
// 只适用于非主键列（主键列不允许 NULL）。
func nullify(t *testing.T, db *repo.DB, table, column string) {
	t.Helper()
	execSQL(t, db, `ALTER TABLE `+table+` ALTER COLUMN `+column+` DROP NOT NULL`)
	execSQL(t, db, `UPDATE `+table+` SET `+column+`=NULL`)
}

// nullifyKeyColumn 先摘掉主键约束再写 NULL。
// 只用于「整张表的可扫列全在主键里」的情况：线上出现这种脏数据，
// 前提本来就是约束被摘掉或历史数据迁移漏了。
func nullifyKeyColumn(t *testing.T, db *repo.DB, table, column string) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Pool.Query(ctx,
		`SELECT conname FROM pg_constraint WHERE conrelid=$1::regclass AND contype='p'`, table)
	if err != nil {
		t.Fatalf("查询 %s 的主键失败: %v", table, err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("读取主键名失败: %v", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历主键失败: %v", err)
	}
	for _, n := range names {
		execSQL(t, db, `ALTER TABLE `+table+` DROP CONSTRAINT "`+n+`"`)
	}
	nullify(t, db, table, column)
}

// failOn 在指定表的指定写操作上挂一个必定抛错的触发器，模拟约束拒绝。
// when 非空时只对满足条件的行生效（用于「同一事务里前一次写成功、后一次失败」）。
func failOn(t *testing.T, db *repo.DB, table, op, when string) {
	t.Helper()
	name := "inject_" + table + "_" + strings.ToLower(op)
	execSQL(t, db, `CREATE OR REPLACE FUNCTION `+name+`_fn() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION '注入故障: `+table+` `+strings.ToLower(op)+`';
END; $$ LANGUAGE plpgsql`)
	suffix := ""
	if when != "" {
		suffix = " WHEN (" + when + ")"
	}
	execSQL(t, db, `CREATE TRIGGER `+name+` BEFORE `+op+` ON `+table+`
FOR EACH ROW`+suffix+` EXECUTE FUNCTION `+name+`_fn()`)
}

// swallowInsert 让 INSERT 被静默丢弃（触发器返回 NULL），
// INSERT ... RETURNING 因此没有行——对应「写入被规则吞掉」的故障。
func swallowInsert(t *testing.T, db *repo.DB, table string) {
	t.Helper()
	name := "swallow_" + table
	execSQL(t, db, `CREATE OR REPLACE FUNCTION `+name+`_fn() RETURNS trigger AS $$
BEGIN
  RETURN NULL;
END; $$ LANGUAGE plpgsql`)
	execSQL(t, db, `CREATE TRIGGER `+name+` BEFORE INSERT ON `+table+`
FOR EACH ROW EXECUTE FUNCTION `+name+`_fn()`)
}

// renameAfterInsert 让 INSERT 成功、但随后的回查失败：
// 触发器在插入成功后把列改名，回查语句就会报「列不存在」。
// 用于覆盖「写成功但立刻读失败」这种半失败状态。
func renameAfterInsert(t *testing.T, db *repo.DB, table, from, to string) {
	t.Helper()
	name := "rename_" + table
	execSQL(t, db, `CREATE OR REPLACE FUNCTION `+name+`_fn() RETURNS trigger AS $$
BEGIN
  ALTER TABLE `+table+` RENAME COLUMN `+from+` TO `+to+`;
  RETURN NEW;
END; $$ LANGUAGE plpgsql`)
	execSQL(t, db, `CREATE TRIGGER `+name+` AFTER INSERT ON `+table+`
FOR EACH ROW EXECUTE FUNCTION `+name+`_fn()`)
}

// instanceSample 返回一条最小可用的实例定义。
// trigger_type 必须显式给值：列上的 DEFAULT 不生效，因为 CreateInstance 会把空串写进去，
// 而 instance 表对 trigger_type 有 CHECK 约束。
func instanceSample(wsID, pipelineID string) model.Instance {
	return model.Instance{
		WorkspaceID: wsID, PipelineID: pipelineID,
		BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k1",
	}
}

// wantErr 断言调用确实返回错误，并可选断言错误里带的关键字。
func wantErr(t *testing.T, err error, keyword string) {
	t.Helper()
	if err == nil {
		t.Fatalf("预期报错，实际返回 nil（关键字 %q）", keyword)
	}
	if keyword != "" && !strings.Contains(err.Error(), keyword) {
		t.Fatalf("错误里应包含 %q，实际: %v", keyword, err)
	}
}

// cfgFromTestDSN 把测试库 DSN 拆成 config.DBConfig。
// 直接用 config.Default() 里的账号密码会连不上：测试库凭证来自 GODW_TEST_PG，
// 与产品默认值不同。
func cfgFromTestDSN(t *testing.T) config.DBConfig {
	t.Helper()
	parsed, err := pgconn.ParseConfig(testsupport.DSN())
	if err != nil {
		t.Fatalf("解析测试库 DSN 失败: %v", err)
	}
	cfg := config.Default().DB
	cfg.Host = parsed.Host
	cfg.Port = int(parsed.Port)
	cfg.User = parsed.User
	cfg.Password = parsed.Password
	cfg.Database = parsed.Database
	cfg.SSLMode = "disable"
	cfg.MaxConns = 4
	cfg.MinConns = 0
	return cfg
}
