package lineage

import (
	"fmt"
	"strings"
)

// 本文件实现一个「够用就好」的 SQL 分析器：只做列级血缘需要的两件事——
// 找出这条语句读了哪些表的哪些列，以及每个输出列由哪些输入列算出来。
//
// 刻意不做的事：不做语义校验、不支持窗口函数的精细归属、不展开 `*`。
// 目标是给出可信的「候选血缘」，让影响分析有据可依；不确定时宁可漏报也不误报。

// ColRef 是「表.列」引用。表为空表示无法确定归属（多表同名列、或来自表达式常量）。
type ColRef struct {
	Table  string
	Column string
}

// Output 是一个输出列及其依赖。
type Output struct {
	Name   string
	Inputs []ColRef
	// Literal 为真表示该列是常量或函数结果，没有上游列。
	Literal bool
}

// Query 是分析结果。
type Query struct {
	// Sources 是本层直接读取的物理表（已解析 schema，别名不保留）。
	Sources []ColRef
	// Aliases 记录别名到真实表的映射，便于排查。
	Aliases map[string]string
	// virtual 记录 CTE/子查询别名到其输出列，用于把别名列还原成基表列。
	virtual map[string][]Output
	Outputs []Output
	// HasStar 为真表示存在 `*` 或 `t.*`，此时列级血缘不完整。
	HasStar bool
}

type token struct {
	text string
	kind byte // 'i' 标识符, 's' 字符串, 'n' 数字, 'p' 标点
}

// Analyze 分析一条 SELECT 语句。解析失败时返回错误，调用方应退化为表级血缘。
func Analyze(sql string) (*Query, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	return p.query()
}

// ---------- 词法 ----------

func lex(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("注释未闭合")
			}
			i += j + 4
		case c == '\'':
			j := i + 1
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("字符串未闭合")
			}
			out = append(out, token{text: s[i : j+1], kind: 's'})
			i = j + 1
		case c == '"' || c == '`':
			q := c
			j := i + 1
			for j < len(s) && s[j] != q {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("引号未闭合")
			}
			out = append(out, token{text: s[i+1 : j], kind: 'i'})
			i = j + 1
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			out = append(out, token{text: s[i:j], kind: 'i'})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			out = append(out, token{text: s[i:j], kind: 'n'})
			i = j
		default:
			out = append(out, token{text: string(c), kind: 'p'})
			i++
		}
	}
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c == '$'
}

// ---------- 语法（只覆盖 SELECT 子集） ----------

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token {
	if p.pos >= len(p.toks) {
		return token{kind: 'p', text: ""}
	}
	return p.toks[p.pos]
}

func (p *parser) peekAt(n int) token {
	if p.pos+n >= len(p.toks) {
		return token{kind: 'p', text: ""}
	}
	return p.toks[p.pos+n]
}

func (p *parser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) accept(word string) bool {
	if strings.EqualFold(p.peek().text, word) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(word string) error {
	if !p.accept(word) {
		return fmt.Errorf("期望 %s，实际 %q", word, p.peek().text)
	}
	return nil
}

// skipToClose 跳过剩余子句（WHERE/GROUP BY/...）直到匹配的右括号，p.pos 停在 ")" 上。
// query() 只关心 SELECT 投影与 FROM，其它子句一律不解析，但必须正确越过。
func (p *parser) skipToClose() {
	depth := 0
	for p.pos < len(p.toks) {
		t := p.toks[p.pos]
		if t.kind == 'p' {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth == 0 {
					return
				}
				depth--
			}
		}
		p.pos++
	}
}

// cte 保存 WITH 定义，展开时用。
type cte struct {
	name    string
	sources []ColRef
	outputs []Output
	hasStar bool
}

