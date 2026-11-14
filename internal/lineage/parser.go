package lineage

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// T0605 SQL 解析器封装。
//
// 决策：不引入 pingcap/tidb/parser。理由是依赖体积（>30MB 源码、数十个包）
// 与收益不匹配——我们需要的是「表名 + 列引用」，不是完整 AST。
// 自研解析器（sqlparse.go）只做词法与浅层语法，对不认识的语法明确降级为
// 「只识别表级血缘」，而不是 panic 或给出错误的列级结论。
//
// 本文件提供对外稳定接口，未来若换成第三方 parser，只需替换实现。

// MaxSQLBytes 是解析上限。超过则只做表级识别，避免大 SQL 拖垮发布接口。
const MaxSQLBytes = 100 * 1024

// ParseResult 是解析输出。
type ParseResult struct {
	Query *Query
	// Degraded 表示因 SQL 过大或语法不支持而降级，只保证表级结果。
	Degraded bool
	// Reason 说明降级原因。
	Reason string
	// Elapsed 是解析耗时，用于性能观测。
	Elapsed time.Duration
}

// ErrSQLEmpty 表示空 SQL。
var ErrSQLEmpty = errors.New("lineage: SQL 为空")

// ParseSQL 是解析入口。对不支持语法返回结构化错误而不是 panic。
func ParseSQL(sqlText string) (res ParseResult, err error) {
	start := time.Now()
	defer func() {
		res.Elapsed = time.Since(start)
		if r := recover(); r != nil {
			// 解析器内部 bug 不应该让发布流程崩溃。
			err = fmt.Errorf("lineage: 解析器内部错误: %v", r)
			res = ParseResult{Degraded: true, Reason: "解析器异常，仅保留表级血缘", Elapsed: time.Since(start)}
		}
	}()

	if strings.TrimSpace(sqlText) == "" {
		return res, ErrSQLEmpty
	}
	if len(sqlText) > MaxSQLBytes {
		return ParseResult{
			Degraded: true,
			Reason:   fmt.Sprintf("SQL 超过 %d KB，降级为表级血缘", MaxSQLBytes/1024),
			Query:    &Query{Sources: roughSources(sqlText)},
		}, nil
	}

	q, perr := Analyze(sqlText)
	if perr != nil {
		// 语法不认识：返回表级兜底，不阻断发布。
		return ParseResult{
			Degraded: true,
			Reason:   "语法超出支持范围，降级为表级血缘: " + perr.Error(),
			Query:    &Query{Sources: roughSources(sqlText)},
		}, nil
	}
	return ParseResult{Query: q}, nil
}

// roughSources 用最粗的规则抓 FROM/JOIN 后的标识符，作为降级兜底。
// 宁可漏抓也不要抓错：抓错了会在图上产生假节点，比缺边更难排查。
func roughSources(sqlText string) []ColRef {
	var out []ColRef
	lower := strings.ToLower(sqlText)
	for _, kw := range []string{" from ", " join ", "\nfrom ", "\njoin "} {
		idx := 0
		for {
			i := strings.Index(lower[idx:], kw)
			if i < 0 {
				break
			}
			pos := idx + i + len(kw)
			rest := strings.TrimLeft(sqlText[pos:], " \t\n")
			tbl := readIdent(rest)
			if tbl != "" {
				out = append(out, ColRef{Table: tbl})
			}
			idx = pos
		}
	}
	return out
}

func readIdent(s string) string {
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '"' || c == '`' {
			end++
			continue
		}
		break
	}
	return strings.Trim(s[:end], "`\"")
}
