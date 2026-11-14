package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/connector/postgres"
	"github.com/godw/godw/internal/pkg/errs"
)

// 本机有真实 PostgreSQL 时跑全链路；没有则跳过，绝不把「环境缺失」判成失败。
// 连接信息与开发环境一致：postgres://godw:godw@127.0.0.1:5432/godw。
func testSpec() connector.Spec {
	return connector.Spec{
		ID: "cov-pg", Name: "cov-pg", Type: "postgres",
		Config: map[string]any{
			"host": "127.0.0.1", "port": 5432, "user": "godw",
			"database": "godw", "sslmode": "disable", "max_conns": 4, "min_conns": 1,
		},
		Secret: map[string]string{"password": "godw"},
	}
}

func newTestConn(t *testing.T) connector.Connector {
	t.Helper()
	c, err := postgres.New(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Test(ctx); err != nil {
		_ = c.Close()
		t.Skipf("本机 PostgreSQL 不可用，跳过: %v", err)
	}
	return c
}

// asTransactor 取出可选的事务能力，Connector 主接口不包含 WithTx。
func asTransactor(t *testing.T, c connector.Connector) connector.Transactor {
	t.Helper()
	tx, ok := c.(connector.Transactor)
	if !ok {
		t.Fatal("PostgreSQL 连接器应实现 Transactor")
	}
	return tx
}

func TestConnBasics(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()

	if c.Type() != "postgres" {
		t.Fatalf("Type = %q", c.Type())
	}
	if c.Dialect().Name() != "postgres" {
		t.Fatalf("方言错误: %s", c.Dialect().Name())
	}
	caps := c.Capabilities()
	if !caps.MergeUpsert || !caps.TransactionalDDL || !caps.MaterializedView ||
		!caps.WindowFunction || !caps.ApproxCountDistinct || !caps.JSONType || !caps.BulkLoad {
		t.Fatalf("PostgreSQL 能力位声明不完整: %+v", caps)
	}
	if caps.InsertOverwrite || caps.ReplacePartition {
		t.Fatalf("PostgreSQL 不应声明覆盖写能力: %+v", caps)
	}
}

// 非法 sslmode 必须归为认证/参数类错误，让调用方知道要改配置而不是重试。
func TestNewInvalidDSN(t *testing.T) {
	spec := testSpec()
	spec.Config["sslmode"] = "definitely-not-a-mode"
	c, err := postgres.New(context.Background(), spec)
	if err == nil {
		_ = c.Close()
		t.Fatal("非法 sslmode 应构造失败")
	}
	if !errs.Is(err, errs.CodeDatasourceAuth) {
		t.Fatalf("DSN 解析失败应为 CodeDatasourceAuth: %v", err)
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Fatalf("错误信息应说明 DSN 解析失败: %v", err)
	}
}

// 密码错误必须在 Test 阶段暴露，而不是拖到第一次查询。
func TestAuthFailure(t *testing.T) {
	// 先确认实例可达，否则这个断言没有意义。
	ok := newTestConn(t)
	_ = ok.Close()

	spec := testSpec()
	spec.Secret["password"] = "definitely-wrong-password"
	c, err := postgres.New(context.Background(), spec)
	if err != nil {
		t.Fatalf("构造阶段不应失败: %v", err)
	}
	defer c.Close()
	if err := c.Test(context.Background()); err == nil {
		t.Fatal("错误密码必须让 Test 失败")
	}
}

// 元数据采集的列序、可空、默认值、注释、主键都要原样带出。
func TestMetadata(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()
	ctx := context.Background()

	suffix := fmt.Sprintf("cov_pg_%d", time.Now().UnixNano())
	table := "public." + suffix
	view := "public." + suffix + "_v"

	if _, err := c.Exec(ctx, `CREATE TABLE `+table+` (
		id bigserial PRIMARY KEY,
		name text NOT NULL DEFAULT 'x',
		note varchar(32)
	)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS "+table+" CASCADE") }()

	if _, err := c.Exec(ctx, `COMMENT ON COLUMN `+table+`.id IS '主键'`); err != nil {
		t.Fatalf("写列注释失败: %v", err)
	}
	if _, err := c.Exec(ctx, `CREATE VIEW `+view+` AS SELECT id, name FROM `+table); err != nil {
		t.Fatalf("建视图失败: %v", err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP VIEW IF EXISTS "+view) }()

	schemas, err := c.ListSchemas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range schemas {
		if s == "public" {
			found = true
		}
		if s == "pg_catalog" || s == "information_schema" {
			t.Fatalf("系统 schema 不应出现: %v", schemas)
		}
	}
	if !found {
		t.Fatalf("ListSchemas 缺少 public: %v", schemas)
	}

	tables, err := c.ListTables(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]connector.TableMeta{}
	for _, tm := range tables {
		byName[tm.Name] = tm
	}
	tm, ok := byName[suffix]
	if !ok {
		t.Fatalf("ListTables 未包含新建表 %s", suffix)
	}
	if tm.Schema != "public" || tm.Type != "table" {
		t.Fatalf("表元信息错误: %+v", tm)
	}
	if v, ok := byName[suffix+"_v"]; !ok || v.Type != "view" {
		t.Fatalf("视图类型识别错误: %+v", v)
	}

	if empty, err := c.ListTables(ctx, "no_such_schema_cov"); err != nil || len(empty) != 0 {
		t.Fatalf("不存在的 schema 应返回空列表: %v %v", empty, err)
	}

	meta, err := c.DescribeTable(ctx, "public", suffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Columns) != 3 {
		t.Fatalf("列数 = %d, want 3", len(meta.Columns))
	}
	id := meta.Columns[0]
	if id.Name != "id" || id.Type != "bigint" || id.Nullable || id.Ordinal != 1 {
		t.Fatalf("id 列元信息错误: %+v", id)
	}
	if id.Default == nil || !strings.Contains(*id.Default, "nextval") {
		t.Fatalf("id 默认值应保留序列表达式: %+v", id.Default)
	}
	if id.Comment != "主键" {
		t.Fatalf("列注释丢失: %q", id.Comment)
	}
	name := meta.Columns[1]
	if name.Nullable || name.Default == nil || !strings.Contains(*name.Default, "x") {
		t.Fatalf("name 列默认值/可空性错误: %+v", name)
	}
	note := meta.Columns[2]
	if !note.Nullable || note.Default != nil {
		t.Fatalf("note 列应可空且无默认值: %+v", note)
	}
	if len(meta.PrimaryKey) != 1 || meta.PrimaryKey[0] != "id" {
		t.Fatalf("主键解析错误: %v", meta.PrimaryKey)
	}

	// 视图没有主键，采集不能因此失败。
	vm, err := c.DescribeTable(ctx, "public", suffix+"_v")
	if err != nil {
		t.Fatalf("视图采集失败: %v", err)
	}
	if len(vm.Columns) != 2 || len(vm.PrimaryKey) != 0 {
		t.Fatalf("视图元信息错误: %+v", vm)
	}

	if _, err := c.DescribeTable(ctx, "public", "no_such_table_cov"); err == nil {
		t.Fatal("不存在的表应报错")
	} else if !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("表不存在应为 CodeNotFound: %v", err)
	}
}

// 复合主键的列序按索引定义保留，上层据此生成唯一键不能乱序。
func TestCompositePrimaryKey(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()
	ctx := context.Background()

	table := fmt.Sprintf("public.cov_pk_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, `CREATE TABLE `+table+` (a int, b int, c int, PRIMARY KEY (b, a))`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()

	meta, err := c.DescribeTable(ctx, "public", strings.TrimPrefix(table, "public."))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b", "a"}
	if len(meta.PrimaryKey) != 2 || meta.PrimaryKey[0] != want[0] || meta.PrimaryKey[1] != want[1] {
		t.Fatalf("复合主键顺序错误: %v, want %v", meta.PrimaryKey, want)
	}
}

// 参数必须真正绑定到服务端（$n），既防注入也避免类型推断错误。
func TestQueryExec(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()
	ctx := context.Background()

	table := fmt.Sprintf("public.cov_q_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, `CREATE TABLE `+table+` (id bigint, name text)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()

	res, err := c.Exec(ctx, `INSERT INTO `+table+` (id, name) VALUES ($1, $2)`, 1, "O'Reilly")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("RowsAffected = %d, want 1", res.RowsAffected)
	}
	if res.Duration < 0 {
		t.Fatalf("耗时不应为负: %v", res.Duration)
	}
	if _, err := c.Exec(ctx, `INSERT INTO `+table+` (id, name) VALUES ($1, $2)`, 2, "b"); err != nil {
		t.Fatal(err)
	}

	rows, err := c.Query(ctx, `SELECT id, name FROM `+table+` WHERE id >= $1 ORDER BY id`, 1)
	if err != nil {
		t.Fatal(err)
	}
	cols := rows.Columns()
	if len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "name" || cols[0].Ordinal != 1 {
		t.Fatalf("列元信息错误: %+v", cols)
	}
	if cols[0].Type == "" {
		t.Fatal("列类型 OID 不应为空")
	}
	var got [][2]any
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		got = append(got, [2]any{id, name})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("残留错误: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close 不应报错: %v", err)
	}
	if len(got) != 2 || got[0][1] != "O'Reilly" || got[1][0].(int64) != 2 {
		t.Fatalf("查询结果错误: %v", got)
	}

	if _, err := c.Query(ctx, `SELECT * FROM no_such_table_cov`); err == nil {
		t.Fatal("非法 SQL 应返回错误")
	}
	if _, err := c.Exec(ctx, `UPDATE no_such_table_cov SET a = 1`); err == nil {
		t.Fatal("非法 SQL 的 Exec 应返回错误")
	}

	// 上下文取消必须中断查询，不能静默返回空结果。
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Query(cctx, `SELECT 1`); err == nil {
		t.Fatal("已取消的查询应返回错误")
	}
}

