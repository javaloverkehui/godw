package sqlx

import (
	"strings"
	"testing"
)

func TestValidateReadOnlySQLAllows(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"最简查询", "select 1"},
		{"大写", "SELECT id, name FROM orders WHERE dt >= '2026-11-01'"},
		{"CTE", "with t as (select 1 as a) select a from t"},
		{"末尾分号", "select 1;"},
		{"字面量里的分号", "select 'a;b' as k"},
		{"字面量里的关键字", "select * from orders where action = 'delete'"},
		{"列名含关键字前缀", "select created_at, updated_at, deleted_flag from orders"},
		{"行注释里的分号", "select 1 -- ; drop table x"},
		{"块注释里的关键字", "select /* delete from t */ 1"},
		{"双引号标识符", `select * from "drop"`},
		{"子查询", "select * from (select id from orders) x limit 10"},
		{"聚合与过滤", "select count(*) filter (where status = 'paid') from orders"},
		{"窗口函数", "select row_number() over (partition by shop_id order by dt) from orders"},
	}
	for _, tc := range cases {
		if err := ValidateReadOnlySQL(tc.sql); err != nil {
			t.Errorf("%s 应通过，实际拒绝: %v（SQL: %s）", tc.name, err, tc.sql)
		}
	}
}

func TestValidateReadOnlySQLRejects(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"空", "   "},
		{"插入", "insert into t values (1)"},
		{"更新", "update t set a = 1"},
		{"删除", "delete from t"},
		{"建表", "create table t (id int)"},
		{"删表", "drop table t"},
		{"多语句", "select 1; drop table t"},
		{"CTE 内删除", "with x as (delete from t returning *) select * from x"},
		{"行锁", "select * from t for update"},
		{"共享锁", "select * from t for share"},
		{"grant", "grant select on t to bob"},
		{"copy", "copy t to '/tmp/x'"},
		{"pragma", "pragma table_info(t)"},
		{"超长", "select '" + strings.Repeat("a", MaxRawSQLBytes) + "'"},
	}
	for _, tc := range cases {
		if err := ValidateReadOnlySQL(tc.sql); err == nil {
			t.Errorf("%s 应被拒绝，实际放行（SQL: %.60s）", tc.name, tc.sql)
		}
	}
}

// 拒绝文案要能指导用户，不能只是一句「非法」。
func TestValidateReadOnlySQLMessages(t *testing.T) {
	err := ValidateReadOnlySQL("update t set a = 1")
	if err == nil || !strings.Contains(err.Error(), "SELECT") {
		t.Errorf("起始词被拒时应提示允许的写法，实际: %v", err)
	}
	err = ValidateReadOnlySQL("select 1; drop table t")
	if err == nil || !strings.Contains(err.Error(), "单条语句") {
		t.Errorf("多语句应提示单条限制，实际: %v", err)
	}
	err = ValidateReadOnlySQL("with x as (delete from t returning *) select 1")
	if err == nil || !strings.Contains(err.Error(), "DELETE") {
		t.Errorf("应指出被拦下的关键字，实际: %v", err)
	}
}

// 等长替换：位置不能变，否则分号判断会错位。
func TestStripSQLNoiseKeepsOffsets(t *testing.T) {
	in := "select 'a;b', \"c--d\", `e` -- tail\nfrom t"
	out := stripSQLNoise(in)
	if len(out) != len(in) {
		t.Fatalf("长度变了: %d -> %d", len(in), len(out))
	}
	if strings.Contains(out, ";") {
		t.Errorf("字面量里的分号没有被屏蔽: %q", out)
	}
	if !strings.Contains(out, "select") || !strings.Contains(out, "from t") {
		t.Errorf("关键字被误伤: %q", out)
	}
}
