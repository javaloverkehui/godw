package graph_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/lineage/graph"
)

// 构图缺陷面：
//   - 节点/边必须去重，否则前端渲染重复元素；
//   - 空图必须序列化成 [] 而不是 null（前端 forEach 会直接崩）；
//   - confidence 缺省值要补成 1，否则「置信度」展示为 0 会被误读为不可信；
//   - 节点上限触发后必须标记 Truncated，而不是静默丢数据。
func TestBuilderBuild(t *testing.T) {
	edges := []graph.Edge{
		{Source: "dwd_order", Target: "dws_sales"},
		{Source: "dwd_order", Target: "dws_sales"}, // 重复边
		{Source: "datasource.pg_main", Target: "ods_order"},
		{Source: "ods.order", Target: "dwd.order"},
	}
	g := graph.Builder{Layers: map[string]string{"dwd_order": "dwd"}}.Build(edges)
	if g.Nodes == nil || g.Edges == nil {
		t.Fatal("Nodes/Edges 必须是非 nil 切片")
	}
	if len(g.Edges) != 3 {
		t.Fatalf("重复边未去重: %+v", g.Edges)
	}
	if g.Edges[0].Confidence != 1 {
		t.Fatalf("confidence 缺省应补 1，实际 %v", g.Edges[0].Confidence)
	}
	byID := map[string]graph.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if len(byID) != 6 {
		t.Fatalf("节点数错误: %+v", g.Nodes)
	}
	if n := byID["datasource.pg_main"]; n.Type != "datasource" || n.Name != "pg_main" {
		t.Fatalf("数据源节点错误: %+v", n)
	}
	if n := byID["ods.order"]; n.Type != "table" {
		t.Fatalf("带点节点应为表: %+v", n)
	}
	if n := byID["dwd_order"]; n.Type != "model" || n.Layer != "dwd" {
		t.Fatalf("模型节点错误: %+v", n)
	}
	// 节点按 ID 排序，保证快照稳定。
	for i := 1; i < len(g.Nodes); i++ {
		if g.Nodes[i-1].ID > g.Nodes[i].ID {
			t.Fatalf("节点未按 ID 排序: %+v", g.Nodes)
		}
	}
}

// 节点上限：到达上限后必须停止并标记 Truncated。
func TestBuilderMaxNodes(t *testing.T) {
	edges := []graph.Edge{
		{Source: "a", Target: "b"},
		{Source: "b", Target: "c"},
	}
	// 上限检查发生在处理下一条边之前：第一条边仍会建出两个节点，
	// 第二条边发现已满则停止并标记截断。
	g := graph.Builder{MaxNodes: 2}.Build(edges)
	if !g.Truncated {
		t.Fatal("超过节点上限必须标记 Truncated")
	}
	if len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("超限后结果错误: nodes=%+v edges=%+v", g.Nodes, g.Edges)
	}
	// MaxNodes<=0 时使用默认值 500，不应误判截断。
	if g2 := (graph.Builder{}).Build(edges); g2.Truncated {
		t.Fatal("默认上限不应触发截断")
	}
}

// Focus 的缺陷面：方向搞反会让「改这张表影响谁」查成「这张表从哪来」；
// 双向展开是单表视角的默认语义，必须同时走上下游。
func TestFocus(t *testing.T) {
	edges := []graph.Edge{
		{Source: "a", Target: "b"},
		{Source: "b", Target: "c"},
		{Source: "d", Target: "e"},
	}
	nodeIDs := func(g graph.Graph) []string {
		out := make([]string, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			out = append(out, n.ID)
		}
		return out
	}
	cases := []struct {
		name  string
		start string
		dir   graph.Direction
		depth int
		want  string
	}{
		{"下游", "a", graph.Down, 2, "a,b,c"},
		{"上游", "c", graph.Up, 2, "a,b,c"},
		{"双向", "b", graph.Both, 1, "a,b,c"},
		{"缺省方向即双向", "b", "", 1, "a,b,c"},
		{"非法深度回退 3", "a", graph.Down, 99, "a,b,c"},
		{"深度为负回退 3", "a", graph.Down, -1, "a,b,c"},
		{"孤立节点", "x", graph.Both, 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.Focus(edges, tc.start, tc.dir, tc.depth)
			if got := strings.Join(nodeIDs(g), ","); got != tc.want {
				t.Fatalf("节点 = %q，期望 %q", got, tc.want)
			}
			if g.Nodes == nil || g.Edges == nil {
				t.Fatal("结果切片必须非 nil")
			}
		})
	}
}

// 环上的重复节点只保留一次：否则带宽展开会无限增长。
func TestFocusCycle(t *testing.T) {
	edges := []graph.Edge{{Source: "a", Target: "b"}, {Source: "b", Target: "a"}}
	g := graph.Focus(edges, "a", graph.Both, 3)
	if len(g.Nodes) != 2 || len(g.Edges) != 2 {
		t.Fatalf("环上展开错误: nodes=%+v edges=%+v", g.Nodes, g.Edges)
	}
}

