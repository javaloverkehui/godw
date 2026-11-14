package jsonx_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/godw/godw/internal/pkg/jsonx"
)

// 守住关闭 HTML 转义的契约：SQL 片段里的 < > & 被写成 \u003c 后，
// 落库与日志里都无法按原文检索。
func TestMarshalKeepsAngleBrackets(t *testing.T) {
	got, err := jsonx.Marshal(map[string]string{"sql": "a < b && c > d"})
	if err != nil {
		t.Fatalf("Marshal 不应报错: %v", err)
	}
	want := `{"sql":"a < b && c > d"}`
	if string(got) != want {
		t.Fatalf("Marshal = %s，期望 %s", got, want)
	}
	if strings.HasSuffix(string(got), "\n") {
		t.Fatal("Marshal 不应保留 Encoder 追加的换行")
	}
}

// 守住错误透传：不可序列化类型必须返回错误而不是产出半截 JSON。
func TestMarshalError(t *testing.T) {
	_, err := jsonx.Marshal(make(chan int))
	if err == nil {
		t.Fatal("对 chan 序列化应返回错误")
	}
}

// 守住 MustMarshal 的双重语义：正常路径返回字节，异常路径 panic（供常量初始化使用）。
func TestMustMarshal(t *testing.T) {
	if got := string(jsonx.MustMarshal([]int{1, 2})); got != "[1,2]" {
		t.Fatalf("MustMarshal = %s，期望 [1,2]", got)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustMarshal 遇到不可序列化值应 panic")
		}
	}()
	jsonx.MustMarshal(make(chan int))
}

// 守住空输入容错：nil/空白 JSON 视为「字段未提供」，
// 返回错误会让可选 JSONB 字段的读取路径到处判空。
func TestUnmarshalEmptyInput(t *testing.T) {
	cases := [][]byte{nil, {}, []byte("   "), []byte("\n\t ")}
	for _, in := range cases {
		var out map[string]int
		if err := jsonx.Unmarshal(in, &out); err != nil {
			t.Fatalf("Unmarshal(%q) 不应报错: %v", in, err)
		}
		if out != nil {
			t.Fatalf("Unmarshal(%q) 不应写入目标，实际 %#v", in, out)
		}
	}
}

// 守住正常解析与非法 JSON 的错误返回。
func TestUnmarshal(t *testing.T) {
	var out struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if err := jsonx.Unmarshal([]byte(` {"name":"godw","age":3} `), &out); err != nil {
		t.Fatalf("Unmarshal 合法输入报错: %v", err)
	}
	if out.Name != "godw" || out.Age != 3 {
		t.Fatalf("解析结果不符: %#v", out)
	}

	var broken map[string]any
	if err := jsonx.Unmarshal([]byte(`{"name":`), &broken); err == nil {
		t.Fatal("截断的 JSON 应返回错误")
	}
}

// 守住 Flexible 接受的四类输入：null、空串、JSON 字符串、JSON 对象。
// 前端把 JSONB 字段既可能传对象也可能传字符串，任一路径丢失都会导致配置读不出来。
func TestFlexibleAcceptsObjectAndString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{"对象", `{"a":1}`, map[string]any{"a": float64(1)}},
		{"字符串包裹的对象", `"{\"a\":1,\"b\":\"x\"}"`, map[string]any{"a": float64(1), "b": "x"}},
		{"null 保持 nil", `null`, nil},
		{"空字符串保持 nil", `""`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f jsonx.Flexible
			if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
				t.Fatalf("解析 %s 报错: %v", tc.in, err)
			}
			if len(f) != len(tc.want) {
				t.Fatalf("字段数 = %d，期望 %d（%#v）", len(f), len(tc.want), f)
			}
			for k, v := range tc.want {
				if f[k] != v {
					t.Fatalf("字段 %s = %#v，期望 %#v", k, f[k], v)
				}
			}
		})
	}
}

// 守住空输入分支：len(b)==0 必须直接返回，否则 json.Unmarshal 会报 unexpected end of JSON。
func TestFlexibleEmptyBytes(t *testing.T) {
	var f jsonx.Flexible
	if err := f.UnmarshalJSON(nil); err != nil {
		t.Fatalf("空输入不应报错: %v", err)
	}
	if f != nil {
		t.Fatalf("空输入不应改写值，实际 %#v", f)
	}
}

// 守住非法输入的错误信息：字符串内层 JSON 错误需要带 flexible json 前缀，
// 便于定位是哪个字段的嵌套内容坏了。
// 这里直接调 UnmarshalJSON：外层 json.Unmarshal 会先做整体语法校验，
// 非法 JSON 根本到不了这个方法，无法覆盖其内部错误分支。
func TestFlexibleErrors(t *testing.T) {
	t.Run("字符串内层不是 JSON", func(t *testing.T) {
		f := jsonx.Flexible{}
		err := f.UnmarshalJSON([]byte(`"not-json"`))
		if err == nil {
			t.Fatal("应返回错误")
		}
		if !strings.Contains(err.Error(), "flexible json:") {
			t.Fatalf("错误应带 flexible json 前缀，实际 %v", err)
		}
	})

	t.Run("字符串本身格式错误", func(t *testing.T) {
		f := jsonx.Flexible{}
		if err := f.UnmarshalJSON([]byte(`"unterminated`)); err == nil {
			t.Fatal("未闭合字符串应返回错误")
		}
	})

	t.Run("对象格式错误", func(t *testing.T) {
		f := jsonx.Flexible{}
		if err := f.UnmarshalJSON([]byte(`{"a":`)); err == nil {
			t.Fatal("截断对象应返回错误")
		}
	})

	t.Run("非对象标量", func(t *testing.T) {
		f := jsonx.Flexible{}
		if err := f.UnmarshalJSON([]byte(`42`)); err == nil {
			t.Fatal("数字不应被解析为对象")
		}
	})
}
