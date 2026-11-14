package sqlx

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
)

// T0406 验收：6 策略 × 4 引擎快照；能力不足必须报错而不是静默全量覆盖。

func baseSpec(d dialect.Dialect, caps Caps) WriteSpec {
	return WriteSpec{
		Dialect: d, Caps: caps,
		Table:           d.QuoteIdent("dwd") + "." + d.QuoteIdent("dwd_order"),
		Columns:         []string{"order_id", "amount", "dt"},
		Keys:            []string{"order_id"},
		Mode:            Append,
		Select:          "SELECT order_id, amount, dt FROM src",
		PartitionColumn: "dt", PartitionValue: "2025-11-10",
	}
}

func TestAppend(t *testing.T) {
	p, err := PlanWrite(baseSpec(dialect.Postgres{}, Caps{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Statements[0], `INSERT INTO "dwd"."dwd_order" ("order_id", "amount", "dt")`) {
		t.Fatalf("append SQL 异常: %s", p.Statements[0])
	}
}

func TestOverwrite(t *testing.T) {
	// 有事务：TRUNCATE + INSERT 同事务
	p, err := PlanWrite(baseSpec(dialect.Postgres{}, Caps{Transactional: true}))
	s := baseSpec(dialect.Postgres{}, Caps{Transactional: true})
	s.Mode = Overwrite
	p, err = PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InTx || len(p.Statements) != 2 || !strings.HasPrefix(p.Statements[0], "TRUNCATE") {
		t.Fatalf("overwrite 计划异常: %+v", p)
	}

	// 无事务：走换名，绝不裸 TRUNCATE 无保护
	s2 := baseSpec(dialect.ClickHouse{}, Caps{})
	s2.Mode = Overwrite
	p2, err := PlanWrite(s2)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Path != "swap" {
		t.Fatalf("无事务引擎 overwrite 应走 swap，实际 %s", p2.Path)
	}
	for _, st := range p2.Statements {
		if strings.HasPrefix(st, "TRUNCATE") {
			t.Fatalf("不应出现无保护 TRUNCATE: %s", st)
		}
	}
}

func TestMergeByEngine(t *testing.T) {
	cases := []struct {
		d    dialect.Dialect
		caps Caps
		path string
		want string
	}{
		{dialect.Postgres{}, Caps{MergeUpsert: true}, "on_conflict", "ON CONFLICT"},
		{dialect.DuckDB{}, Caps{MergeUpsert: true}, "on_conflict", "ON CONFLICT"},
		{dialect.MySQL{}, Caps{MergeUpsert: true}, "on_duplicate_key", "ON DUPLICATE KEY UPDATE"},
		{dialect.ClickHouse{}, Caps{}, "alter_delete+insert", "ALTER TABLE"},
	}
	for _, c := range cases {
		s := baseSpec(c.d, c.caps)
		s.Mode = Merge
		p, err := PlanWrite(s)
		if err != nil {
			t.Fatalf("%s merge 失败: %v", c.d.Name(), err)
		}
		if p.Path != c.path {
			t.Fatalf("%s 路径 %s，期望 %s", c.d.Name(), p.Path, c.path)
		}
		if !strings.Contains(strings.Join(p.Statements, "\n"), c.want) {
			t.Fatalf("%s SQL 缺少 %q: %+v", c.d.Name(), c.want, p.Statements)
		}
	}
}

func TestMergeRequiresKey(t *testing.T) {
	s := baseSpec(dialect.Postgres{}, Caps{})
	s.Mode = Merge
	s.Keys = nil
	if _, err := PlanWrite(s); err == nil {
		t.Fatal("merge 无键应报错")
	}
}

func TestSCD2(t *testing.T) {
	s := baseSpec(dialect.Postgres{}, Caps{Transactional: true})
	s.Mode = SCD2
	s.Columns = []string{"user_id", "name", "level"}
	s.Keys = []string{"user_id"}
	s.Select = "SELECT user_id, name, level FROM src"
	s.SCD2 = SCD2Options{BizDate: "2025-11-10"}
	p, err := PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InTx || len(p.Statements) != 2 {
		t.Fatalf("scd2 计划异常: %+v", p)
	}
	if !strings.Contains(p.Statements[0], "SET \"valid_to\" = '2025-11-10'") {
		t.Fatalf("关链语句缺少 end_date: %s", p.Statements[0])
	}
	if !strings.Contains(p.Statements[0], `"is_current" = 0`) {
		t.Fatalf("关链语句缺少 is_current=0: %s", p.Statements[0])
	}
	if !strings.Contains(p.Statements[1], "SELECT") || !strings.Contains(p.Statements[1], "'2025-11-10', NULL, 1") {
		t.Fatalf("开链语句异常: %s", p.Statements[1])
	}

	// 无事务引擎必须拒绝
	s2 := baseSpec(dialect.ClickHouse{}, Caps{})
	s2.Mode = SCD2
	s2.SCD2 = SCD2Options{BizDate: "2025-11-10"}
	if _, err := PlanWrite(s2); err == nil {
		t.Fatal("无事务引擎 scd2 应报错")
	}
}

func TestOverwritePartitionPriority(t *testing.T) {
	// 1) INSERT OVERWRITE 优先
	s := baseSpec(dialect.MySQL{}, Caps{InsertOverwrite: true})
	s.Mode = OverwritePartition
	p, err := PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != "insert_overwrite" || !strings.Contains(p.Statements[0], "INSERT OVERWRITE TABLE") {
		t.Fatalf("优先级 1 失败: %+v", p)
	}

	// 2) REPLACE PARTITION
	s.Caps = Caps{ReplacePartition: true}
	p, err = PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != "replace_partition" || !strings.Contains(strings.Join(p.Statements, "\n"), "REPLACE PARTITION") {
		t.Fatalf("优先级 2 失败: %+v", p)
	}

	// 3) DELETE + INSERT（需事务）
	s.Caps = Caps{Transactional: true}
	p, err = PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != "delete+insert" || !p.InTx {
		t.Fatalf("优先级 3 失败: %+v", p)
	}

	// 4) 三档都不支持 → 明确报错
	s.Caps = Caps{}
	if _, err := PlanWrite(s); err == nil {
		t.Fatal("三档都不支持时应报错，不得静默全量覆盖")
	}
}

func TestDeleteInsert(t *testing.T) {
	s := baseSpec(dialect.Postgres{}, Caps{Transactional: true})
	s.Mode = DeleteInsert
	p, err := PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InTx || !strings.Contains(p.Statements[0], `DELETE FROM "dwd"."dwd_order" WHERE ("order_id") IN`) {
		t.Fatalf("delete_insert 异常: %+v", p)
	}
}

func TestFullRefreshSwap(t *testing.T) {
	s := baseSpec(dialect.Postgres{}, Caps{Transactional: true})
	s.Mode = FullRefresh
	p, err := PlanWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != "swap" || len(p.Statements) != 7 {
		t.Fatalf("full_refresh 异常: %+v", p)
	}
	joined := strings.Join(p.Statements, "\n")
	if !strings.Contains(joined, "CREATE TABLE") || !strings.Contains(joined, "RENAME TO") {
		t.Fatalf("swap 语句缺少关键步骤: %s", joined)
	}
	// 旧表在最后才删，中途失败仍可回退
	if !strings.HasSuffix(p.Statements[len(p.Statements)-1], "__godw_old") {
		t.Fatalf("最后一步应删除备份表: %s", p.Statements[len(p.Statements)-1])
	}
}

func TestUnsupportedMode(t *testing.T) {
	s := baseSpec(dialect.Postgres{}, Caps{})
	s.Mode = "unknown"
	if _, err := PlanWrite(s); err == nil {
		t.Fatal("未知策略应报错")
	}
}

// REPLACE PARTITION 的临时表名必须在引号内追加，否则 `dw`.`tbl`__tmp 是语法错误。
func TestTmpName(t *testing.T) {
	q := func(s string) string { return "`" + s + "`" }
	cases := map[string]string{
		"`dw`.`dws_order`": "`dw`.`dws_order__godw_tmp`",
		"`dws_order`":      "`dws_order__godw_tmp`",
		"dws_order":        "dws_order__godw_tmp",
		`"dw"."tbl"`:       `"dw"."tbl__godw_tmp"`,
	}
	for in, want := range cases {
		if got := tmpName(in, q); got != want {
			t.Fatalf("tmpName(%q) = %q, want %q", in, got, want)
		}
	}
}
