package scheduler

import (
	"testing"
	"time"
)

func TestParseAndMatch(t *testing.T) {
	cases := []struct {
		expr string
		at   string
		want bool
	}{
		{"0 2 * * *", "2025-03-01 02:00:00", true},
		{"0 2 * * *", "2025-03-01 02:01:00", false},
		{"*/15 * * * *", "2025-03-01 10:30:00", true},
		{"*/15 * * * *", "2025-03-01 10:31:00", false},
		{"0 0 1 * *", "2025-03-01 00:00:00", true},
		{"0 0 1 * *", "2025-03-02 00:00:00", false},
		{"0 9 * * 1-5", "2025-03-03 09:00:00", true},  // 周一
		{"0 9 * * 1-5", "2025-03-02 09:00:00", false}, // 周日
		{"0 9 * * 0", "2025-03-02 09:00:00", true},    // 周日用 0
		{"0 9 * * 7", "2025-03-02 09:00:00", true},    // 周日用 7
		{"30 8 1,15 * *", "2025-03-15 08:30:00", true},
		{"30 8 1,15 * *", "2025-03-14 08:30:00", false},
		{"0 0 * * *", "2025-03-01 00:00:00", true},
		{"0 0 * * *", "2025-03-01 12:00:00", false},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", c.at, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Match(at); got != c.want {
			t.Errorf("%s @ %s = %v, want %v", c.expr, c.at, got, c.want)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	bad := []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "*/0 * * * *", "a b c d e"}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("%q 应解析失败", expr)
		}
	}
}

func TestNext(t *testing.T) {
	s, err := Parse("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2025, 3, 1, 3, 0, 0, 0, time.Local)
	next, ok := s.Next(from)
	if !ok {
		t.Fatal("应能找到下一次")
	}
	want := time.Date(2025, 3, 2, 2, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

// 不可能命中的表达式不能死循环。
func TestNextImpossible(t *testing.T) {
	s, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Next(time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local)); ok {
		t.Fatal("2 月 30 日不应命中")
	}
}
