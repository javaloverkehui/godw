package connector

import (
	"fmt"
	"time"
)

// MemRows 是内存结果集，用于 Stream 分批、文件类数据源与测试。
type MemRows struct {
	cols   []ColumnMeta
	data   [][]any
	cursor int
}

func NewMemRows(cols []ColumnMeta, data [][]any) *MemRows {
	return &MemRows{cols: cols, data: data, cursor: -1}
}

func (r *MemRows) Columns() []ColumnMeta { return r.cols }

func (r *MemRows) Next() bool {
	r.cursor++
	return r.cursor < len(r.data)
}

func (r *MemRows) Scan(dest ...any) error {
	if r.cursor < 0 || r.cursor >= len(r.data) {
		return fmt.Errorf("MemRows: 游标越界")
	}
	row := r.data[r.cursor]
	if len(dest) > len(row) {
		return fmt.Errorf("MemRows: 目标列数 %d 超过结果列数 %d", len(dest), len(row))
	}
	for i, d := range dest {
		if err := assignScan(d, row[i]); err != nil {
			return fmt.Errorf("MemRows: 第 %d 列赋值失败: %w", i+1, err)
		}
	}
	return nil
}

func (r *MemRows) Err() error   { return nil }
func (r *MemRows) Close() error { return nil }

// assignScan 支持常用标量类型，Unsupported 时退化为字符串赋值。
func assignScan(dest, src any) error {
	switch d := dest.(type) {
	case *any:
		*d = src
		return nil
	case *string:
		if src == nil {
			*d = ""
			return nil
		}
		*d = toString(src)
		return nil
	case *int64:
		*d = toInt64(src)
		return nil
	case *int:
		*d = int(toInt64(src))
		return nil
	case *float64:
		*d = toFloat64(src)
		return nil
	case *bool:
		if b, ok := src.(bool); ok {
			*d = b
		}
		return nil
	case *time.Time:
		switch v := src.(type) {
		case time.Time:
			*d = v
			return nil
		case nil:
			return nil
		case string:
			// ClickHouse / Hive 这类走 HTTP/文本协议的引擎，日期就是字符串。
			// 早先这里只接受 time.Time，字符串被静默跳过，指标的时间轴全变成 0001-01-01。
			for _, layout := range []string{
				time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05",
				"2006-01-02T15:04:05", "2006-01-02",
			} {
				if t, err := time.Parse(layout, v); err == nil {
					*d = t
					return nil
				}
			}
			return fmt.Errorf("无法把 %q 解析成时间", v)
		}
		return fmt.Errorf("不支持的时间源类型 %T", src)
	}
	return fmt.Errorf("不支持的扫描目标 %T", dest)
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		var out int64
		fmt.Sscan(n, &out)
		return out
	}
	return 0
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		var out float64
		fmt.Sscan(n, &out)
		return out
	}
	return 0
}
