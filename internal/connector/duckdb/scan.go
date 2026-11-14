//go:build duckdb

package duckdb

import (
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"time"

	duckdb "github.com/marcboeker/go-duckdb"
)

// Scan 把驱动专有类型归一化后再交给调用方。
//
// 不这么做会出两类问题：
//   - DECIMAL 以 duckdb.Decimal 结构体身份被 JSON 序列化成 {"Width":18,...}；
//   - 指标查询把 SUM(decimal) 扫进 *float64 时直接报
//     「converting driver.Value type duckdb.Decimal ... invalid syntax」。
//
// database/sql 只认基础类型，所以这里先扫到 any，再按目标类型赋值。
func scanRows(rows interface {
	Scan(dest ...any) error
},
	dest ...any) error {
	raw := make([]any, len(dest))
	ptrs := make([]any, len(dest))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return err
	}
	for i, d := range dest {
		if err := assign(d, normalize(raw[i])); err != nil {
			return fmt.Errorf("列 %d: %w", i, err)
		}
	}
	return nil
}

// normalize 把驱动专有类型转成通用类型。
func normalize(v any) any {
	switch t := v.(type) {
	case duckdb.Decimal:
		return formatDecimal(t)
	case *big.Int:
		if t == nil {
			return nil
		}
		return t.String()
	case duckdb.Interval:
		// Interval 没有 String()，按 PostgreSQL 风格拼一个可读形式。
		return fmt.Sprintf("%d mons %d days %d micros", t.Months, t.Days, t.Micros)
	case []any, map[string]any:
		return t
	default:
		return v
	}
}

// assign 按目标类型赋值。覆盖 connector 内部实际会用到的几种：
// *any（预览/导出）、*string、*float64、*int64、*bool、*time.Time。
func assign(dest, src any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("扫描目标必须是非空指针，实际 %T", dest)
	}
	elem := rv.Elem()
	if src == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}
	if elem.Kind() == reflect.Interface {
		elem.Set(reflect.ValueOf(src))
		return nil
	}
	switch elem.Kind() {
	case reflect.String:
		elem.SetString(fmt.Sprint(src))
		return nil
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(fmt.Sprint(src), elem.Type().Bits())
		if err != nil {
			return fmt.Errorf("无法把 %T(%v) 转成 %s", src, src, elem.Kind())
		}
		elem.SetFloat(f)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if d, ok := src.(time.Duration); ok {
			elem.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(fmt.Sprint(src), 10, 64)
		if err != nil {
			return fmt.Errorf("无法把 %T(%v) 转成整数", src, src)
		}
		elem.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(fmt.Sprint(src), 10, 64)
		if err != nil {
			return fmt.Errorf("无法把 %T(%v) 转成无符号整数", src, src)
		}
		elem.SetUint(n)
		return nil
	case reflect.Bool:
		b, err := strconv.ParseBool(fmt.Sprint(src))
		if err != nil {
			return fmt.Errorf("无法把 %T(%v) 转成布尔", src, src)
		}
		elem.SetBool(b)
		return nil
	case reflect.Struct:
		if t, ok := src.(time.Time); ok && elem.Type() == reflect.TypeOf(time.Time{}) {
			elem.Set(reflect.ValueOf(t))
			return nil
		}
		return fmt.Errorf("无法把 %T 赋给 %s", src, elem.Type())
	case reflect.Slice:
		if b, ok := src.([]byte); ok && elem.Type().Elem().Kind() == reflect.Uint8 {
			elem.SetBytes(b)
			return nil
		}
		sv := reflect.ValueOf(src)
		if sv.Type().AssignableTo(elem.Type()) {
			elem.Set(sv)
			return nil
		}
		return fmt.Errorf("无法把 %T 赋给 %s", src, elem.Type())
	}
	sv := reflect.ValueOf(src)
	if sv.Type().AssignableTo(elem.Type()) {
		elem.Set(sv)
		return nil
	}
	if sv.Type().ConvertibleTo(elem.Type()) {
		elem.Set(sv.Convert(elem.Type()))
		return nil
	}
	return fmt.Errorf("无法把 %T 赋给 %s", src, elem.Type())
}
