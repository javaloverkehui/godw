package mysql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/connector/mysql"
	"github.com/godw/godw/internal/pkg/errs"
)

// 本机没有 MySQL 实例，但连接器里真正容易写错的是「结果集列类型映射、
// information_schema 查询的扫描顺序、事务提交/回滚」——这些都能在
// database/sql 的驱动边界上用假驱动验证，不必连真实库。
// 这里只用标准库，不引入 sqlmock 之类的新依赖。

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

// fakeConnector 的 mode 用于精确制造「扫描失败 / 迭代中途出错」等异常路径。
type fakeConnector struct{ mode string }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{mode: c.mode}, nil
}
func (fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeConn struct{ mode string }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: Prepare 不应被调用")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &fakeTx{}, nil
}
func (c *fakeConn) Ping(context.Context) error { return nil }

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// 表名是绑定参数，不参与 query 文本匹配。
	table := ""
	if len(args) >= 2 {
		table = fmt.Sprint(args[1].Value)
	}
	switch {
	case strings.Contains(query, "information_schema.schemata"):
		if c.mode == "bad-schema" {
			return &fakeRows{cols: []string{"schema_name"}, data: [][]driver.Value{{nil}}}, nil
		}
		return &fakeRows{
			cols: []string{"schema_name"},
			data: [][]driver.Value{{"dw"}, {"analytics"}},
		}, nil
	case strings.Contains(query, "information_schema.tables"):
		if c.mode == "bad-tables" {
			return &fakeRows{
				cols: []string{"table_name", "table_type", "table_comment", "table_rows", "size"},
				data: [][]driver.Value{{nil, nil, nil, nil, nil}},
			}, nil
		}
		return &fakeRows{
			cols: []string{"table_name", "table_type", "table_comment", "table_rows", "size"},
			data: [][]driver.Value{
				{"t1", "BASE TABLE", "主表", int64(10), int64(1024)},
				{"v1", "VIEW", "视图", int64(0), int64(0)},
			},
		}, nil
	case strings.Contains(query, "information_schema.columns"):
		if table == "missing" {
			return &fakeRows{cols: []string{"column_name"}}, nil
		}
		if c.mode == "bad-cols" {
			return &fakeRows{
				cols: []string{"column_name", "column_type", "is_nullable", "column_default", "column_comment", "ordinal_position", "column_key"},
				data: [][]driver.Value{{nil, nil, nil, nil, nil, nil, nil}},
			}, nil
		}
		if c.mode == "cols-err" {
			return &fakeRows{
				cols: []string{"column_name", "column_type", "is_nullable", "column_default", "column_comment", "ordinal_position", "column_key"},
				data: [][]driver.Value{{"id", "bigint", int64(0), nil, "", int64(1), "PRI"}},
				err:  errors.New("fake: 遍历列时连接中断"),
			}, nil
		}
		return &fakeRows{
			cols: []string{"column_name", "column_type", "is_nullable", "column_default", "column_comment", "ordinal_position", "column_key"},
			data: [][]driver.Value{
				{"id", "bigint", int64(0), nil, "", int64(1), "PRI"},
				{"name", "varchar(255)", int64(1), "无名", "名称", int64(2), ""},
			},
		}, nil
	case strings.Contains(query, "FAIL"):
		return nil, errors.New("fake: 查询失败")
	default:
		r := &fakeRows{
			cols: []string{"id", "name"},
			data: [][]driver.Value{
				{int64(1), "n1"},
				{int64(2), "n2"},
				{int64(3), "n3"},
				{int64(4), "n4"},
				{int64(5), "n5"},
			},
		}
		if c.mode == "rows-err" {
			r.data = r.data[:2]
			r.err = errors.New("fake: 遍历结果集时连接中断")
		}
		return r, nil
	}
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "FAIL") {
		return nil, errors.New("fake: 执行失败")
	}
	return driver.RowsAffected(3), nil
}

type fakeTx struct{}

func (t *fakeTx) Commit() error   { return nil }
func (t *fakeTx) Rollback() error { return nil }

