//go:build integration

// 真实引擎集成测试：验证「连接器 + 方言 DDL + 基本 DML + 元数据」这条最小闭环。
//
// 与单测的分工：单测只验 SQL 字符串生成，这里真的执行。历史上有多个缺陷
// （ClickHouse 建库语句、能力位声明、DuckDB 扫描）只在这一层才会暴露。
//
// 运行方式见 scripts/integration.sh 或 `make test-integration`。
// 环境变量缺失时用例自动跳过，不会把没起库的机器判成失败。
package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"

	_ "github.com/godw/godw/internal/connector/clickhouse"
	_ "github.com/godw/godw/internal/connector/mysql"
	_ "github.com/godw/godw/internal/connector/postgres"
)

type engineCase struct {
	typ     string
	env     string
	schema  string
	dialect dialect.Dialect
}

func cases() []engineCase {
	return []engineCase{
		{"postgres", "GODW_TEST_PG", "public", dialect.Postgres{}},
		{"mysql", "GODW_TEST_MYSQL", "dw", dialect.MySQL{}},
		{"clickhouse", "GODW_TEST_CH", "dw", dialect.ClickHouse{}},
	}
}

func TestEngineRoundTrip(t *testing.T) {
	for _, c := range cases() {
		c := c
		t.Run(c.typ, func(t *testing.T) {
			t.Parallel()
			dsn := os.Getenv(c.env)
			if dsn == "" {
				t.Skipf("%s 未设置，跳过", c.env)
			}
			spec, err := specFromDSN(c.typ, dsn)
			if err != nil {
				t.Fatalf("解析 DSN 失败: %v", err)
			}
			runRoundTrip(t, spec, c.schema, c.dialect)
		})
	}
}

func runRoundTrip(t *testing.T, spec connector.Spec, schema string, d dialect.Dialect) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := connector.Open(ctx, spec)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()

	table := fmt.Sprintf("it_%d", time.Now().UnixNano())
	modelSpec := model.ModelSpec{
		Name:       table,
		Layer:      "ods",
		Datasource: spec.Name,
		Schema:     schema,
		Table:      table,
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "string", Nullable: false},
			{Name: "amt", Type: "decimal(18,2)", Nullable: false},
			{Name: "dt", Type: "date", Nullable: false},
		},
		PrimaryKey:  []string{"id"},
		PartitionBy: []string{"dt"},
	}
	ddl, err := modelc.Compile(modelSpec, d)
	if err != nil {
		t.Fatalf("编译 DDL 失败: %v", err)
	}
	qualified := d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+qualified)
	}()

	for _, stmt := range splitStatements(ddl) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("执行 DDL 失败: %v\n%s", err, stmt)
		}
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s (%s, %s, %s) VALUES ('a', 12.34, '2026-11-09'), ('b', 7.66, '2026-11-09')",
		qualified, d.QuoteIdent("id"), d.QuoteIdent("amt"), d.QuoteIdent("dt"))); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT COUNT(*), SUM(%s) FROM %s", d.QuoteIdent("amt"), qualified))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("查询没有返回行")
	}
	var cnt int64
	var sum float64
	err = rows.Scan(&cnt, &sum)
	// 必须在这里关掉：DuckDB 是单连接（MaxOpenConns=1），
	// 结果集没关时后续查询会一直等连接，表现为 context deadline exceeded。
	_ = rows.Close()
	if err != nil {
		t.Fatalf("扫描失败（DECIMAL/NULL 处理有问题？）: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("行数 = %d, want 2", cnt)
	}
	if sum < 19.99 || sum > 20.01 {
		t.Fatalf("金额合计 = %v, want 20.00", sum)
	}

	meta, err := conn.DescribeTable(ctx, schema, table)
	if err != nil {
		t.Fatalf("DescribeTable 失败: %v", err)
	}
	if len(meta.Columns) != 3 {
		t.Fatalf("列数 = %d, want 3", len(meta.Columns))
	}
}

// specFromDSN 把 postgres://user:pass@host:port/db 这样的地址转成连接器配置。
func specFromDSN(typ, raw string) (connector.Spec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return connector.Spec{}, err
	}
	port := 0
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return connector.Spec{}, fmt.Errorf("端口 %q 不合法", p)
		}
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return connector.Spec{
		Type: typ,
		Name: "it-" + typ,
		Config: map[string]any{
			"host": u.Hostname(), "port": port,
			"database": strings.TrimPrefix(u.Path, "/"), "user": user,
		},
		Secret: map[string]string{"password": pass},
	}, nil
}

// splitStatements 按分号切分 DDL。DDL 里没有字符串字面量，不需要完整解析。
func splitStatements(ddl string) []string {
	var out []string
	for _, part := range strings.Split(ddl, ";") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}
