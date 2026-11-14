package sqlx_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/sqlx"
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
		if err := sqlx.ValidateReadOnlySQL(tc.sql); err != nil {
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
		{"超长", "select '" + strings.Repeat("a", sqlx.MaxRawSQLBytes) + "'"},
	}
	for _, tc := range cases {
		if err := sqlx.ValidateReadOnlySQL(tc.sql); err == nil {
			t.Errorf("%s 应被拒绝，实际放行（SQL: %.60s）", tc.name, tc.sql)
		}
	}
}
