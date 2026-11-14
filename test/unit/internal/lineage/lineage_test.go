package lineage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/godw/godw/internal/lineage"
)

// 解析入口的缺陷面：空 SQL 必须报明确错误而不是返回空结果；超大 SQL
// 或无法识别的语法必须降级为表级血缘，绝不能 panic 或「假装解析成功」——
// 发布接口一旦 panic，整条发布链路就断了。
func TestParseSQLEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\r\n"} {
		if _, err := lineage.ParseSQL(in); !errors.Is(err, lineage.ErrSQLEmpty) {
			t.Fatalf("ParseSQL(%q) 应返回 ErrSQLEmpty，实际 %v", in, err)
		}
	}
}

// 超过上限的 SQL 只做表级兜底：此时绝不能给出列级结论，
// 否则会影响分析的准确度。
func TestParseSQLTooLarge(t *testing.T) {
	body := "SELECT x\nfrom nf\njoin nj\n from sp\n join jp\n from \"QT\"\n from (select 1)"
	sqlText := body + strings.Repeat(" ", lineage.MaxSQLBytes)
	res, err := lineage.ParseSQL(sqlText)
	if err != nil {
		t.Fatalf("超大 SQL 不应返回错误: %v", err)
	}
	if !res.Degraded {
		t.Fatal("超大 SQL 必须标记 Degraded")
	}
	if !strings.Contains(res.Reason, "降级") {
		t.Fatalf("降级原因缺失: %q", res.Reason)
	}
	if res.Query == nil {
		t.Fatal("降级时仍应返回表级来源")
	}
	got := map[string]bool{}
	for _, s := range res.Query.Sources {
		got[s.Table] = true
	}
	// 覆盖四种关键字写法（空格/换行 × from/join），并确认引号被剥掉、括号被忽略。
	for _, want := range []string{"sp", "jp", "nf", "nj", "QT"} {
		if !got[want] {
			t.Fatalf("降级结果缺少表 %s，实际 %v", want, res.Query.Sources)
		}
	}
}

// 无法解析的 SQL 必须降级且保留 FROM 后的表名——
// 这是影响分析的最低保障，「解析失败就当没血缘」会让删表无人预警。
func TestParseSQLDegradedOnParseError(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"非 SELECT 语句", "UPDATE t SET a = 1 FROM src_table WHERE id = 1", "src_table"},
		{"块注释未闭合", "SELECT a FROM t1 /* broken", "t1"},
		{"字符串未闭合", "SELECT a FROM t2 WHERE b = 'oops", "t2"},
		{"引号未闭合", "SELECT a FROM t3 WHERE b = \"oops", "t3"},
		{"反引号未闭合", "SELECT a FROM t4 WHERE b = `oops", "t4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := lineage.ParseSQL(tc.sql)
			if err != nil {
				t.Fatalf("解析失败应降级而不是报错: %v", err)
			}
			if !res.Degraded || res.Query == nil {
				t.Fatalf("应降级并返回表级结果: %+v", res)
			}
			if !strings.Contains(res.Reason, "语法超出支持范围") {
				t.Fatalf("降级原因应说明语法不支持: %q", res.Reason)
			}
			found := false
			for _, s := range res.Query.Sources {
				if s.Table == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("兜底来源应包含 %s，实际 %v", tc.want, res.Query.Sources)
			}
		})
	}
}

// 正常 SELECT：列名、别名、限定名与来源表都要正确，
// 否则下游看到的就是一张接错线的血缘图。
func TestParseSQLSimple(t *testing.T) {
	res, err := lineage.ParseSQL("SELECT a AS x, b, t.c FROM db.tbl t WHERE a = 1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Degraded {
		t.Fatalf("正常 SQL 不应降级: %s", res.Reason)
	}
	q := res.Query
	if len(q.Sources) != 1 || q.Sources[0].Table != "db.tbl" {
		t.Fatalf("来源表错误: %+v", q.Sources)
	}
	if q.Aliases["t"] != "db.tbl" {
		t.Fatalf("别名映射错误: %+v", q.Aliases)
	}
	want := map[string]string{"x": "db.tbl.a", "b": "db.tbl.b", "c": "db.tbl.c"}
	if len(q.Outputs) != len(want) {
		t.Fatalf("输出列数错误: %+v", q.Outputs)
	}
	for _, o := range q.Outputs {
		wantRef, ok := want[o.Name]
		if !ok {
			t.Fatalf("意外输出列 %q", o.Name)
		}
		if len(o.Inputs) != 1 || o.Inputs[0].Table+"."+o.Inputs[0].Column != wantRef {
			t.Fatalf("列 %s 的血缘错误: %+v，期望 %s", o.Name, o.Inputs, wantRef)
		}
		if o.Literal {
			t.Fatalf("列 %s 不应被判为常量", o.Name)
		}
	}
}

