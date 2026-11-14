package modelc

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

func sample() model.ModelSpec {
	return model.ModelSpec{
		Kind:       "Model",
		Name:       "dwd_order",
		Layer:      "dwd",
		Datasource: "pg_main",
		Schema:     "dw",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint", Nullable: false, Comment: "订单号"},
			{Name: "user_id", Type: "bigint", Nullable: false},
			{Name: "amount", Type: "decimal", Nullable: true},
			{Name: "dt", Type: "date", Nullable: false},
		},
		PrimaryKey:  []string{"order_id"},
		PartitionBy: []string{"dt"},
		Indexes:     []model.IndexSpec{{Columns: []string{"user_id", "dt"}}},
	}
}

func TestCompilePostgres(t *testing.T) {
	ddl, err := Compile(sample(), dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "dw"."dwd_order"`,
		`"order_id" bigint NOT NULL`,
		`PRIMARY KEY ("order_id")`,
		`COMMENT ON COLUMN "dw"."dwd_order"."order_id" IS '订单号'`,
		`CREATE INDEX IF NOT EXISTS "idx_dwd_order_user_id_dt"`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL 缺少 %q\n%s", want, ddl)
		}
	}
}

func TestCompileClickHouse(t *testing.T) {
	spec := sample()
	spec.Schema = "dw"
	spec.Columns[2].Nullable = true
	ddl, err := Compile(spec, dialect.ClickHouse{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `dw`",
		"ENGINE = MergeTree()",
		"ORDER BY (`order_id`)",
		"PARTITION BY (`dt`)",
		"`amount` Nullable(Decimal(20,4))",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL 缺少 %q\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "COMMENT ON TABLE") {
		t.Error("ClickHouse 不应生成 COMMENT ON TABLE")
	}
	// ClickHouse 没有 SCHEMA 关键字，写了就是语法错误（真实集群 24.10 实测）。
	if strings.Contains(ddl, "CREATE SCHEMA") {
		t.Errorf("ClickHouse 应使用 CREATE DATABASE\n%s", ddl)
	}
}

func TestValidateCollectsProblems(t *testing.T) {
	spec := sample()
	spec.Name = "9bad-name"
	spec.Columns = append(spec.Columns, model.ColumnSpec{Name: "order_id", Type: "bigint"})
	spec.PrimaryKey = append(spec.PrimaryKey, "ghost")

	err := Validate(spec)
	if err == nil {
		t.Fatal("应校验失败")
	}
	msg := err.Error()
	for _, want := range []string{"name 只能包含", "列名重复", "primary_key 引用了不存在的列"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q: %s", want, msg)
		}
	}
}

func TestCompileDeterministic(t *testing.T) {
	a, _ := Compile(sample(), dialect.Postgres{})
	b, _ := Compile(sample(), dialect.Postgres{})
	if a != b {
		t.Fatal("同一 spec 必须产生完全一致的 DDL，否则渲染快照失去意义")
	}
}
