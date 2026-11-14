package scheduler_test

import (
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/scheduler"
)

func mustParse(t *testing.T, expr string) *scheduler.Schedule {
	t.Helper()
	s, err := scheduler.Parse(expr)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", expr, err)
	}
	return s
}

// 守住 cron 语法矩阵：通配、步长、区间、区间步长、列表、周日 0/7 都要正确命中。
func TestCronParseAndMatchMatrix(t *testing.T) {
	cases := []struct {
		expr string
		at   string
		want bool
	}{
		{"0 2 * * *", "2025-03-01 02:00:00", true},
		{"0 2 * * *", "2025-03-01 02:01:00", false},
		{"*/15 * * * *", "2025-03-01 10:30:00", true},
		{"*/15 * * * *", "2025-03-01 10:31:00", false},
		{"1-5/2 * * * *", "2025-03-01 10:03:00", true},
		{"1-5/2 * * * *", "2025-03-01 10:02:00", false},
		{"0 0 1 * *", "2025-03-01 00:00:00", true},
		{"0 0 1 * *", "2025-03-02 00:00:00", false},
		{"0 9 * * 1-5", "2025-03-03 09:00:00", true},  // 周一
		{"0 9 * * 1-5", "2025-03-02 09:00:00", false}, // 周日
		{"0 9 * * 0", "2025-03-02 09:00:00", true},    // 周日写 0
		{"0 9 * * 7", "2025-03-02 09:00:00", true},    // 周日写 7
		{"30 8 1,15 * *", "2025-03-15 08:30:00", true},
		{"30 8 1,15 * *", "2025-03-14 08:30:00", false},
		{"  0  2 * * *  ", "2025-03-01 02:00:00", true}, // 前后空白与多空格
		{"0 0 29 2 *", "2024-02-29 00:00:00", true},     // 闰日
		{"0 0 29 2 *", "2025-03-01 00:00:00", false},
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		at, err := time.ParseInLocation("2006-01-02 15:04:05", c.at, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Match(at); got != c.want {
			t.Errorf("%s @ %s = %v，期望 %v", c.expr, c.at, got, c.want)
		}
	}
}

// 守住 String：调度器日志会打印原始表达式，不能丢空格或改写。
func TestCronString(t *testing.T) {
	s := mustParse(t, "0 9 * * 1-5")
	if got := s.String(); got != "0 9 * * 1-5" {
		t.Fatalf("String() = %q", got)
	}
}

// 守住 Next 语义：严格晚于入参、按分钟对齐，找不到时返回 false 而不是死循环。
func TestCronNext(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
		ok   bool
	}{
		{"0 2 * * *", "2025-03-01 03:00:00", "2025-03-02 02:00:00", true},
		{"0 2 * * *", "2025-03-01 02:00:00", "2025-03-02 02:00:00", true}, // 严格晚于
		{"*/15 * * * *", "2025-03-01 10:07:00", "2025-03-01 10:15:00", true},
		{"0 0 30 2 *", "2025-01-01 00:00:00", "", false}, // 2 月 30 日不可能命中
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		from, err := time.ParseInLocation("2006-01-02 15:04:05", c.from, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := s.Next(from)
		if ok != c.ok {
			t.Fatalf("%s Next 命中 = %v，期望 %v", c.expr, ok, c.ok)
		}
		if !ok {
			if !got.IsZero() {
				t.Fatalf("不命中时应返回零值时间，实际 %v", got)
			}
			continue
		}
		want, _ := time.ParseInLocation("2006-01-02 15:04:05", c.want, time.Local)
		if !got.Equal(want) {
			t.Fatalf("%s Next = %v，期望 %v", c.expr, got, want)
		}
		if got.Second() != 0 || got.Nanosecond() != 0 {
			t.Fatalf("Next 应对齐到分钟: %v", got)
		}
	}
}

// 守住 cron 解析失败的定位信息：每段都有独立前缀，用户能直接看出是第几段写错。
func TestCronParseErrors(t *testing.T) {
	cases := []struct {
		expr    string
		wantSub string
	}{
		{"", "5 段"},             // 段数不足
		{"* * * *", "5 段"},      // 段数不足
		{"* * * * * *", "5 段"},  // 段数过多
		{"   ", "5 段"},          // 全空白
		{"60 * * * *", "分钟段"},   // 超出范围
		{"abc * * * *", "分钟段"},  // 非数字
		{"*/0 * * * *", "分钟段"},  // 步长为 0
		{"*/x * * * *", "分钟段"},  // 步长非数字
		{"*/-1 * * * *", "分钟段"}, // 步长为负
		{"1,,2 * * * *", "分钟段"}, // 空列表项
		{"1-a * * * *", "分钟段"},  // 区间端点非数字
		{"5-1 * * * *", "分钟段"},  // 区间倒置
		{"* 24 * * *", "小时段"},   // 小时越界
		{"* * 0 * *", "日段"},     // 日从 1 开始
		{"* * * 13 *", "月段"},    // 月份越界
		{"* * * * 8", "周段"},     // 周越界（7 已被归一为 0）
	}
	for _, c := range cases {
		_, err := scheduler.Parse(c.expr)
		if err == nil {
			t.Errorf("%q 应解析失败", c.expr)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%q 错误信息 %q 缺少 %q", c.expr, err.Error(), c.wantSub)
		}
	}
}

// 守住边界值：0 分、59 分、23 时、31 日、12 月、周六都应可解析。
func TestCronBoundaries(t *testing.T) {
	exprs := []string{
		"0 0 1 1 0",
		"59 23 31 12 6",
		"0-59 0-23 1-31 1-12 0-6",
		"0-59/59 0-23/23 1-31/30 1-12/11 0-6/6",
	}
	for _, e := range exprs {
		if _, err := scheduler.Parse(e); err != nil {
			t.Errorf("%q 应解析成功: %v", e, err)
		}
	}
}