// 路径查询：多路径、限流、去环、无路径。
func TestPaths(t *testing.T) {
	edges := []graph.Edge{
		{Source: "a", Target: "b"},
		{Source: "a", Target: "c"},
		{Source: "b", Target: "d"},
		{Source: "c", Target: "d"},
		{Source: "d", Target: "a"}, // 环
	}
	got := graph.Paths(edges, "a", "d", 0)
	if len(got) != 2 {
		t.Fatalf("应返回 2 条路径，实际 %v", got)
	}
	for _, p := range got {
		if strings.Join(p, ">") != "a>b>d" && strings.Join(p, ">") != "a>c>d" {
			t.Fatalf("路径错误: %v", p)
		}
	}
	if got := graph.Paths(edges, "a", "d", 1); len(got) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %v", got)
	}
	if got := graph.Paths(edges, "a", "d", 99); len(got) != 2 {
		t.Fatalf("limit 越界应回退默认值: %v", got)
	}
	if got := graph.Paths(edges, "x", "y", 5); got != nil {
		t.Fatalf("无路径应返回 nil，实际 %v", got)
	}
	// 环上不能死循环：a→a 直接视为一条长度 1 的路径。
	if got := graph.Paths(edges, "a", "a", 5); len(got) != 1 {
		t.Fatalf("起点即终点应返回单点路径: %v", got)
	}
}

// DOT 导出：层级要有颜色区分，否则贴到工单里看不出分层。
func TestDOT(t *testing.T) {
	g := graph.Graph{
		Nodes: []graph.Node{
			{ID: "o", Name: "O", Layer: "ods"},
			{ID: "d", Name: "D", Layer: "dwd"},
			{ID: "s", Name: "S", Layer: "dws"},
			{ID: "a", Name: "A", Layer: "ads"},
			{ID: "u", Name: "U"},
		},
		Edges: []graph.Edge{{Source: "o", Target: "d"}},
	}
	out := graph.DOT(g)
	for _, want := range []string{
		"digraph lineage", "rankdir=LR", "#e8f0fe", "#e6f4ea", "#fff4e5", "#fce8e6",
		`"o" -> "d"`, `"o" [label=`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("DOT 输出缺少 %q:\n%s", want, out)
		}
	}
}

// 缓存缺陷面：过期条目必须删除而不是继续返回；
// 超过容量要淘汰最旧的键；Invalidate 后必须彻底失效；
// 命中率统计错误会让运维误判缓存是否生效。
func TestCache(t *testing.T) {
	c := graph.NewCache(time.Minute, 2)
	g := graph.Graph{Nodes: []graph.Node{{ID: "a"}}}
	c.Put("k1", g)
	if v, ok := c.Get("k1"); !ok || len(v.Nodes) != 1 {
		t.Fatalf("应命中缓存: %+v", v)
	}
	// 覆盖已存在的键：不重复追加顺序表，避免容量被重复计数。
	c.Put("k1", g)
	c.Put("k2", g)
	c.Put("k3", g) // 淘汰 k1
	if _, ok := c.Get("k1"); ok {
		t.Fatal("超出容量后最旧键应被淘汰")
	}
	c.Get("missing")
	st := c.Stats()
	if st["hits"].(int64) != 1 || st["misses"].(int64) != 2 {
		t.Fatalf("命中统计错误: %+v", st)
	}
	if st["entries"].(int) != 2 || st["capacity"].(int) != 2 {
		t.Fatalf("容量统计错误: %+v", st)
	}
	if st["hitRate"].(float64) <= 0 {
		t.Fatalf("命中率计算错误: %+v", st)
	}
	c.Invalidate()
	if _, ok := c.Get("k2"); ok {
		t.Fatal("Invalidate 后不应命中")
	}

	// 默认参数：ttl<=0 与 max<=0 都要落到安全默认值。
	def := graph.NewCache(0, 0)
	if def.Stats()["capacity"].(int) != 512 {
		t.Fatalf("默认容量错误: %+v", def.Stats())
	}
	if st := def.Stats(); st["hitRate"].(float64) != 0 {
		t.Fatalf("无请求时命中率应为 0: %+v", st)
	}

	// 过期条目：Get 时必须删除且计 miss，随后再 Get 仍是 miss。
	exp := graph.NewCache(time.Nanosecond, 10)
	exp.Put("e", g)
	time.Sleep(time.Millisecond)
	if _, ok := exp.Get("e"); ok {
		t.Fatal("过期条目不应命中")
	}
	if _, ok := exp.Get("e"); ok {
		t.Fatal("过期条目应被删除")
	}
}

// 列级缓存与表级共用实现，必须同样可用。
func TestColumnCache(t *testing.T) {
	c := graph.NewColumnCache(time.Minute, 4)
	cg := graph.ColumnGraph{Focus: graph.ColumnRef{Table: "t", Column: "c"}}
	c.Put("k", cg)
	v, ok := c.Get("k")
	if !ok || v.Focus.Column != "c" {
		t.Fatalf("列级缓存错误: %+v", v)
	}
	c.Invalidate()
	if _, ok := c.Get("k"); ok {
		t.Fatal("列级缓存 Invalidate 失败")
	}
}