func (p *parser) query() (*Query, error) {
	q := &Query{Aliases: map[string]string{}, virtual: map[string][]Output{}}

	// WITH ... AS ( ... ) [, ...]
	ctes := map[string]*cte{}
	if p.accept("WITH") {
		p.accept("RECURSIVE")
		for {
			name := p.next()
			if name.kind != 'i' {
				return nil, fmt.Errorf("CTE 名称非法")
			}
			// 可选的列别名列表
			var aliasCols []string
			if strings.EqualFold(p.peek().text, "(") {
				p.pos++
				for p.peek().text != ")" && p.peek().kind != 'p' || p.peek().text != ")" {
					if p.peek().kind == 'p' && p.peek().text == ")" {
						break
					}
					aliasCols = append(aliasCols, p.next().text)
					if p.peek().text == "," {
						p.pos++
					}
				}
				p.pos++ // )
			}
			if err := p.expect("AS"); err != nil {
				return nil, err
			}
			if err := p.expect("("); err != nil {
				return nil, err
			}
			inner, err := p.query()
			if err != nil {
				return nil, err
			}
			p.skipToClose()
			p.pos++ // )
			c := &cte{name: strings.ToLower(name.text), sources: inner.Sources, outputs: inner.Outputs, hasStar: inner.HasStar}
			if len(aliasCols) > 0 {
				for i := range c.outputs {
					if i < len(aliasCols) {
						c.outputs[i].Name = aliasCols[i]
					}
				}
			}
			ctes[c.name] = c
			if !p.accept(",") {
				break
			}
		}
	}

	// 允许括号包裹的查询
	for p.peek().text == "(" {
		p.pos++
		if p.accept("SELECT") {
			p.pos--
			break
		}
	}

	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	p.accept("DISTINCT")
	p.accept("ALL")

	// 投影
	for {
		out, star, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		if star {
			q.HasStar = true
		} else {
			q.Outputs = append(q.Outputs, out)
		}
		if !p.accept(",") {
			break
		}
	}

	// FROM
	if p.accept("FROM") {
		if err := p.fromList(q, ctes); err != nil {
			return nil, err
		}
	}

	// 把未限定表的列引用尽量归属到唯一来源表
	resolve(q)
	return q, nil
}

// selectItem 解析一个投影项，返回输出列与是否为 *。
func (p *parser) selectItem() (Output, bool, error) {
	start := p.pos
	exprEnd := p.pos
	depth := 0
	alias := ""
	// 先扫到顶层逗号或 FROM
	for p.pos < len(p.toks) {
		t := p.peek()
		if t.kind == 'p' {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth == 0 {
					goto done
				}
				depth--
			case ",":
				if depth == 0 {
					goto done
				}
			}
		}
		if depth == 0 && t.kind == 'i' && strings.EqualFold(t.text, "FROM") {
			goto done
		}
		// 记录 AS 别名
		if depth == 0 && t.kind == 'i' && strings.EqualFold(t.text, "AS") && p.peekAt(1).kind == 'i' {
			exprEnd = p.pos
			alias = p.peekAt(1).text
			p.pos += 2
			goto done
		}
		p.pos++
		exprEnd = p.pos
	}
done:
	expr := p.toks[start:minInt(exprEnd, len(p.toks))]

	// 去掉尾部隐式别名：expr 末尾连续两个标识符且不是函数调用
	if alias == "" && len(expr) >= 2 {
		last, prev := expr[len(expr)-1], expr[len(expr)-2]
		if last.kind == 'i' && prev.kind == 'i' && !strings.EqualFold(prev.text, "END") {
			alias = last.text
			expr = expr[:len(expr)-1]
		}
	}
	// 去掉尾部逗号
	for len(expr) > 0 && expr[len(expr)-1].text == "," {
		expr = expr[:len(expr)-1]
	}
	if len(expr) == 0 {
		return Output{}, false, fmt.Errorf("空投影项")
	}

	// 星号
	if expr[len(expr)-1].text == "*" {
		return Output{}, true, nil
	}

	name := alias
	if name == "" {
		// 取最后一个标识符作为列名
		for i := len(expr) - 1; i >= 0; i-- {
			if expr[i].kind == 'i' {
				name = expr[i].text
				break
			}
		}
		if name == "" {
			name = fmt.Sprintf("col_%d", p.pos)
		}
	}
	// 带限定名的裸列：t.c
	if len(expr) == 3 && expr[0].kind == 'i' && expr[1].text == "." && expr[2].kind == 'i' && alias == "" {
		name = expr[2].text
	}

	return Output{Name: strings.ToLower(name), Inputs: refsIn(expr)}, false, nil
}

// refsIn 从表达式 token 中抽取列引用。函数名、关键字不算列。
func refsIn(expr []token) []ColRef {
	var out []ColRef
	for i := 0; i < len(expr); i++ {
		t := expr[i]
		if t.kind != 'i' {
			continue
		}
		up := strings.ToUpper(t.text)
		if keywords[up] {
			continue
		}
		// 函数调用
		if i+1 < len(expr) && expr[i+1].text == "(" {
			continue
		}
		// 限定名 a.b
		if i+2 < len(expr) && expr[i+1].text == "." && expr[i+2].kind == 'i' {
			out = append(out, ColRef{Table: strings.ToLower(t.text), Column: strings.ToLower(expr[i+2].text)})
			i += 2
			continue
		}
		out = append(out, ColRef{Column: strings.ToLower(t.text)})
	}
	return dedup(out)
}

var keywords = map[string]bool{
	"AS": true, "AND": true, "OR": true, "NOT": true, "NULL": true, "IS": true, "IN": true,
	"CASE": true, "WHEN": true, "THEN": true, "ELSE": true, "END": true, "CAST": true,
	"DISTINCT": true, "ALL": true, "INTERVAL": true, "TRUE": true, "FALSE": true,
	"OVER": true, "PARTITION": true, "BY": true, "ORDER": true, "GROUP": true,
	"ASC": true, "DESC": true, "FILTER": true, "WHERE": true, "COALESCE": true,
	"EXTRACT": true, "FROM": true, "AT": true, "TIME": true, "ZONE": true,
	"DATE": true, "TIMESTAMP": true, "NUMERIC": true, "DECIMAL": true,
}