type fakeRows struct {
	cols []string
	data [][]driver.Value
	pos  int
	err  error
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		if r.err != nil {
			return r.err
		}
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// 可选接口：让 columnsOf 能拿到类型名与可空性。
func (r *fakeRows) ColumnTypeDatabaseTypeName(i int) string { return "VARCHAR" }
func (r *fakeRows) ColumnTypeNullable(int) (bool, bool)     { return true, true }

func fakeConnDB(t *testing.T) connector.Connector {
	t.Helper()
	return fakeConnDBMode(t, "")
}

func fakeConnDBMode(t *testing.T, mode string) connector.Connector {
	t.Helper()
	c := mysql.NewWithDB(sql.OpenDB(fakeConnector{mode: mode}))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// asTransactor 取出事务能力；WithTx 是可选接口，不在 Connector 主接口里。
func asTransactor(t *testing.T, c connector.Connector) connector.Transactor {
	t.Helper()
	tx, ok := c.(connector.Transactor)
	if !ok {
		t.Fatal("MySQL 连接器应实现 Transactor")
	}
	return tx
}

func TestFakeListSchemas(t *testing.T) {
	c := fakeConnDB(t)
	got, err := c.ListSchemas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "dw" || got[1] != "analytics" {
		t.Fatalf("库列表错误: %v", got)
	}
}

// 视图/表必须按 table_type 分类：把视图当表会在写入策略里选错路径。
func TestFakeListTables(t *testing.T) {
	c := fakeConnDB(t)
	got, err := c.ListTables(context.Background(), "dw")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("表数量 = %d, want 2", len(got))
	}
	if got[0].Name != "t1" || got[0].Type != "table" || got[0].Schema != "dw" {
		t.Fatalf("普通表元信息错误: %+v", got[0])
	}
	if got[0].Comment != "主表" || got[0].RowCount != 10 || got[0].SizeBytes != 1024 {
		t.Fatalf("普通表字段映射错误: %+v", got[0])
	}
	if got[1].Name != "v1" || got[1].Type != "view" {
		t.Fatalf("视图分类错误: %+v", got[1])
	}
}

// column_default 为 NULL 是常态，扫进 string 会整表采集失败，必须用 NullString。
func TestFakeDescribeTable(t *testing.T) {
	c := fakeConnDB(t)
	meta, err := c.DescribeTable(context.Background(), "dw", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Columns) != 2 {
		t.Fatalf("列数 = %d, want 2", len(meta.Columns))
	}
	first := meta.Columns[0]
	if first.Name != "id" || first.Type != "bigint" || first.Nullable || first.Default != nil {
		t.Fatalf("无默认值列解析错误: %+v", first)
	}
	if first.Ordinal != 1 {
		t.Fatalf("序号错误: %d", first.Ordinal)
	}
	second := meta.Columns[1]
	if !second.Nullable || second.Default == nil || *second.Default != "无名" || second.Comment != "名称" {
		t.Fatalf("带默认值列解析错误: %+v", second)
	}
	// column_key=PRI 必须被识别为主键，且按列序保留（复合主键顺序有意义）。
	if len(meta.PrimaryKey) != 1 || meta.PrimaryKey[0] != "id" {
		t.Fatalf("主键解析错误: %v", meta.PrimaryKey)
	}

	if _, err := c.DescribeTable(context.Background(), "dw", "missing"); err == nil {
		t.Fatal("不存在的表必须报错")
	} else if !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("表不存在应为 CodeNotFound: %v", err)
	}
}