// CTE 必须展开到基表：把 CTE 名当物理表会在血缘图里留下假节点。
func TestParseSQLCTE(t *testing.T) {
	sqlText := "WITH base AS (SELECT id, amount FROM ods.t1) " +
		"SELECT b.amount AS total FROM base b JOIN dwd.t2 d ON b.id = d.id"
	q, err := lineage.Analyze(sqlText)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 2 || q.Sources[0].Table != "ods.t1" || q.Sources[1].Table != "dwd.t2" {
		t.Fatalf("CTE 来源展开错误: %+v", q.Sources)
	}
	if len(q.Outputs) != 1 {
		t.Fatalf("输出列数错误: %+v", q.Outputs)
	}
	o := q.Outputs[0]
	if o.Name != "total" || len(o.Inputs) != 1 ||
		o.Inputs[0].Table != "ods.t1" || o.Inputs[0].Column != "amount" {
		t.Fatalf("CTE 列级回溯错误: %+v", o)
	}
}

// 带列别名列表与多个 CTE 时，别名重命名与逗号分隔都要被正确识别；
// 漏掉第二个 CTE 会让下游整段血缘消失。
func TestParseSQLMultipleCTE(t *testing.T) {
	q, err := lineage.Analyze("WITH a (k, v) AS (SELECT id, amount FROM ods.t1), " +
		"b AS (SELECT y FROM ods.t2) SELECT y.y FROM a x JOIN b y ON x.k = y.y")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 2 {
		t.Fatalf("多 CTE 来源错误: %+v", q.Sources)
	}
	if len(q.Outputs) != 1 || q.Outputs[0].Name != "y" {
		t.Fatalf("输出错误: %+v", q.Outputs)
	}
	// CTE 别名列列表只改输出名，不改列归属：v 仍归属唯一来源表。
	q2, err := lineage.Analyze("WITH base (k, v) AS (SELECT id, amount FROM ods.t1) SELECT v FROM base")
	if err != nil {
		t.Fatal(err)
	}
	if len(q2.Outputs) != 1 || q2.Outputs[0].Inputs[0].Table != "ods.t1" {
		t.Fatalf("别名列回溯错误: %+v", q2.Outputs)
	}
}

// RECURSIVE 与 CTE 内的 * 都要保留：前者不是表名，后者意味着列级血缘不完整。
func TestParseSQLRecursiveCTEWithStar(t *testing.T) {
	q, err := lineage.Analyze("WITH RECURSIVE r AS (SELECT * FROM ods.t) SELECT a FROM r")
	if err != nil {
		t.Fatal(err)
	}
	if !q.HasStar {
		t.Fatal("CTE 内的 * 必须向上传播 HasStar")
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "ods.t" {
		t.Fatalf("来源错误: %+v", q.Sources)
	}
}

// 子查询（带/不带别名）也要展开到基表。
func TestParseSQLSubquery(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		wantTable string
	}{
		{"带别名", "SELECT x.a FROM (SELECT a FROM src.t) x", "src.t"},
		{"缺省别名", "SELECT a FROM (SELECT a FROM src.t)", "src.t"},
		{"嵌套括号", "SELECT a FROM (SELECT b FROM t WHERE (id IN (1, 2))) x", "t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := lineage.Analyze(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Sources) != 1 || q.Sources[0].Table != tc.wantTable {
				t.Fatalf("来源错误: %+v", q.Sources)
			}
			if len(q.Outputs) != 1 || q.Outputs[0].Literal || len(q.Outputs[0].Inputs) != 1 {
				t.Fatalf("列级回溯错误: %+v", q.Outputs)
			}
		})
	}
}

// UNION 只取第一段：平台不做集合级列对齐，宁可少报也不错配。
func TestParseSQLUnion(t *testing.T) {
	q, err := lineage.Analyze("SELECT a FROM t1 UNION ALL SELECT b FROM t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "t1" {
		t.Fatalf("UNION 应只保留首段来源: %+v", q.Sources)
	}
	if len(q.Outputs) != 1 || q.Outputs[0].Inputs[0].Table != "t1" {
		t.Fatalf("UNION 输出错误: %+v", q.Outputs)
	}
}

