//go:build duckdb

package duckdb_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/connector/duckdb"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/sqlx"
)

// 真实 DuckDB 集成测试：默认构建不含驱动，用 `go test -tags duckdb ./internal/connector/duckdb/` 运行。
// 这些用例的存在意义是把「只在真实引擎上才暴露」的问题钉住，
// 例如事务内 DELETE+INSERT 相同主键会被唯一索引拒绝。

func openDuck(t *testing.T) *Conn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.duckdb")
	c, err := duckdb.New(context.Background(), connector.Spec{
		Type: "duckdb", Config: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*Conn)
}

func mustExec(t *testing.T, c *Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func count(t *testing.T, c *Conn, q string) int {
	t.Helper()
	rows, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		n = v
	}
	return n
}

// 分区覆写必须可以重复执行：第二次跑同一业务日期，结果仍然是 1 行而不是报主键冲突。
func TestOverwritePartitionRerun(t *testing.T) {
	c := openDuck(t)
	ctx := context.Background()
	mustExec(t, c,
		`CREATE TABLE t (id VARCHAR, dt DATE, PRIMARY KEY (id, dt))`,
		`INSERT INTO t VALUES ('a', DATE '2026-11-09'), ('b', DATE '2026-11-10')`,
	)
	plan, err := sqlx.PlanWrite(sqlx.WriteSpec{
		Mode: sqlx.OverwritePartition, Dialect: dialect.DuckDB{},
		Caps:            sqlx.Caps{Transactional: true},
		Table:           `"t"`,
		Columns:         []string{"id", "dt"},
		Select:          `SELECT 'a', DATE '2026-11-09'`,
		PartitionColumn: "dt", PartitionValue: "2026-11-09",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.InTx {
		t.Fatal("DuckDB 分区覆写不应要求单事务：事务内删除+插入相同主键会被唯一索引拒绝")
	}
	for i := 0; i < 2; i++ {
		for _, s := range plan.Statements {
			if _, err := c.Exec(ctx, s); err != nil {
				t.Fatalf("第 %d 次执行失败: %v", i+1, err)
			}
		}
	}
	// 11-09 被替换成 1 行，11-10 不受影响
	if n := count(t, c, `SELECT count(*) FROM t`); n != 2 {
		t.Fatalf("总行数 = %d, want 2", n)
	}
	if n := count(t, c, `SELECT count(*) FROM t WHERE dt = DATE '2026-11-09'`); n != 1 {
		t.Fatalf("11-09 行数 = %d, want 1", n)
	}
}

// merge 走 INSERT ... ON CONFLICT，单语句幂等。
func TestMergeUpsert(t *testing.T) {
	c := openDuck(t)
	mustExec(t, c, `CREATE TABLE m (id VARCHAR, amt INTEGER, PRIMARY KEY (id))`)
	plan, err := sqlx.PlanWrite(sqlx.WriteSpec{
		Mode: sqlx.Merge, Dialect: dialect.DuckDB{},
		Caps:    sqlx.Caps{MergeUpsert: true, Transactional: true},
		Table:   `"m"`,
		Columns: []string{"id", "amt"},
		Keys:    []string{"id"},
		Select:  `SELECT 'a', 2`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for _, s := range plan.Statements {
			if _, err := c.Exec(context.Background(), s); err != nil {
				t.Fatalf("merge 第 %d 次失败: %v\n%s", i+1, err, s)
			}
		}
	}
	if n := count(t, c, `SELECT count(*) FROM m`); n != 1 {
		t.Fatalf("merge 后行数 = %d, want 1", n)
	}
}

// DECIMAL 必须输出定点字符串，而不是 {Width,Scale,Value} 结构体或 float64。
func TestDecimalScan(t *testing.T) {
	c := openDuck(t)
	mustExec(t, c, `CREATE TABLE d (v DECIMAL(18,2))`,
		`INSERT INTO d VALUES (208.50), (-0.05), (0)`)
	rows, err := c.Query(context.Background(), `SELECT v FROM d ORDER BY v DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		s, ok := v.(string)
		if !ok {
			t.Fatalf("DECIMAL 扫描结果类型 = %T，期望 string", v)
		}
		got = append(got, s)
	}
	want := []string{"208.50", "0.00", "-0.05"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// 指标查询会把 SUM(decimal) 直接扫进 *float64，这条路径不能报类型转换错误。
func TestDecimalIntoFloat(t *testing.T) {
	c := openDuck(t)
	mustExec(t, c, `CREATE TABLE d2 (v DECIMAL(18,2))`,
		`INSERT INTO d2 VALUES (208.50), (45.20)`)
	rows, err := c.Query(context.Background(), `SELECT sum(v) FROM d2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("没有返回行")
	}
	var f float64
	if err := rows.Scan(&f); err != nil {
		t.Fatalf("扫进 *float64 失败: %v", err)
	}
	if f < 253.69 || f > 253.71 {
		t.Fatalf("sum = %v, want 253.70", f)
	}
}

// 采集元数据时 column_default 为 NULL 不能导致整表失败。
func TestDescribeTableNullDefault(t *testing.T) {
	c := openDuck(t)
	mustExec(t, c, `CREATE TABLE meta_t (id INTEGER, note VARCHAR DEFAULT 'x')`)
	m, err := c.DescribeTable(context.Background(), "main", "meta_t")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Columns) != 2 {
		t.Fatalf("列数 = %d, want 2", len(m.Columns))
	}
	if m.Columns[0].Default != nil {
		t.Fatalf("id 不应有默认值: %v", m.Columns[0].Default)
	}
	if m.Columns[1].Default == nil || *m.Columns[1].Default != "'x'" {
		t.Fatalf("note 默认值 = %v", m.Columns[1].Default)
	}
}
