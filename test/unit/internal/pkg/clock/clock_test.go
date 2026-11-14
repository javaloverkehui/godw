package clock_test

import (
	"testing"
	"time"

	"github.com/godw/godw/internal/pkg/clock"
)

// 守住 Real() 必须返回真实时间：若它退化成固定值，调度器的租约续约会被判为过期。
func TestRealFollowsSystemTime(t *testing.T) {
	c := clock.Real()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real().Now() 落在 [%v, %v] 之外: %v", before, after, got)
	}
}

// 守住 Fixed 的快照语义：多次读取必须完全一致，否则基于固定时钟的定时测试会偶发漂移。
func TestFixedIsStable(t *testing.T) {
	want := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	c := clock.Fixed{T: want}
	for i := 0; i < 3; i++ {
		if got := c.Now(); !got.Equal(want) {
			t.Fatalf("第 %d 次读取 Fixed 得到 %v，期望 %v", i, got, want)
		}
	}
}

// 守住 Travel 的推进步长：每次调用必须比上一次晚 Step，回退会让「时间已过期」判断失效。
func TestTravelAdvancesByStep(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	step := 90 * time.Second
	c := &clock.Travel{Base: base, Step: step}
	for i := 0; i < 4; i++ {
		want := base.Add(time.Duration(i) * step)
		if got := c.Now(); !got.Equal(want) {
			t.Fatalf("第 %d 次读取 Travel 得到 %v，期望 %v", i, got, want)
		}
	}
}

// 守住 UTC 的两个约束：转成 UTC 且截断到秒。遗漏截断会让同一逻辑秒写入不同分区键。
func TestUTCTruncatesToSecond(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	in := time.Date(2026, 3, 4, 9, 8, 7, 999_999_999, cst)
	got := clock.UTC(in)
	want := time.Date(2026, 3, 4, 1, 8, 7, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("UTC(%v) = %v，期望 %v", in, got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("UTC 结果时区应为 UTC，实际 %v", got.Location())
	}
	if got.Nanosecond() != 0 {
		t.Fatalf("UTC 结果应截断到秒，仍有余数 %d ns", got.Nanosecond())
	}
}

// 守住幂等：已规范化的时间再次 UTC 不应发生变化。
func TestUTCIsIdempotent(t *testing.T) {
	in := time.Date(2026, 3, 4, 1, 8, 7, 0, time.UTC)
	if got := clock.UTC(clock.UTC(in)); !got.Equal(in) {
		t.Fatalf("UTC 不幂等: %v -> %v", in, got)
	}
}