// fromList 解析 FROM 子句，填充 Sources 与 Aliases，也展开子查询与 CTE。
func (p *parser) fromList(q *Query, ctes map[string]*cte) error {
	for {
		if p.peek().text == "(" {
			// 子查询
			p.pos++
			inner, err := p.query()
			if err != nil {
				return err
			}
			p.skipToClose()
			p.pos++ // )
			alias := p.ident()
			if alias == "" {
				alias = "_sq"
			}
			q.virtual[alias] = inner.Outputs
			q.Sources = append(q.Sources, inner.Sources...)
			if inner.HasStar {
				q.HasStar = true
			}
		} else {
			name := p.next()
			if name.kind != 'i' {
				return fmt.Errorf("FROM 表名非法: %q", name.text)
			}
			lname := strings.ToLower(name.text)
			// 可能是 schema.table
			full := lname
			if p.peek().text == "." {
				p.pos++
				second := p.next()
				full = lname + "." + strings.ToLower(second.text)
			}
			alias := p.ident()
			if alias == "" {
				alias = strings.ToLower(lastPart(full))
			}
			if c, ok := ctes[lname]; ok {
				// CTE：来源展开为它的基表，列通过 virtual 回溯，不把 CTE 名当表名。
				q.Sources = append(q.Sources, c.sources...)
				q.virtual[alias] = c.outputs
				if c.hasStar {
					q.HasStar = true
				}
			} else {
				q.Sources = append(q.Sources, ColRef{Table: full})
				q.Aliases[alias] = full
			}
		}
		if p.accept("JOIN") || p.accept("INNER") || p.accept("LEFT") || p.accept("RIGHT") || p.accept("FULL") {
			// 左/右连接后可能还有 OUTER
			p.accept("OUTER")
			p.accept("JOIN")
			continue
		}
		if p.accept("CROSS") {
			p.accept("JOIN")
			continue
		}
		break
	}
	return nil
}

func (p *parser) ident() string {
	t := p.peek()
	if t.kind != 'i' || keywords[strings.ToUpper(t.text)] {
		return ""
	}
	p.pos++
	return strings.ToLower(t.text)
}

// resolve 把列引用归属到基表：
//   - 别名是物理表 → 换成真实表名；
//   - 别名是 CTE/子查询 → 用其输出列的上游列替换（可递归）；
//   - 无限定名 → 单源直接归属；多源时按 CTE 输出唯一匹配。
//
// 无法归属的引用会被丢弃，宁可少一条边，也不要产出错误的血缘。
func resolve(q *Query) {
	var out []Output
	for _, o := range q.Outputs {
		var fixed []ColRef
		for _, ref := range o.Inputs {
			if ref.Table != "" {
				if real, ok := q.Aliases[ref.Table]; ok {
					fixed = append(fixed, ColRef{Table: real, Column: ref.Column})
					continue
				}
				if outs, ok := q.virtual[ref.Table]; ok {
					fixed = append(fixed, expandVirtual(outs, ref.Column, 0)...)
					continue
				}
				// 未知别名：可能是 schema 限定名被写成了别名，直接保留
				fixed = append(fixed, ref)
				continue
			}
			if len(q.Sources) == 1 {
				fixed = append(fixed, ColRef{Table: q.Sources[0].Table, Column: ref.Column})
				continue
			}
			// 多源：若恰好一个虚拟源含该列，则回溯到它
			var hit []ColRef
			for _, outs := range q.virtual {
				for _, vo := range outs {
					if vo.Name == ref.Column {
						hit = append(hit, vo.Inputs...)
					}
				}
			}
			if len(hit) > 0 {
				fixed = append(fixed, hit...)
			}
		}
		o.Inputs = dedup(fixed)
		if len(o.Inputs) == 0 {
			o.Literal = true
		}
		out = append(out, o)
	}
	q.Outputs = out
}

// expandVirtual 把「虚拟表.列」展开成基表列。depth 防止环形 CTE 无限递归。
func expandVirtual(outs []Output, col string, depth int) []ColRef {
	if depth > 8 {
		return nil
	}
	var res []ColRef
	for _, vo := range outs {
		if vo.Name != col {
			continue
		}
		for _, in := range vo.Inputs {
			res = append(res, in)
		}
	}
	return res
}

func dedup(in []ColRef) []ColRef {
	seen := map[ColRef]bool{}
	out := in[:0]
	for _, r := range in {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

func lastPart(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
