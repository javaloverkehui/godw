package sqlx_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/sqlx"
)

// 字符串里的转义单引号不能被当成字面量结束，否则后续分号会被误判。
func TestValidateReadOnlySQLEscapedQuote(t *testing.T) {
	if err := sqlx.ValidateReadOnlySQL("SELECT 'it''s ok; still one' AS v FROM t"); err != nil {
		t.Fatalf("转义引号应通过: %v", err)
	}
	// 注释也要按等长空格抹掉，避免注释里的 delete 触发误判。
	if err := sqlx.ValidateReadOnlySQL("SELECT 1 -- delete from t\nFROM t"); err != nil {
		t.Fatalf("行注释应被忽略: %v", err)
	}
	if err := sqlx.ValidateReadOnlySQL("SELECT /* drop table t */ 1"); err != nil {
		t.Fatalf("块注释应被忽略: %v", err)
	}
	// 首字符不是字母（数字/符号）时 leadingWord 为空，错误信息要明确。
	err := sqlx.ValidateReadOnlySQL("1 SELECT")
	if err == nil || !strings.Contains(err.Error(), "（空）") {
		t.Fatalf("空前导词应给出可读提示，实际 %v", err)
	}
}

// 审计指纹必须忽略空白差异，同一句换缩进不能变成两条记录。
func TestSQLFingerprint(t *testing.T) {
	a := sqlx.SQLFingerprint("SELECT  *\nFROM t")
	b := sqlx.SQLFingerprint("SELECT * FROM t")
	if a != b {
		t.Fatalf("空白差异不应影响指纹: %s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("指纹长度应为 16，实际 %d", len(a))
	}
	if sqlx.SQLFingerprint("SELECT * FROM u") == a {
		t.Fatal("不同 SQL 不应得到相同指纹")
	}
}
