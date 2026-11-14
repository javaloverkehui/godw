package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是标准 5 段 cron：分 时 日 月 周。
// 支持 *、*/n、a-b、a-b/n、逗号列表；周支持 0-7（0 与 7 都是周日）。
type Schedule struct {
	expr   string
	minute uint64
	hour   uint64
	dom    uint64
	month  uint64
	dow    uint64
}

func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 需要 5 段（分 时 日 月 周），实际 %d 段: %q", len(fields), expr)
	}
	s := &Schedule{expr: expr}
	var err error
	if s.minute, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("分钟段: %w", err)
	}
	if s.hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("小时段: %w", err)
	}
	if s.dom, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("日段: %w", err)
	}
	if s.month, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("月段: %w", err)
	}
	// 周日既可能是 0 也可能是 7，统一到 0-6。
	dowExpr := strings.ReplaceAll(fields[4], "7", "0")
	if s.dow, err = parseField(dowExpr, 0, 6); err != nil {
		return nil, fmt.Errorf("周段: %w", err)
	}
	return s, nil
}

func (s *Schedule) String() string { return s.expr }

// Match 判断给定时刻是否命中。秒级不参与判断，调用方按分钟去重。
func (s *Schedule) Match(t time.Time) bool {
	return bit(s.minute, t.Minute()) &&
		bit(s.hour, t.Hour()) &&
		bit(s.dom, t.Day()) &&
		bit(s.month, int(t.Month())) &&
		bit(s.dow, int(t.Weekday()))
}

// Next 返回严格晚于 t 的下一个命中时刻，用于界面预览与补数推导。
// 最多向后找 366 天，超出即认为表达式不可能命中（如 2 月 30 日）。
func (s *Schedule) Next(t time.Time) (time.Time, bool) {
	cur := t.Truncate(time.Minute).Add(time.Minute)
	limit := cur.AddDate(1, 0, 0)
	for cur.Before(limit) {
		if s.Match(cur) {
			return cur, true
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}, false
}

func parseField(field string, min, max int) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("空表达式")
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("步长非法: %q", part)
			}
			step = n
			part = part[:i]
		}
		lo, hi := min, max
		switch {
		case part == "*":
			// 全范围
		case strings.Contains(part, "-"):
			segs := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(segs[0])
			b, err2 := strconv.Atoi(segs[1])
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("区间非法: %q", part)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return 0, fmt.Errorf("取值非法: %q", part)
			}
			lo, hi = v, v
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("超出范围 [%d,%d]: %q", min, max, part)
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, nil
}

func bit(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }
