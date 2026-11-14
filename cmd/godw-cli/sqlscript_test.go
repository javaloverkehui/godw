package main

import (
	"strings"
	"testing"
)

func TestSplitSQLScript(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "前置注释不应吞掉语句",
			in:   "-- 说明\n-- 还是说明\nINSERT INTO t VALUES (1);\n",
			want: []string{"INSERT INTO t VALUES (1)"},
		},
		{
			name: "行内注释被移除",
			in:   "SELECT 1 -- 注释里有;分号\n;SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "字符串里的分号不切",
			in:   "INSERT INTO t VALUES ('a;b');",
			want: []string{"INSERT INTO t VALUES ('a;b')"},
		},
		{
			name: "字符串里的双横线不是注释",
			in:   "SELECT '--not-comment';",
			want: []string{"SELECT '--not-comment'"},
		},
		{
			name: "单引号转义",
			in:   "SELECT 'it''s;ok';",
			want: []string{"SELECT 'it''s;ok'"},
		},
		{
			name: "块注释被移除",
			in:   "SELECT /* ; */ 1;",
			want: []string{"SELECT   1"},
		},
		{
			name: "结尾无分号也成句",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "纯注释返回空",
			in:   "-- nothing here\n",
			want: nil,
		},
		{
			name: "多条语句",
			in:   "CREATE TABLE t(a int);\n-- 灌数\nINSERT INTO t SELECT generate_series(1,3);",
			want: []string{"CREATE TABLE t(a int)", "INSERT INTO t SELECT generate_series(1,3)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitSQLScript(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("语句数 %d != %d：%q", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 条：%q != %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestSplitSQLScriptDollarQuoted(t *testing.T) {
	in := `DO $$
BEGIN
  IF to_regclass('dwd.t') IS NOT NULL THEN TRUNCATE dwd.t; END IF;
END $$;
SELECT 1;`
	got := splitSQLScript(in)
	if len(got) != 2 {
		t.Fatalf("应为 2 条语句，实际 %d：%q", len(got), got)
	}
	if !strings.Contains(got[0], "TRUNCATE dwd.t") || !strings.HasSuffix(got[0], "$$") {
		t.Fatalf("DO 块被切坏: %q", got[0])
	}
	if got[1] != "SELECT 1" {
		t.Fatalf("第二条错误: %q", got[1])
	}
}

func TestDollarTag(t *testing.T) {
	cases := map[string]string{"$$": "$$", "$body$": "$body$", "$1": "", "$": "", "abc": ""}
	for in, want := range cases {
		if got := dollarTag(in); got != want {
			t.Fatalf("dollarTag(%q)=%q want %q", in, got, want)
		}
	}
}
