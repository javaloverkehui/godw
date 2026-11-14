package service

import "testing"

// 命中测试的表名匹配规则：全名与末段互相命中，否则「配了却不生效」会被误判成没配。
func TestTableMatches(t *testing.T) {
	cases := []struct {
		policy, target string
		want           bool
	}{
		{"dwd_order", "dwd_order", true},
		{"dwd_order", "dwd.dwd_order", true},
		{"dwd.dwd_order", "dwd_order", true},
		{"DWD.DWD_ORDER", "dwd_order", true},
		{"dwd_order", "dwd_order_v2", false},
		{"order", "dwd.dwd_order", false},
		{"", "dwd_order", false},
		{"dwd_order", "", false},
	}
	for _, c := range cases {
		if got := tableMatches(c.policy, c.target); got != c.want {
			t.Errorf("tableMatches(%q,%q)=%v，期望 %v", c.policy, c.target, got, c.want)
		}
	}
}

// 占位符替换只加一层引号：策略里写 '${user}' 是最自然的写法，
// 早先会渲染成 ”alice”，SQL 语法错误，而且只有命中策略时才暴露。
func TestApplyRowFiltersQuoting(t *testing.T) {
	cases := []struct {
		filter string
		want   string
	}{
		{"region = '${user}'", "(region = 'alice')"},
		{"region = ${user}", "(region = 'alice')"},
		{"owner = '${user}' AND level = ${role}", "(owner = 'alice' AND level = 'viewer')"},
		{"note <> ''", "(note <> '')"},
		// 单引号必须转义，否则 O'Brien 这类取值会把 SQL 截断。
		{"owner = '${user}'", "(owner = 'O''Brien')"},
	}
	for i, c := range cases {
		user := "alice"
		if i == 4 {
			user = "O'Brien"
		}
		if got := ApplyRowFilters([]string{c.filter}, user, "viewer"); got != c.want {
			t.Errorf("ApplyRowFilters(%q)=%q，期望 %q", c.filter, got, c.want)
		}
	}
	if got := ApplyRowFilters([]string{"a=1", "b=2"}, "u", "viewer"); got != "(a=1) AND (b=2)" {
		t.Errorf("多条策略应 AND 拼接，实际 %q", got)
	}
}