func TestFakeQuery(t *testing.T) {
	c := fakeConnDB(t)
	rows, err := c.Query(context.Background(), "SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := rows.Columns()
	if len(cols) != 2 || cols[0].Name != "id" || cols[0].Type != "VARCHAR" || !cols[0].Nullable {
		t.Fatalf("列元信息错误: %+v", cols)
	}
	if cols[1].Ordinal != 2 {
		t.Fatalf("列序号错误: %+v", cols[1])
	}

	n := 0
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		n++
		if id != int64(n) || name != fmt.Sprintf("n%d", n) {
			t.Fatalf("第 %d 行数据错误: %d %q", n, id, name)
		}
	}
	if n != 5 {
		t.Fatalf("行数 = %d, want 5", n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("残留错误: %v", err)
	}

	if _, err := c.Query(context.Background(), "SELECT FAIL"); err == nil {
		t.Fatal("查询失败必须返回错误")
	}
}

func TestFakeExec(t *testing.T) {
	c := fakeConnDB(t)
	res, err := c.Exec(context.Background(), "UPDATE t SET name = ?", "x")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected != 3 {
		t.Fatalf("RowsAffected = %d, want 3", res.RowsAffected)
	}
	if res.Duration < 0 {
		t.Fatalf("耗时不应为负: %v", res.Duration)
	}
	if _, err := c.Exec(context.Background(), "UPDATE FAIL"); err == nil {
		t.Fatal("执行失败必须返回错误")
	}
}

// 分批流式读取的边界错位会丢行或重复回调。
func TestFakeStream(t *testing.T) {
	c := fakeConnDB(t)
	var sizes []int
	var total int64
	err := c.Stream(context.Background(), "SELECT id, name FROM t", 2, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			total += id
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
	if total != 15 {
		t.Fatalf("累计值 = %d, want 15", total)
	}

	sentinel := errors.New("consumer stopped")
	if err := c.Stream(context.Background(), "SELECT id, name FROM t", 2, func(connector.Rows) error {
		return sentinel
	}); err != sentinel {
		t.Fatalf("回调错误应原样返回，得到 %v", err)
	}

	if err := c.Stream(context.Background(), "SELECT FAIL", 2, func(connector.Rows) error {
		return nil
	}); err == nil {
		t.Fatal("查询失败时 Stream 必须报错")
	}
}

// 事务必须做到：成功提交、回调失败回滚、语句失败原样上报。
func TestFakeWithTx(t *testing.T) {
	c := fakeConnDB(t)
	ctx := context.Background()

	var got int64
	err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		res, err := exec("INSERT INTO t VALUES (1)")
		if err != nil {
			return err
		}
		got = res.RowsAffected
		return nil
	})
	if err != nil {
		t.Fatalf("提交路径失败: %v", err)
	}
	if got != 3 {
		t.Fatalf("事务内 RowsAffected = %d, want 3", got)
	}

	sentinel := errors.New("业务校验失败")
	if err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		_, _ = exec("INSERT INTO t VALUES (2)")
		return sentinel
	}); err != sentinel {
		t.Fatalf("回调错误应原样返回并回滚，得到 %v", err)
	}

	if err := asTransactor(t, c).WithTx(ctx, func(exec func(string, ...any) (connector.Result, error)) error {
		_, err := exec("UPDATE FAIL")
		return err
	}); err == nil {
		t.Fatal("事务内语句失败必须返回错误")
	}
}

// 扫描失败必须显式返回：把 NULL 当空串吞掉会让元数据看起来「有列但没名字」。
func TestFakeScanErrors(t *testing.T) {
	ctx := context.Background()

	if _, err := fakeConnDBMode(t, "bad-schema").ListSchemas(ctx); err == nil {
		t.Fatal("schema 名扫描失败应返回错误")
	}
	if _, err := fakeConnDBMode(t, "bad-tables").ListTables(ctx, "dw"); err == nil {
		t.Fatal("表元信息扫描失败应返回错误")
	}
	if _, err := fakeConnDBMode(t, "bad-cols").DescribeTable(ctx, "dw", "t1"); err == nil {
		t.Fatal("列元信息扫描失败应返回错误")
	}
}

// 结果集遍历到一半断线时，Err() 必须把错误带出来，而不是当成正常结束。
func TestFakeRowsErr(t *testing.T) {
	c := fakeConnDBMode(t, "cols-err")
	if _, err := c.DescribeTable(context.Background(), "dw", "t1"); err == nil {
		t.Fatal("列遍历中断应返回错误")
	}

	c2 := fakeConnDBMode(t, "rows-err")
	if err := c2.Stream(context.Background(), "SELECT id, name FROM t", 10, func(rows connector.Rows) error {
		for rows.Next() {
		}
		return rows.Err()
	}); err == nil {
		t.Fatal("流式读取中断应返回错误")
	}
}
