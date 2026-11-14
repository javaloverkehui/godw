package connector_test

import (
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
)

// 游标越界与列数不匹配必须显式报错：静默返回零值会让下游把「没数据」当成「数据是空的」。
func TestMemRowsCursor(t *testing.T) {
	cols := []connector.ColumnMeta{{Name: "id", Type: "bigint"}}
	r := connector.NewMemRows(cols, [][]any{{int64(1)}, {int64(2)}})

	if got := r.Columns(); len(got) != 1 || got[0].Name != "id" {
		t.Fatalf("Columns 元信息丢失: %+v", got)
	}

	// 未调用 Next 就 Scan：游标为 -1。
	var id int64
	if err := r.Scan(&id); err == nil {
		t.Fatal("游标未开始就 Scan 应报错")
	}

	count := 0
	for r.Next() {
		count++
		if err := r.Scan(&id); err != nil {
			t.Fatalf("第 %d 行扫描失败: %v", count, err)
		}
		if id != int64(count) {
			t.Fatalf("第 %d 行值 = %d", count, id)
		}
	}
	if count != 2 {
		t.Fatalf("迭代行数 = %d, want 2", count)
	}
	if r.Next() {
		t.Fatal("数据耗尽后 Next 必须返回 false")
	}
	if err := r.Scan(&id); err == nil {
		t.Fatal("游标越界后 Scan 应报错")
	}
	if r.Err() != nil {
		t.Fatalf("MemRows 不应有残留错误: %v", r.Err())
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close 应幂等无错: %v", err)
	}
}

func TestMemRowsEmpty(t *testing.T) {
	r := connector.NewMemRows(nil, nil)
	if r.Next() {
		t.Fatal("空结果集不应产出行")
	}
	if err := r.Scan(new(any)); err == nil {
		t.Fatal("空结果集 Scan 应报错")
	}
}

func TestMemRowsTooManyDest(t *testing.T) {
	r := connector.NewMemRows([]connector.ColumnMeta{{Name: "a"}}, [][]any{{1}})
	r.Next()
	var a, b any
	err := r.Scan(&a, &b)
	if err == nil {
		t.Fatal("目标列多于结果列应报错")
	}
	if !strings.Contains(err.Error(), "目标列数") || !strings.Contains(err.Error(), "超过结果列数") {
		t.Fatalf("错误信息不足以定位问题: %v", err)
	}
}

// 目标列少于结果列是合法用法（只取前缀列），不能误报。
func TestMemRowsPartialScan(t *testing.T) {
	r := connector.NewMemRows([]connector.ColumnMeta{{Name: "a"}, {Name: "b"}}, [][]any{{"x", "y"}})
	r.Next()
	var a string
	if err := r.Scan(&a); err != nil {
		t.Fatalf("只取前缀列应允许: %v", err)
	}
	if a != "x" {
		t.Fatalf("a = %q, want x", a)
	}
}

// 单行结果集用于覆盖各扫描目标类型的赋值分支。
func oneRow(t *testing.T, src any) *connector.MemRows {
	t.Helper()
	r := connector.NewMemRows([]connector.ColumnMeta{{Name: "v"}}, [][]any{{src}})
	if !r.Next() {
		t.Fatal("结果集应有一行")
	}
	return r
}

func TestAssignScanAnyAndString(t *testing.T) {
	r := oneRow(t, nil)
	var anyDest any
	if err := r.Scan(&anyDest); err != nil || anyDest != nil {
		t.Fatalf("*any 应原样接收 nil: %v %v", anyDest, err)
	}

	var s string
	if err := r.Scan(&s); err != nil || s != "" {
		t.Fatalf("nil 应赋为空串: %q %v", s, err)
	}
	r = oneRow(t, 42)
	if err := r.Scan(&s); err != nil || s != "42" {
		t.Fatalf("非字符串应退化为字符串: %q %v", s, err)
	}
	r = oneRow(t, "ok")
	if err := r.Scan(&s); err != nil || s != "ok" {
		t.Fatalf("字符串直传失败: %q %v", s, err)
	}
	r = oneRow(t, int64(5))
	if err := r.Scan(&anyDest); err != nil || anyDest != int64(5) {
		t.Fatalf("*any 应原样接收值: %v %v", anyDest, err)
	}
}