// 带引号标识符：引号只是语法包装，不能进入表名，否则图节点会对不上。
func TestParseSQLQuotedIdentifiers(t *testing.T) {
	q, err := lineage.Analyze("SELECT \"Amount\" AS \"Total Amt\" FROM \"S\".\"T\" t")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "s.t" {
		t.Fatalf("引号表名处理错误: %+v", q.Sources)
	}
	if len(q.Outputs) != 1 || q.Outputs[0].Name != "total amt" {
		t.Fatalf("引号别名处理错误: %+v", q.Outputs)
	}
	if q.Outputs[0].Inputs[0].Column != "amount" {
		t.Fatalf("列引用错误: %+v", q.Outputs[0].Inputs)
	}
}

// * 与 t.*：只能标记 HasStar，不能凭空造列。
func TestParseSQLStar(t *testing.T) {
	for _, sqlText := range []string{"SELECT * FROM t", "SELECT t.* FROM t"} {
		q, err := lineage.Analyze(sqlText)
		if err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
		if !q.HasStar {
			t.Fatalf("%s 应标记 HasStar", sqlText)
		}
		if len(q.Outputs) != 0 {
			t.Fatalf("%s 不应产生输出列: %+v", sqlText, q.Outputs)
		}
	}
}

// 各种 JOIN 写法都必须继续解析后续表；漏一种就少一条上游边。
func TestParseSQLJoins(t *testing.T) {
	cases := []string{
		// 序列化后的 FROM 必须带表别名，否则 JOIN 关键字会被当成上一张表的别名
		// （解析器只认「表 [别名] 连接词」这种形式），这里按真实写法覆盖。
		"SELECT a FROM t1 x JOIN t2 y ON x.a = y.a",
		"SELECT a FROM t1 x INNER JOIN t2 y ON x.a = y.a",
		"SELECT a FROM t1 x LEFT OUTER JOIN t2 y ON x.a = y.a",
		"SELECT a FROM t1 x RIGHT JOIN t2 y ON x.a = y.a",
		"SELECT a FROM t1 x FULL OUTER JOIN t2 y ON x.a = y.a",
		"SELECT a FROM t1 x CROSS JOIN t2 y JOIN t3 z ON y.a = z.a",
	}
	for _, sqlText := range cases {
		q, err := lineage.Analyze(sqlText)
		if err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
		if len(q.Sources) < 2 {
			t.Fatalf("%s 应识别多张来源表，实际 %+v", sqlText, q.Sources)
		}
		// 多源下的无限定列无法归属，必须丢弃并标记 Literal，而不是瞎猜一张表。
		if len(q.Outputs) != 1 || !q.Outputs[0].Literal {
			t.Fatalf("%s 无限定列应判为无法归属: %+v", sqlText, q.Outputs)
		}
	}
}

// 多源 + 子查询：无限定列恰好只在一个虚拟源里出现时，应回溯到它的基表列。
func TestParseSQLMultiSourceVirtualHit(t *testing.T) {
	q, err := lineage.Analyze("SELECT total FROM (SELECT amount AS total FROM src.s) x " +
		"JOIN b.t2 d ON x.total = d.total")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 2 {
		t.Fatalf("来源数错误: %+v", q.Sources)
	}
	o := q.Outputs[0]
	if o.Literal || len(o.Inputs) != 1 || o.Inputs[0].Table != "src.s" || o.Inputs[0].Column != "amount" {
		t.Fatalf("虚拟源回溯错误: %+v", o)
	}
}