func colLink(upT, upC, downT, downC string) graph.ColumnLink {
	return graph.ColumnLink{
		Upstream:   graph.ColumnRef{Table: upT, Column: upC},
		Downstream: graph.ColumnRef{Table: downT, Column: downC},
	}
}

// ColumnRef.ID：无表名时只能返回列名，否则会拼出 ".col" 这种对不上的 ID。
func TestColumnRefID(t *testing.T) {
	if got := (graph.ColumnRef{Column: "c"}).ID(); got != "c" {
		t.Fatalf("无表名 ID = %q", got)
	}
	if got := (graph.ColumnRef{Table: "t", Column: "c"}).ID(); got != "t.c" {
		t.Fatalf("ID = %q", got)
	}
}

// 列级展开缺陷面：
//   - 自引用必须丢弃（否则图上出现自环）；
//   - 方向必须正确；
//   - 节点按表/列排序，保证前端泳道稳定。
func TestColumnFocus(t *testing.T) {
	links := []graph.ColumnLink{
		colLink("a", "x", "b", "y"),
		colLink("b", "y", "c", "z"),
		colLink("b", "y", "b", "y"), // 自引用
		colLink("d", "w", "e", "v"), // 无关子图
	}
	focus := graph.ColumnRef{Table: "b", Column: "y"}

	both := graph.ColumnFocus(links, focus, graph.Both, 2)
	if both.UpstreamCount != 1 || both.DownstreamCount != 1 {
		t.Fatalf("双向计数错误: up=%d down=%d", both.UpstreamCount, both.DownstreamCount)
	}
	if len(both.Nodes) != 3 || len(both.Edges) != 2 {
		t.Fatalf("双向子图错误: %+v", both)
	}
	if both.Truncated {
		t.Fatal("小图不应截断")
	}
	for i := 1; i < len(both.Nodes); i++ {
		a, b := both.Nodes[i-1], both.Nodes[i]
		if a.Table > b.Table || (a.Table == b.Table && a.Column > b.Column) {
			t.Fatalf("节点未排序: %+v", both.Nodes)
		}
	}

	up := graph.ColumnFocus(links, focus, graph.Up, 1)
	if up.UpstreamCount != 1 || up.DownstreamCount != 0 || len(up.Nodes) != 2 {
		t.Fatalf("上游子图错误: %+v", up)
	}
	down := graph.ColumnFocus(links, focus, graph.Down, 1)
	if down.DownstreamCount != 1 || down.UpstreamCount != 0 || len(down.Nodes) != 2 {
		t.Fatalf("下游子图错误: %+v", down)
	}
	// 缺省方向按双向处理；非法深度回退默认值。
	def := graph.ColumnFocus(links, focus, "", 99)
	if def.UpstreamCount != 1 || def.DownstreamCount != 1 {
		t.Fatalf("缺省方向错误: %+v", def)
	}
	if empty := graph.ColumnFocus(nil, focus, graph.Both, 2); empty.Nodes == nil || empty.Edges == nil {
		t.Fatal("空图切片必须非 nil")
	}
	// 环上已访问的列不能重复计数。
	cyc := []graph.ColumnLink{colLink("a", "x", "b", "y"), colLink("b", "y", "a", "x")}
	cg := graph.ColumnFocus(cyc, graph.ColumnRef{Table: "a", Column: "x"}, graph.Both, 3)
	// 双向模式下先走上游，环上的下游节点已被计入上游集合，
	// 下游计数因此为 0——这里锁定现状：不会重复计数，也不会死循环。
	if cg.UpstreamCount != 1 || cg.DownstreamCount != 0 {
		t.Fatalf("环上展开错误: %+v", cg)
	}
	if len(cg.Nodes) != 2 || len(cg.Edges) != 2 {
		t.Fatalf("环上子图错误: %+v", cg)
	}
}

// 列级图节点上限 300：超限必须停止遍历并标记 Truncated，
// 否则一张宽表就能把浏览器画崩。
func TestColumnFocusTruncated(t *testing.T) {
	var links []graph.ColumnLink
	for i := 0; i < 400; i++ {
		links = append(links, colLink("t0", "c", fmt.Sprintf("t%d", i+1), "c"))
	}
	g := graph.ColumnFocus(links, graph.ColumnRef{Table: "t0", Column: "c"}, graph.Down, 2)
	if !g.Truncated {
		t.Fatalf("超过 300 个节点必须标记 Truncated，实际 %d", len(g.Nodes))
	}
	if len(g.Nodes) != 300 {
		t.Fatalf("应保留 300 个节点，实际 %d", len(g.Nodes))
	}
	if g.DownstreamCount != 299 {
		t.Fatalf("下游计数应在截断处停止，实际 %d", g.DownstreamCount)
	}
}