func TestAssignScanNumeric(t *testing.T) {
	cases := []struct {
		name string
		src  any
		want int64
	}{
		{"int", 3, 3},
		{"int32", int32(4), 4},
		{"int64", int64(5), 5},
		{"float64 截断", 6.9, 6},
		{"字符串", "7", 7},
		{"非法字符串", "abc", 0},
		{"不支持类型", true, 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := oneRow(t, tc.src)
			var got int64
			if err := r.Scan(&got); err != nil {
				t.Fatalf("int64 扫描不应报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("int64(%v) = %d, want %d", tc.src, got, tc.want)
			}
			r = oneRow(t, tc.src)
			var gi int
			if err := r.Scan(&gi); err != nil {
				t.Fatalf("int 扫描不应报错: %v", err)
			}
			if int64(gi) != tc.want {
				t.Fatalf("int(%v) = %d, want %d", tc.src, gi, tc.want)
			}
		})
	}
}

func TestAssignScanFloat(t *testing.T) {
	cases := []struct {
		src  any
		want float64
	}{
		{1.5, 1.5},
		{float32(2.5), 2.5},
		{3, 3},
		{int64(4), 4},
		{"5.25", 5.25},
		{"bad", 0},
		{true, 0},
	}
	for _, tc := range cases {
		r := oneRow(t, tc.src)
		var got float64
		if err := r.Scan(&got); err != nil {
			t.Fatalf("float64 扫描不应报错: %v", err)
		}
		if got != tc.want {
			t.Errorf("float64(%v) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// 布尔列扫进 *bool 时，非布尔值不能被当成 true（那是数据污染）。
func TestAssignScanBool(t *testing.T) {
	r := oneRow(t, true)
	var b bool
	if err := r.Scan(&b); err != nil || !b {
		t.Fatalf("true 赋值失败: %v %v", b, err)
	}
	r = oneRow(t, false)
	if err := r.Scan(&b); err != nil || b {
		t.Fatalf("false 赋值失败: %v %v", b, err)
	}
	r = oneRow(t, "not-bool")
	b = true
	if err := r.Scan(&b); err != nil {
		t.Fatalf("非布尔源不应报错（保持原值）: %v", err)
	}
	if !b {
		t.Fatal("非布尔源不应改写目标值")
	}
}

func TestAssignScanTime(t *testing.T) {
	base := time.Date(2026, 11, 9, 13, 45, 1, 0, time.UTC)
	cases := []struct {
		name string
		src  any
		want time.Time
	}{
		{"time.Time 直传", base, base},
		{"RFC3339Nano", "2026-11-09T13:45:01Z", base},
		{"带微秒", "2026-11-09 13:45:01.000000", base},
		{"空格分隔", "2026-11-09 13:45:01", base},
		{"T 分隔无时区", "2026-11-09T13:45:01", base},
		{"仅日期", "2026-11-09", time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := oneRow(t, tc.src)
			var got time.Time
			if err := r.Scan(&got); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	r := oneRow(t, nil)
	var got time.Time
	if err := r.Scan(&got); err != nil {
		t.Fatalf("nil 时间应为零值而不是错误: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("nil 应留下零值，得到 %v", got)
	}

	r = oneRow(t, "2026-13-99")
	if err := r.Scan(&got); err == nil {
		t.Fatal("非法时间字符串必须报错，不能静默留下零值")
	}
	r = oneRow(t, 123)
	if err := r.Scan(&got); err == nil {
		t.Fatal("不支持的时间源类型必须报错")
	}
}

func TestAssignScanUnsupportedDest(t *testing.T) {
	r := oneRow(t, "x")
	var b []byte
	err := r.Scan(&b)
	if err == nil {
		t.Fatal("不支持的扫描目标必须报错，而不是静默跳过")
	}
	if !strings.Contains(err.Error(), "不支持的扫描目标") || !strings.Contains(err.Error(), "[]uint8") {
		t.Fatalf("错误信息应带目标类型: %v", err)
	}
}

// MemRows 的错误要带列号，否则宽表里根本定位不到是哪一列类型不匹配。
func TestMemRowsScanErrorHasColumn(t *testing.T) {
	r := connector.NewMemRows([]connector.ColumnMeta{{Name: "a"}, {Name: "b"}}, [][]any{{"x", "y"}})
	r.Next()
	var a string
	var b []byte
	err := r.Scan(&a, &b)
	if err == nil {
		t.Fatal("第二列类型不匹配应报错")
	}
	if !strings.Contains(err.Error(), "第 2 列") || !strings.Contains(err.Error(), "赋值失败") {
		t.Fatalf("错误信息缺少列号: %v", err)
	}
}
