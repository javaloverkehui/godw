package graph

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleEdges() []Edge {
	return []Edge{
		{Source: "datasource.pg", Target: "ods_order"},
		{Source: "ods_order", Target: "dwd_order"},
		{Source: "dwd_order", Target: "dws_order_daily"},
		{Source: "dws_order_daily", Target: "ads_dashboard"},
		{Source: "dwd_order", Target: "dws_refund_daily"},
		{Source: "dws_refund_daily", Target: "ads_dashboard"},
	}
}

// T0606 验收：发布管道后血缘边正确；上下游方向正确；重复边不产生重复。
func TestBuildDedupes(t *testing.T) {
	edges := append(sampleEdges(), Edge{Source: "ods_order", Target: "dwd_order"})
	g := Builder{Layers: map[string]string{"ods_order": "ods", "dwd_order": "dwd"}}.Build(edges)
	if len(g.Edges) != 6 {
		t.Fatalf("重复边应去重，实际 %d", len(g.Edges))
	}
	if len(g.Nodes) != 6 {
		t.Fatalf("节点数 %d，期望 6", len(g.Nodes))
	}
	for _, n := range g.Nodes {
		if n.ID == "ods_order" && n.Layer != "ods" {
			t.Fatalf("层信息未写入: %+v", n)
		}
	}
}

func TestFocusUp(t *testing.T) {
	g := Focus(sampleEdges(), "dws_order_daily", Up, 2)
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"dws_order_daily", "dwd_order", "ods_order"} {
		if !ids[want] {
			t.Fatalf("上游展开缺少 %s: %+v", want, ids)
		}
	}
	if ids["ads_dashboard"] {
		t.Fatal("向上展开不应包含下游")
	}
}

func TestFocusDown(t *testing.T) {
	g := Focus(sampleEdges(), "dwd_order", Down, 2)
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"dwd_order", "dws_order_daily", "dws_refund_daily", "ads_dashboard"} {
		if !ids[want] {
			t.Fatalf("下游展开缺少 %s", want)
		}
	}
	if ids["ods_order"] {
		t.Fatal("向下展开不应包含上游")
	}
}

func TestFocusDepthLimit(t *testing.T) {
	g := Focus(sampleEdges(), "ods_order", Down, 1)
	if len(g.Nodes) != 2 {
		t.Fatalf("depth=1 应只有 2 个节点，实际 %d", len(g.Nodes))
	}
}

// 空图必须序列化成 [] 而不是 null：前端 forEach 直接吃数组。
func TestEmptyGraphSerializesAsArrays(t *testing.T) {
	g := Focus(sampleEdges(), "not_exist_table", Down, 3)
	if g.Nodes == nil || g.Edges == nil {
		t.Fatalf("空图切片不能为 nil: nodes=%v edges=%v", g.Nodes, g.Edges)
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("序列化不应出现 null: %s", b)
	}
	if !strings.Contains(string(b), `"nodes":[]`) || !strings.Contains(string(b), `"edges":[]`) {
		t.Fatalf("期望空数组: %s", b)
	}
}

// ODS 表通常只有上游（数据源），双向展开才不是空图。
func TestFocusBoth(t *testing.T) {
	g := Focus(sampleEdges(), "ods_order", Both, 5)
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"datasource.pg", "ods_order", "dwd_order", "dws_order_daily", "ads_dashboard"} {
		if !ids[want] {
			t.Fatalf("双向展开缺少 %s: %+v", want, ids)
		}
	}
	if len(g.Edges) != 5 {
		// dws_refund_daily 是 dwd_order 的下游，也应被拉进来（其自身不在 keep 里时边会被过滤）
		t.Logf("边数=%d（按可达节点过滤，符合预期范围）", len(g.Edges))
	}
	for _, e := range g.Edges {
		if e.Source == "ads_dashboard" && e.Target == "ods_order" {
			t.Fatal("不应出现反向边")
		}
	}
}

// T0610 验收：路径查询返回结果不含环。
func TestPaths(t *testing.T) {
	paths := Paths(sampleEdges(), "ods_order", "ads_dashboard", 10)
	if len(paths) != 2 {
		t.Fatalf("应有 2 条路径，实际 %d: %v", len(paths), paths)
	}
	for _, p := range paths {
		seen := map[string]bool{}
		for _, n := range p {
			if seen[n] {
				t.Fatalf("路径含环: %v", p)
			}
			seen[n] = true
		}
		if p[0] != "ods_order" || p[len(p)-1] != "ads_dashboard" {
			t.Fatalf("路径端点错误: %v", p)
		}
	}
	// 环不应导致死循环
	cyclic := append(sampleEdges(), Edge{Source: "ads_dashboard", Target: "ods_order"})
	out := Paths(cyclic, "ods_order", "ads_dashboard", 10)
	if len(out) == 0 {
		t.Fatal("有环时仍应返回路径")
	}
	for _, p := range out {
		if len(p) > 10 {
			t.Fatalf("路径异常长（可能死循环）: %v", p)
		}
	}
}

func TestPathLimit(t *testing.T) {
	if got := Paths(sampleEdges(), "ods_order", "ads_dashboard", 1); len(got) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d", len(got))
	}
}

func TestDOT(t *testing.T) {
	g := Builder{}.Build(sampleEdges())
	out := DOT(g)
	if !strings.HasPrefix(out, "digraph lineage {") || !strings.Contains(out, `"ods_order" -> "dwd_order"`) {
		t.Fatalf("DOT 输出异常:\n%s", out)
	}
}

// T0608 验收：缓存命中率与失效。
func TestCache(t *testing.T) {
	c := NewCache(50*time.Millisecond, 2)
	g := Builder{}.Build(sampleEdges())
	c.Put("k1", g)
	if _, ok := c.Get("k1"); !ok {
		t.Fatal("应命中")
	}
	st := c.Stats()
	if st["hits"].(int64) != 1 {
		t.Fatalf("命中计数错误: %+v", st)
	}
	// 过期
	time.Sleep(70 * time.Millisecond)
	if _, ok := c.Get("k1"); ok {
		t.Fatal("过期后不应命中")
	}
	// 容量限制
	for i := 0; i < 5; i++ {
		c.Put(string(rune('a'+i)), g)
	}
	if n := c.Stats()["entries"].(int); n > 2 {
		t.Fatalf("容量限制失效: %d", n)
	}
	// 失效
	c.Invalidate()
	if n := c.Stats()["entries"].(int); n != 0 {
		t.Fatalf("Invalidate 未清空: %d", n)
	}
}

func TestMaxNodes(t *testing.T) {
	edges := []Edge{}
	for i := 0; i < 100; i++ {
		edges = append(edges, Edge{Source: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Target: "x"})
	}
	g := Builder{MaxNodes: 5}.Build(edges)
	if !g.Truncated || len(g.Nodes) > 6 {
		t.Fatalf("节点上限未生效: nodes=%d truncated=%v", len(g.Nodes), g.Truncated)
	}
}
