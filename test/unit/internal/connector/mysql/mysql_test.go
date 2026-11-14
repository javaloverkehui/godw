package mysql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/connector/mysql"
	"github.com/godw/godw/internal/pkg/errs"
)

// 本机没有 MySQL 实例，因此这里只验证「不依赖服务端」的确定性行为：
// DSN 校验、能力声明、以及连接失败时的错误分类。真实读写路径由假驱动与集成测试覆盖。

func newConn(t *testing.T, spec connector.Spec) connector.Connector {
	t.Helper()
	c, err := mysql.New(context.Background(), spec)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	return c
}

func TestNewDefaults(t *testing.T) {
	c := newConn(t, connector.Spec{})
	defer c.Close()

	if c.Type() != "mysql" {
		t.Fatalf("Type() = %q", c.Type())
	}
	if c.Dialect().Name() != "mysql" {
		t.Fatalf("方言错误: %s", c.Dialect().Name())
	}
	caps := c.Capabilities()
	if !caps.MergeUpsert || !caps.WindowFunction || !caps.ApproxCountDistinct || !caps.JSONType {
		t.Fatalf("MySQL 能力位缺失: %+v", caps)
	}
	// MySQL 的 DDL 隐式提交，不能声明事务性 DDL；也没有分区覆写/物化视图。
	if caps.TransactionalDDL || caps.ReplacePartition || caps.InsertOverwrite || caps.MaterializedView {
		t.Fatalf("MySQL 不应声明这些能力: %+v", caps)
	}

	// 自定义连接池参数不能改变构造结果（服务端未启动也应成功）。
	c2 := newConn(t, connector.Spec{Config: map[string]any{
		"host": "127.0.0.1", "port": 3306, "user": "app",
		"database": "dw", "max_conns": 5, "min_conns": 1,
	}})
	defer c2.Close()
}

// 非法 DSN 必须在打开阶段就失败，否则错误会被拖到第一次查询才暴露。
func TestNewInvalidDSN(t *testing.T) {
	// 库名会经过 url.PathUnescape，非法百分号转义在 sql.Open 阶段即报错。
	// 注意地址里的右括号不会触发 DSN 校验（驱动按最后一个 ')' 取地址），
	// 那种情况会在拨号阶段以「连接失败」暴露，不属于本用例。
	c, err := mysql.New(context.Background(), connector.Spec{Config: map[string]any{
		"database": "bad%zz",
	}})
	if err == nil {
		_ = c.Close()
		t.Fatal("非法 DSN 应构造失败")
	}
	if !errs.Is(err, errs.CodeDatasourceAuth) {
		t.Fatalf("DSN 错误应归为 CodeDatasourceAuth: %v", err)
	}
}

// 连接失败的错误码决定前端提示（认证/网络）与是否重试。
func TestUnreachable(t *testing.T) {
	ctx := context.Background()
	c := newConn(t, connector.Spec{Config: map[string]any{
		"host": "127.0.0.1", "port": 1, "user": "root",
	}})
	defer c.Close()

	if err := c.Test(ctx); err == nil {
		t.Fatal("不可达端口必须让 Test 失败")
	}

	if _, err := c.ListSchemas(ctx); err == nil {
		t.Fatal("ListSchemas 应返回连接错误")
	}
	if _, err := c.ListTables(ctx, "dw"); err == nil {
		t.Fatal("ListTables 应返回连接错误")
	}
	if _, err := c.DescribeTable(ctx, "dw", "t"); err == nil {
		t.Fatal("DescribeTable 应返回连接错误")
	}
	if _, err := c.Query(ctx, "SELECT 1"); err == nil {
		t.Fatal("Query 应返回连接错误")
	}
	if _, err := c.Exec(ctx, "DO 1"); err == nil {
		t.Fatal("Exec 应返回连接错误")
	}
	if err := c.Stream(ctx, "SELECT 1", 10, func(connector.Rows) error { return nil }); err == nil {
		t.Fatal("Stream 应返回连接错误")
	}
	tx, ok := c.(connector.Transactor)
	if !ok {
		t.Fatal("MySQL 连接器应实现 Transactor")
	}
	if err := tx.WithTx(ctx, func(func(string, ...any) (connector.Result, error)) error {
		t.Fatal("事务无法开始，回调不应被调用")
		return nil
	}); err == nil {
		t.Fatal("WithTx 应返回连接错误")
	}
}

// 已取消的上下文必须立刻失败，不能等满 10 秒超时。
func TestTestCanceledContext(t *testing.T) {
	c := newConn(t, connector.Spec{Config: map[string]any{
		"host": "127.0.0.1", "port": 1,
	}})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := c.Test(ctx); err == nil {
		t.Fatal("已取消的上下文不应成功")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("取消后仍阻塞了 %v", elapsed)
	}
}

// 错误文案要能区分「网络不可达」与「认证失败」，否则运维只能猜。
func TestConnectionErrorMessage(t *testing.T) {
	c := newConn(t, connector.Spec{Config: map[string]any{
		"host": "127.0.0.1", "port": 1,
	}})
	defer c.Close()

	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("期望连接失败")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "connect") && !strings.Contains(msg, "refused") && !strings.Contains(msg, "dial") {
		t.Fatalf("错误信息缺少连接语义: %v", err)
	}
}
