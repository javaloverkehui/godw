package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Marshal 序列化时关闭 HTML 转义，避免 SQL 片段里的 < > 被写成 \u003c。
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MustMarshal 仅用于常量与测试。
func MustMarshal(v any) []byte {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Unmarshal 对 nil 输入返回零值，避免调用方到处判空。
func Unmarshal(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// Flexible 用于解析前端传来的 JSONB 字段：既接受对象也接受字符串形式。
type Flexible map[string]any

func (f *Flexible) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return fmt.Errorf("flexible json: %w", err)
		}
		*f = m
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*f = m
	return nil
}
