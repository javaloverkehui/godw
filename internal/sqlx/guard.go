package sqlx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
)

// 自助查询的只读守卫。
//
// 这里做的是「语法白名单 + 单语句 + 关键字黑名单」三道静态约束。它挡住的是误操作，
// 不是恶意攻击：真正无法被绕过的防线是给分析账号只读权限（见 docs/14 §4.3）。
// 所以拒绝时的文案要能指导用户，而不是只说「非法」。
const (
	// MaxRawSQLBytes 是单条 SQL 的长度上限。超长的多半是误粘贴了整个脚本。
	MaxRawSQLBytes = 20_000
	// MaxRawSQLLimit 是结果行数的硬上限。
	MaxRawSQLLimit = 5000
)

// 允许的起始关键字：查询与 CTE。
var allowedLeading = map[string]bool{"select": true, "with": true}

// 出现在任何位置都直接拒绝的关键字。
//
// 为什么除了首词还要扫全文：PostgreSQL 的 WITH 可以包数据修改语句
// （WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x），首词检查拦不住。
var forbiddenWords = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|drop|alter|create|` +
	`grant|revoke|copy|call|execute|exec|attach|detach|install|load|pragma|` +
	`vacuum|reindex|refresh|cluster|listen|notify|unlisten|discard)\b`)

// 行锁同样是写语义：拿锁的查询会阻塞别人的写入。
var forbiddenLocks = regexp.MustCompile(`(?i)\bfor\s+(update|share|no\s+key\s+update|key\s+share)\b`)

// ValidateReadOnlySQL 校验一条自助查询是否只读。
// 通过返回 nil；不通过返回带具体原因的 CodeInvalidArgument。
func ValidateReadOnlySQL(text string) error {
	if strings.TrimSpace(text) == "" {
		return errs.New(errs.CodeInvalidArgument, "SQL 不能为空")
	}
	if len(text) > MaxRawSQLBytes {
		return errs.New(errs.CodeInvalidArgument,
			fmt.Sprintf("SQL 过长（%d 字节，上限 %d）", len(text), MaxRawSQLBytes))
	}

	code := stripSQLNoise(text)

	// 单语句：允许末尾一个分号，但不允许分号后面还有内容。
	if i := strings.IndexByte(code, ';'); i >= 0 {
		if strings.TrimSpace(code[i+1:]) != "" {
			return errs.New(errs.CodeInvalidArgument, "只允许单条语句：分号后面还有内容")
		}
		code = code[:i]
	}

	lead := leadingWord(code)
	if !allowedLeading[lead] {
		return errs.New(errs.CodeInvalidArgument,
			"只允许 SELECT 或 WITH 开头的只读查询，当前是 "+quoteWord(lead))
	}
	if m := forbiddenWords.FindString(code); m != "" {
		return errs.New(errs.CodeInvalidArgument,
			"查询里出现了写操作关键字 "+strings.ToUpper(m)+"（含 CTE 内的数据修改），已拒绝")
	}
	if m := forbiddenLocks.FindString(code); m != "" {
		return errs.New(errs.CodeInvalidArgument,
			"查询里出现了行锁 "+strings.ToUpper(strings.Join(strings.Fields(m), " "))+"，自助查询不允许加锁")
	}
	return nil
}

// stripSQLNoise 把字符串字面量与注释替换成等长空格。
//
// 等长是关键：替换后位置不变，分号与关键字的判断不会被字符串里的内容干扰
// （例如 select 'a;b' 或 where action = 'delete'）。
// 已知不处理 $$dollar quoted$$：这类语法在自助查询里几乎不出现，
// 且误判方向是「拒绝」而不是「放行」。
func stripSQLNoise(sql string) string {
	b := []byte(sql)
	out := make([]byte, len(b))
	copy(out, b)
	i := 0
	for i < len(b) {
		switch {
		case b[i] == '\'' || b[i] == '"' || b[i] == '`':
			q := b[i]
			out[i] = ' '
			i++
			for i < len(b) {
				if b[i] == q {
					// 连续两个引号是转义，继续留在字面量里
					if i+1 < len(b) && b[i+1] == q {
						out[i], out[i+1] = ' ', ' '
						i += 2
						continue
					}
					out[i] = ' '
					i++
					break
				}
				out[i] = ' '
				i++
			}
		case b[i] == '-' && i+1 < len(b) && b[i+1] == '-':
			for i < len(b) && b[i] != '\n' {
				out[i] = ' '
				i++
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for i < len(b) {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i += 2
					break
				}
				out[i] = ' '
				i++
			}
		default:
			i++
		}
	}
	return string(out)
}

// leadingWord 取第一个标识符，跳过开头的空白、括号与注释残留。
func leadingWord(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "(")
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			end++
			continue
		}
		break
	}
	return strings.ToLower(s[:end])
}

func quoteWord(w string) string {
	if w == "" {
		return "（空）"
	}
	return strings.ToUpper(w)
}

// SQLFingerprint 返回 SQL 的短指纹。
// 审计里存指纹 + 前若干字符的预览即可定位问题，不必把整段原文（可能含敏感常量）落库。
// 先归一化空白，避免同一句换个缩进就变成不同指纹。
func SQLFingerprint(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:8])
}
