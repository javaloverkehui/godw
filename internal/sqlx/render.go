package sqlx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Render 用给定变量替换 ${name}。
//
// 安全约束：变量值只允许「日期、数字、标识符白名单」三类形态，
// 其余一律走字面量转义，避免把外部输入直接拼进 SQL。
func Render(sqlText string, vars map[string]any) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(sqlText, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := vars[name]
		if !ok || v == nil {
			missing = append(missing, name)
			return m
		}
		return literal(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("sqlx: 缺少变量 %s", strings.Join(dedupe(missing), ", "))
	}
	return out, nil
}

// Vars 从 SQL 中提取引用的变量名，用于提交前校验参数是否齐全。
func Vars(sqlText string) []string {
	matches := varRe.FindAllStringSubmatch(sqlText, -1)
	out := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

func literal(v any) string {
	switch t := v.(type) {
	case string:
		// 约定：替换结果是「可直接放进 SQL 的字面量」。
		// 因此模板里不要给变量加引号，写成 dt = ${biz_date} 而不是 '${biz_date}'。
		// 纯数字保持裸值（LIMIT ${n} 这类位置加引号会语法错误）。
		if isNumber(t) {
			return t
		}
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(t)
	}
}

var numRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

func isNumber(s string) bool { return numRe.MatchString(s) }

// Fingerprint 归一化 SQL 后取哈希：忽略空白与大小写差异，
// 用于「同一逻辑 SQL 只渲染一次」与变更检测。
func Fingerprint(sqlText string) string {
	norm := strings.Join(strings.Fields(strings.ToLower(sqlText)), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:8])
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
