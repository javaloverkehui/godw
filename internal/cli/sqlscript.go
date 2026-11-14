package cli

import "strings"

// SplitSQLScript 把多语句 SQL 脚本切成可逐条执行的语句。
//
// 为什么要自己写而不用 strings.Split(s, ";"):
//  1. 前置注释会让整条语句被判定为「注释行」而跳过——曾经因此静默丢掉
//     了四张表的 INSERT，脚本仍然打印「灌入成功」，数据却是空的；
//  2. 字符串字面量里的分号（如 'a;b'）会被切断；
//  3. 块注释 /* ... */ 里的分号同样不该切。
//
// 这里用一个小状态机：注释直接丢弃，字符串与美元引用（$$ / $tag$）原样保留，
// 只有它们之外的分号才是语句边界。
func SplitSQLScript(s string) []string {
	var (
		out  []string
		b    strings.Builder
		i    int
		n    = len(s)
		emit = func() {
			if t := strings.TrimSpace(b.String()); t != "" {
				out = append(out, t)
			}
			b.Reset()
		}
	)

	for i < n {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			// 行注释：跳到换行，保留一个空格避免前后 token 粘连。
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')

		case strings.HasPrefix(s[i:], "/*"):
			i += 2
			for i < n && !strings.HasPrefix(s[i:], "*/") {
				i++
			}
			if i < n {
				i += 2
			}
			b.WriteByte(' ')

		case s[i] == '$' && DollarTag(s[i:]) != "":
			// 美元引用：DO $$ ... $$ 里的分号不能切。整段原样复制。
			tag := DollarTag(s[i:])
			b.WriteString(tag)
			i += len(tag)
			end := strings.Index(s[i:], tag)
			if end < 0 { // 未闭合，剩下的全部当作函数体
				b.WriteString(s[i:])
				i = n
				continue
			}
			b.WriteString(s[i : i+end+len(tag)])
			i += end + len(tag)

		case s[i] == '\'' || s[i] == '"':
			quote := s[i]
			b.WriteByte(quote)
			i++
			for i < n {
				b.WriteByte(s[i])
				if s[i] == quote {
					// 连写两个引号是转义，不算结束。
					if i+1 < n && s[i+1] == quote {
						b.WriteByte(s[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}

		case s[i] == ';':
			emit()
			i++

		default:
			b.WriteByte(s[i])
			i++
		}
	}
	emit()
	return out
}

// DollarTag 识别 PostgreSQL 的美元引用起始标记，返回 "$tag$" 形式；
// 不是美元引用时返回空串。只接受 ASCII 标识符，避免把 $1 这类占位符当成引用。
func DollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	i := 1
	for i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i < len(s) && s[i] == '$' {
		return s[:i+1]
	}
	return ""
}