// Stream 分批边界、空结果、回调错误都要按约定处理。
func TestStream(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()
	ctx := context.Background()

	table := fmt.Sprintf("public.cov_s_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, `CREATE TABLE `+table+` (id bigint)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()
	if _, err := c.Exec(ctx, `INSERT INTO `+table+` SELECT generate_series(1, 5)`); err != nil {
		t.Fatal(err)
	}

	var sizes []int
	var sum int64
	err := c.Stream(ctx, `SELECT id FROM `+table+` ORDER BY id`, 2, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			sum += id
			n++
		}
		sizes = append(sizes, n)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("分批错误: %v", sizes)
	}
	if sum != 15 {
		t.Fatalf("累计值 = %d, want 15", sum)
	}

	sizes = nil
	if err := c.Stream(ctx, `SELECT id FROM `+table, 0, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			n++
		}
		sizes = append(sizes, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[0] != 5 {
		t.Fatalf("不分批时应一次回调: %v", sizes)
	}

	// 空结果不触发回调和错误。
	called := false
	if err := c.Stream(ctx, `SELECT id FROM `+table+` WHERE false`, 10, func(connector.Rows) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("空结果不应触发回调")
	}

	sentinel := errors.New("consumer stopped")
	if err := c.Stream(ctx, `SELECT id FROM `+table, 2, func(connector.Rows) error {
		return sentinel
	}); err != sentinel {
		t.Fatalf("回调错误应原样返回，得到 %v", err)
	}

	if err := c.Stream(ctx, `SELECT * FROM no_such_table_cov`, 10, func(connector.Rows) error {
		return nil
	}); err == nil {
		t.Fatal("查询失败时 Stream 应返回错误")
	}
}

// 事务要么全部生效，要么一行都不能留下。
func TestWithTx(t *testing.T) {
	c := newTestConn(t)
	defer c.Close()
	ctx := context.Background()

	table := fmt.Sprintf("public.cov_tx_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, `CREATE TABLE `+table+` (id bigint)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()

	count := func() int64 {
		rows, err := c.Query(ctx, `SELECT count(*) FROM `+table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var n int64
		if rows.Next() {
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}

	err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		if _, err := exec(`INSERT INTO ` + table + ` VALUES (1)`); err != nil {
			return err
		}
		res, err := exec(`INSERT INTO ` + table + ` VALUES (2)`)
		if err != nil {
			return err
		}
		if res.RowsAffected != 1 {
			t.Fatalf("事务内 RowsAffected = %d", res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("提交路径失败: %v", err)
	}
	if n := count(); n != 2 {
		t.Fatalf("提交后行数 = %d, want 2", n)
	}

	sentinel := errors.New("业务校验失败")
	if err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		if _, err := exec(`INSERT INTO ` + table + ` VALUES (3)`); err != nil {
			return err
		}
		return sentinel
	}); err != sentinel {
		t.Fatalf("回调错误应原样返回: %v", err)
	}
	if n := count(); n != 2 {
		t.Fatalf("回滚后行数 = %d, want 2", n)
	}

	if err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		_, err := exec(`INSERT INTO no_such_table_cov VALUES (1)`)
		return err
	}); err == nil {
		t.Fatal("事务内语句失败应返回错误")
	}
	if n := count(); n != 2 {
		t.Fatalf("语句失败后行数 = %d, want 2", n)
	}
}

// 关闭后的连接必须明确失败；同时覆盖 WithTx 无法开启事务的分支。
func TestClosedConn(t *testing.T) {
	c := newTestConn(t)
	ctx := context.Background()
	if err := c.Close(); err != nil {
		t.Fatalf("Close 不应报错: %v", err)
	}

	if err := c.Test(ctx); err == nil {
		t.Fatal("关闭后 Test 应失败")
	}
	if _, err := c.Query(ctx, `SELECT 1`); err == nil {
		t.Fatal("关闭后 Query 应失败")
	}
	if _, err := c.Exec(ctx, `SELECT 1`); err == nil {
		t.Fatal("关闭后 Exec 应失败")
	}
	if _, err := c.ListSchemas(ctx); err == nil {
		t.Fatal("关闭后 ListSchemas 应失败")
	}
	if _, err := c.ListTables(ctx, "public"); err == nil {
		t.Fatal("关闭后 ListTables 应失败")
	}
	if _, err := c.DescribeTable(ctx, "public", "x"); err == nil {
		t.Fatal("关闭后 DescribeTable 应失败")
	}
	if err := asTransactor(t, c).WithTx(ctx, func(func(string, ...any) (connector.Result, error)) error {
		t.Fatal("事务无法开始，回调不应执行")
		return nil
	}); err == nil {
		t.Fatal("关闭后 WithTx 应失败")
	}
}

// 连接池参数非法时，构造阶段就要失败，而不是留下一个永远拿不到连接的池。
func TestNewInvalidPoolConfig(t *testing.T) {
	spec := testSpec()
	spec.Config["max_conns"] = 0
	c, err := postgres.New(context.Background(), spec)
	if err == nil {
		_ = c.Close()
		t.Skip("当前 pgxpool 版本允许 MaxConns=0，构造阶段无错误可断言")
	}
	if !strings.Contains(err.Error(), "MaxSize") && !strings.Contains(err.Error(), "pool") {
		t.Fatalf("错误信息应指向连接池配置: %v", err)
	}
}

// 假结果集专门覆盖 pgx 缓冲模式下无法复现的「扫描失败 / 遍历中断」分支。
// StreamRows 是 Stream 的公开复用点，导出目的即让这些语义可被独立验证。
type fakeStreamRows struct {
	cols    []connector.ColumnMeta
	data    [][]any
	pos     int
	scanErr error
	iterErr error
}

func (f *fakeStreamRows) Columns() []connector.ColumnMeta { return f.cols }
func (f *fakeStreamRows) Next() bool                      { f.pos++; return f.pos <= len(f.data) }
func (f *fakeStreamRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	row := f.data[f.pos-1]
	for i := range dest {
		*(dest[i].(*any)) = row[i]
	}
	return nil
}
func (f *fakeStreamRows) Err() error   { return f.iterErr }
func (f *fakeStreamRows) Close() error { return nil }

func TestStreamRows(t *testing.T) {
	cols := []connector.ColumnMeta{{Name: "id"}}

	// 正常分批。
	f := &fakeStreamRows{cols: cols, data: [][]any{{1}, {2}, {3}}}
	var sizes []int
	if err := postgres.StreamRows(f, 2, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			n++
		}
		sizes = append(sizes, n)
		return nil
	}); err != nil {
		t.Fatalf("正常分批失败: %v", err)
	}
	if len(sizes) != 2 || sizes[0] != 2 || sizes[1] != 1 {
		t.Fatalf("分批错误: %v", sizes)
	}

	// 空结果不回调。
	f = &fakeStreamRows{cols: cols}
	if err := postgres.StreamRows(f, 10, func(connector.Rows) error {
		t.Fatal("空结果不应回调")
		return nil
	}); err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}

	// 扫描失败必须中断并原样返回。
	scanErr := errors.New("scan failed")
	f = &fakeStreamRows{cols: cols, data: [][]any{{1}}, scanErr: scanErr}
	if err := postgres.StreamRows(f, 0, func(connector.Rows) error { return nil }); err != scanErr {
		t.Fatalf("扫描错误应原样返回，得到 %v", err)
	}

	// 遍历中途出错必须通过 Err 带出。
	iterErr := errors.New("iteration aborted")
	f = &fakeStreamRows{cols: cols, data: [][]any{{1}}, iterErr: iterErr}
	if err := postgres.StreamRows(f, 0, func(connector.Rows) error { return nil }); err != iterErr {
		t.Fatalf("遍历错误应原样返回，得到 %v", err)
	}

	// 回调错误必须中断。
	sentinel := errors.New("consumer stopped")
	f = &fakeStreamRows{cols: cols, data: [][]any{{1}, {2}}}
	if err := postgres.StreamRows(f, 1, func(connector.Rows) error { return sentinel }); err != sentinel {
		t.Fatalf("回调错误应原样返回，得到 %v", err)
	}
}
