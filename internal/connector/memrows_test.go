package connector

import (
	"testing"
	"time"
)

// 文本协议的引擎把日期当字符串返回，赋值给 *time.Time 时必须解析而不是留零值。
func TestAssignScanTimeFromString(t *testing.T) {
	cases := map[string]time.Time{
		"2026-11-09":           time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		"2026-11-09 13:45:01":  time.Date(2026, 11, 9, 13, 45, 1, 0, time.UTC),
		"2026-11-09T13:45:01Z": time.Date(2026, 11, 9, 13, 45, 1, 0, time.UTC),
	}
	for in, want := range cases {
		var got time.Time
		if err := assignScan(&got, in); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !got.Equal(want) {
			t.Fatalf("%q => %v, want %v", in, got, want)
		}
	}
	var bad time.Time
	if err := assignScan(&bad, "not-a-time"); err == nil {
		t.Fatal("非法时间不应静默通过")
	}
}
