package connector_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"

	// 只为触发 init() 注册。测试必须对「真实注册的能力」断言——
	// 早先这里在包内用一张手写的能力表做断言，等于自己验自己：
	// ClickHouse 明明声明了并不存在的 InsertOverwrite，测试依然是绿的，
	// 直到在真实集群上执行分区覆写才暴露。
	_ "github.com/godw/godw/internal/connector/clickhouse"
	_ "github.com/godw/godw/internal/connector/duckdb"
	_ "github.com/godw/godw/internal/connector/mysql"
	_ "github.com/godw/godw/internal/connector/postgres"
)

// T0206 验收：能力声明错一个，写入策略就会生成跑不通的 SQL。
func TestCapabilityMatrixConsistency(t *testing.T) {
	ctx := context.Background()
	seen := map[string]bool{}
	for _, typ := range connector.Types() {
		c, err := connector.New(ctx, connector.Spec{Type: typ, Name: "test-" + typ})
		if err != nil {
			// duckdb 在默认构建下是「请加 -tags duckdb」的占位实现，构造失败属预期。
			if typ == "duckdb" {
				t.Logf("duckdb 未编译：%v", err)
				continue
			}
			t.Fatalf("%s 构造失败: %v", typ, err)
		}
		caps := c.Capabilities()
		d := c.Dialect()
		if d == nil || d.Name() != typ {
			t.Fatalf("%s 的方言不匹配: %v", typ, d)
		}
		if _, ok := dialect.Get(typ); !ok {
			t.Fatalf("方言层缺少引擎: %s", typ)
		}
		checkCaps(t, typ, caps)
		seen[typ] = true
		_ = c.Close()
	}
	for _, must := range []string{"postgres", "mysql", "clickhouse"} {
		if !seen[must] {
			t.Fatalf("连接器未注册: %s", must)
		}
	}
}

func checkCaps(t *testing.T, typ string, c connector.Capability) {
	t.Helper()
	switch typ {
	case "postgres":
		if !c.MergeUpsert || !c.TransactionalDDL || !c.WindowFunction {
			t.Fatalf("postgres 能力声明不完整: %+v", c)
		}
		if c.InsertOverwrite {
			t.Fatal("postgres 不支持 INSERT OVERWRITE，不应声明")
		}
	case "mysql":
		if !c.MergeUpsert {
			t.Fatal("mysql 应声明 MergeUpsert（ON DUPLICATE KEY）")
		}
		if c.TransactionalDDL {
			t.Fatal("mysql 的 DDL 隐式提交，不应声明 TransactionalDDL")
		}
	case "clickhouse":
		// ClickHouse 只能用 ALTER TABLE ... REPLACE PARTITION；INSERT OVERWRITE 是 Hive 语法。
		if c.InsertOverwrite {
			t.Fatal("ClickHouse 没有 INSERT OVERWRITE，只能 REPLACE PARTITION")
		}
		if !c.ReplacePartition {
			t.Fatal("ClickHouse 应声明 ReplacePartition")
		}
		if c.TransactionalDDL {
			t.Fatal("ClickHouse 无事务，不应声明 TransactionalDDL")
		}
	case "duckdb":
		if !c.MergeUpsert {
			t.Fatal("duckdb 应声明 MergeUpsert（ON CONFLICT）")
		}
	}
}