// 未知限定名要原样保留：可能是 schema 限定被写成了别名，
// 直接丢弃会漏掉真实的列引用。
func TestParseSQLUnknownAliasKept(t *testing.T) {
	q, err := lineage.Analyze("SELECT z.a FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Outputs) != 1 || len(q.Outputs[0].Inputs) != 1 ||
		q.Outputs[0].Inputs[0].Table != "z" || q.Outputs[0].Inputs[0].Column != "a" {
		t.Fatalf("未知别名应保留: %+v", q.Outputs)
	}
}

// 表达式/函数/常量/CASE/重复列：
//   - 函数名与关键字不能被当成列（否则血缘里出现假的 "count" 列）；
//   - 同一列重复引用要合并；
//   - 纯常量列标记 Literal。
func TestParseSQLExpressions(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		wantName string
		wantCols []string
		literal  bool
	}{
		{"函数与别名", "SELECT COUNT(*) AS c FROM t", "c", nil, true},
		{"常量", "SELECT 1 FROM t", "col_2", nil, true},
		{"无 FROM", "SELECT a", "a", nil, true},
		{"AS 结尾", "SELECT a AS", "as", nil, true},
		{"CASE", "SELECT CASE WHEN a > 0 THEN b ELSE 0 END AS x FROM t", "x", []string{"a", "b"}, false},
		{"重复列合并", "SELECT a + a AS doubled FROM t", "doubled", []string{"a"}, false},
		{"限定裸列", "SELECT t.c FROM t", "c", []string{"c"}, false},
		{"隐式别名", "SELECT amount total FROM t", "total", []string{"amount"}, false},
		// END 之后的词不会被当成别名（避免把 CASE 尾词误认成别名），
		// 代价是它会被当作列引用保留；这是保守策略的已知取舍。
		{"紧邻 END 的隐式别名被拒绝", "SELECT CASE WHEN a THEN b END alias FROM t", "alias", []string{"a", "b", "alias"}, false},
		{"DISTINCT", "SELECT DISTINCT a FROM t", "a", []string{"a"}, false},
		{"ALL", "SELECT ALL a FROM t", "a", []string{"a"}, false},
		{"行注释", "SELECT a FROM t -- comment", "a", []string{"a"}, false},
		{"字符串内转义单引号", "SELECT a FROM t WHERE b = 'it''s'", "a", []string{"a"}, false},
		{"块注释", "SELECT a /* comment */ FROM t", "a", []string{"a"}, false},
		{"数字字面量", "SELECT 1.5 AS r FROM t", "r", nil, true},
		{"括号包裹查询", "((SELECT a FROM t))", "a", []string{"a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := lineage.Analyze(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Outputs) != 1 {
				t.Fatalf("输出列数错误: %+v", q.Outputs)
			}
			o := q.Outputs[0]
			if o.Name != tc.wantName {
				t.Fatalf("列名 = %q，期望 %q", o.Name, tc.wantName)
			}
			if o.Literal != tc.literal {
				t.Fatalf("Literal = %v，期望 %v（inputs=%+v）", o.Literal, tc.literal, o.Inputs)
			}
			var cols []string
			for _, in := range o.Inputs {
				cols = append(cols, in.Column)
			}
			if strings.Join(cols, ",") != strings.Join(tc.wantCols, ",") {
				t.Fatalf("输入列 = %v，期望 %v", cols, tc.wantCols)
			}
		})
	}
}

// 畸形 SQL 的每条错误路径都要有明确错误，绝不能静默返回半个结果。
func TestAnalyzeErrors(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"空投影项", "SELECT FROM t"},
		{"FROM 表名非法", "SELECT a FROM 1"},
		{"CTE 名称非法", "WITH 1 AS (SELECT a FROM t) SELECT a FROM t"},
		{"CTE 缺少 AS", "WITH c SELECT a FROM t"},
		{"CTE 缺少括号", "WITH c AS SELECT a FROM t"},
		{"CTE 内层错误", "WITH c AS (SELECT FROM t) SELECT a FROM c"},
		{"子查询错误向外传播", "SELECT a FROM (SELECT FROM t)"},
		{"JOIN 后表名非法", "SELECT a FROM t x LEFT OUTER JOIN 1"},
		{"未知语句", "INSERT INTO t VALUES (1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := lineage.Analyze(tc.sql); err == nil {
				t.Fatalf("畸形 SQL 应返回错误: %s", tc.sql)
			}
		})
	}
}

// 子查询里的 * 必须继续向上传播：否则外层以为列级血缘完整。
func TestParseSQLSubqueryStar(t *testing.T) {
	q, err := lineage.Analyze("SELECT a FROM (SELECT * FROM t) x")
	if err != nil {
		t.Fatal(err)
	}
	if !q.HasStar {
		t.Fatal("子查询内的 * 必须传播 HasStar")
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "t" {
		t.Fatalf("来源错误: %+v", q.Sources)
	}
}

// 顶层右括号会提前结束投影项：解析器必须容忍并继续，而不是 panic。
func TestAnalyzeStrayParen(t *testing.T) {
	q, err := lineage.Analyze("SELECT a) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Outputs) != 1 || q.Outputs[0].Name != "a" {
		t.Fatalf("输出错误: %+v", q.Outputs)
	}
}
