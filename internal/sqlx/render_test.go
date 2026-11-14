package sqlx

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	got, err := Render("SELECT * FROM t WHERE dt = ${biz_date} AND id > ${min_id}", map[string]any{
		"biz_date": "2025-01-01",
		"min_id":   100,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT * FROM t WHERE dt = '2025-01-01' AND id > 100"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRenderMissingVar(t *testing.T) {
	_, err := Render("SELECT ${a}, ${b}", map[string]any{"a": 1})
	if err == nil {
		t.Fatal("缺变量应报错")
	}
	if !strings.Contains(err.Error(), "b") {
		t.Errorf("错误信息应指出缺失变量: %v", err)
	}
}

// 外部字符串必须转义，不能直接拼进 SQL。
func TestRenderEscapesStrings(t *testing.T) {
	got, err := Render("SELECT ${name}", map[string]any{"name": "a' OR 1=1 --"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a'' OR 1=1 --") {
		t.Errorf("单引号应转义为两个单引号: %s", got)
	}
	if !strings.HasPrefix(got, "SELECT '") || !strings.HasSuffix(got, "'") {
		t.Errorf("整体应是一个被引号包裹的字面量: %s", got)
	}
}

func TestVars(t *testing.T) {
	got := Vars("SELECT ${a}, ${b} FROM x WHERE d='${a}'")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("去重结果错误: %v", got)
	}
}

func TestFingerprintStable(t *testing.T) {
	a := Fingerprint("SELECT  *  FROM T")
	b := Fingerprint("select * from t")
	if a != b {
		t.Error("空白与大小写差异不应改变指纹")
	}
	c := Fingerprint("SELECT * FROM u")
	if a == c {
		t.Error("不同 SQL 不应得到相同指纹")
	}
}
