package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// T0606 / T0608 血缘图。
//
// 存储用邻接表（PG 里就是 lineage_edge），查询用递归展开。
// 图规模不大（万级边），但访问非常集中（90% 的请求集中在几十个核心表），
// 因此在内存里加一层 LRU，命中率远高于盲目上 Redis。

// Node 是图上的一个节点。
type Node struct {
	ID    string         `json:"id"`
	Type  string         `json:"type"` // model | dataset | table | datasource
	Layer string         `json:"layer,omitempty"`
	Name  string         `json:"name"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// Edge 是一条边。
type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	// Columns 是列级映射，表级血缘时为空。
	Columns []ColumnPair `json:"columns,omitempty"`
	// Confidence 0–1，1 表示由平台写入语句直接推导，低于 1 表示靠命名推断。
	Confidence float64 `json:"confidence"`
}

// ColumnPair 是一条列映射。
type ColumnPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph 是一张可渲染的图。
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
	// Truncated 表示因深度或节点上限被裁剪。
	Truncated bool `json:"truncated"`
}

// Builder 从边列表构图。
type Builder struct {
	// Layers 提供节点分层信息，用于按层着色。
	Layers map[string]string
	// MaxNodes 限制节点数，防止前端渲染爆炸。
	MaxNodes int
}

// Build 把原始边整理成带节点的图。自动去重节点与边。
func (b Builder) Build(edges []Edge) Graph {
	maxNodes := b.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 500
	}
	nodes := map[string]Node{}
	edgeSet := map[string]bool{}
	// Nodes/Edges 必须是非 nil 切片：空图会被序列化成 "nodes":null，
	// 前端拿到的就是 null 而不是 []，一次 forEach 就崩。
	out := Graph{Nodes: []Node{}, Edges: []Edge{}}

	for _, e := range edges {
		if len(nodes) >= maxNodes {
			out.Truncated = true
			break
		}
		for _, id := range []string{e.Source, e.Target} {
			if _, ok := nodes[id]; ok {
				continue
			}
			n := Node{ID: id, Name: id, Type: "model"}
			if strings.HasPrefix(id, "datasource.") {
				n.Type = "datasource"
				n.Name = strings.TrimPrefix(id, "datasource.")
			} else if strings.Contains(id, ".") {
				n.Type = "table"
			}
			if b.Layers != nil {
				n.Layer = b.Layers[id]
			}
			nodes[id] = n
		}
		key := e.Source + "\x00" + e.Target
		if edgeSet[key] {
			continue
		}
		edgeSet[key] = true
		if e.Confidence == 0 {
			e.Confidence = 1
		}
		out.Edges = append(out.Edges, e)
	}

	out.Nodes = make([]Node, 0, len(nodes))
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].Source != out.Edges[j].Source {
			return out.Edges[i].Source < out.Edges[j].Source
		}
		return out.Edges[i].Target < out.Edges[j].Target
	})
	return out
}

// Direction 是展开方向。
type Direction string

const (
	Up   Direction = "up"   // 追上游：这张表从哪来
	Down Direction = "down" // 看下游：改这张表会伤到谁
	Both Direction = "both" // 双向：看一列时既查来源也查用途
)

// Focus 从指定节点出发做 N 跳展开，只保留相关子图。
// 这是前端「点一个表看血缘」的核心查询，避免一次性拉全图。
//
// dir=Both 时上下游都要走：ODS 表通常没有下游，只看 Down 会得到一张空图，
// 「模型设计 → 血缘」这种以单表为中心的视角必须双向，否则页面永远是空的。
func Focus(all []Edge, start string, dir Direction, depth int) Graph {
	if depth <= 0 || depth > 8 {
		depth = 3
	}
	neighbors := map[string][]string{}
	for _, e := range all {
		switch dir {
		case Down:
			neighbors[e.Source] = append(neighbors[e.Source], e.Target)
		case Up:
			neighbors[e.Target] = append(neighbors[e.Target], e.Source)
		default:
			neighbors[e.Source] = append(neighbors[e.Source], e.Target)
			neighbors[e.Target] = append(neighbors[e.Target], e.Source)
		}
	}

	keep := map[string]bool{start: true}
	frontier := []string{start}
	for d := 0; d < depth; d++ {
		next := []string{}
		for _, cur := range frontier {
			for _, nxt := range neighbors[cur] {
				if keep[nxt] {
					continue
				}
				keep[nxt] = true
				next = append(next, nxt)
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}

	out := []Edge{}
	for _, e := range all {
		if keep[e.Source] && keep[e.Target] {
			out = append(out, e)
		}
	}
	return Builder{}.Build(out)
}

// Paths 返回两表之间的所有路径，最多 limit 条。
// 用 DFS + 访问栈去环，保证返回的路径不含环。
func Paths(all []Edge, from, to string, limit int) [][]string {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	adj := map[string][]string{}
	for _, e := range all {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	for k := range adj {
		sort.Strings(adj[k])
	}

	var out [][]string
	var stack []string
	visited := map[string]bool{}

	var dfs func(cur string)
	dfs = func(cur string) {
		if len(out) >= limit {
			return
		}
		if cur == to {
			p := make([]string, len(stack))
			copy(p, stack)
			out = append(out, p)
			return
		}
		visited[cur] = true
		for _, nxt := range adj[cur] {
			if visited[nxt] {
				continue
			}
			stack = append(stack, nxt)
			dfs(nxt)
			stack = stack[:len(stack)-1]
		}
		visited[cur] = false
	}
	stack = []string{from}
	dfs(from)
	return out
}

// DOT 导出 Graphviz 格式，便于贴到文档或工单里。
func DOT(g Graph) string {
	var b strings.Builder
	b.WriteString("digraph lineage {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box, style=rounded, fontname=\"Helvetica\"];\n")
	for _, n := range g.Nodes {
		label := n.Name
		if n.Layer != "" {
			label = n.Layer + "\\n" + n.Name
		}
		color := "white"
		switch n.Layer {
		case "ods":
			color = "#e8f0fe"
		case "dwd":
			color = "#e6f4ea"
		case "dws":
			color = "#fff4e5"
		case "ads":
			color = "#fce8e6"
		}
		b.WriteString(fmt.Sprintf("  %q [label=%q, fillcolor=%q, style=\"rounded,filled\"];\n", n.ID, label, color))
	}
	for _, e := range g.Edges {
		b.WriteString(fmt.Sprintf("  %q -> %q;\n", e.Source, e.Target))
	}
	b.WriteString("}\n")
	return b.String()
}

// ---------- 缓存 ----------

// 缓存是带 TTL 的 LRU。热点路径反复查同一张表，缓存能把递归查询压到内存级。
//
// 表级图与列级图共用同一份实现，只是元素类型不同，所以这里用泛型而不是
// 复制一份代码——两份 LRU 的淘汰逻辑早晚会改歪。
type ttlCache[T any] struct {
	ttl   time.Duration
	max   int
	mu    sync.Mutex
	items map[string]*cacheItem[T]
	order []string
	hits  int64
	miss  int64
}

type cacheItem[T any] struct {
	v       T
	expires time.Time
}

// Cache 是表级图缓存。保留这个名字，调用方不用关心泛型细节。
type Cache = ttlCache[Graph]

// ColumnCache 是列级图缓存。
type ColumnCache = ttlCache[ColumnGraph]

func newTTLCache[T any](ttl time.Duration, max int) *ttlCache[T] {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if max <= 0 {
		max = 512
	}
	return &ttlCache[T]{ttl: ttl, max: max, items: map[string]*cacheItem[T]{}}
}

func NewCache(ttl time.Duration, max int) *Cache { return newTTLCache[Graph](ttl, max) }

func NewColumnCache(ttl time.Duration, max int) *ColumnCache {
	return newTTLCache[ColumnGraph](ttl, max)
}

func (c *ttlCache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		if ok {
			delete(c.items, key)
			c.removeOrder(key)
		}
		c.miss++
		var zero T
		return zero, false
	}
	c.hits++
	return it.v, true
}

func (c *ttlCache[T]) Put(key string, v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
	}
	c.items[key] = &cacheItem[T]{v: v, expires: time.Now().Add(c.ttl)}
	for len(c.order) > c.max {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
}

// Invalidate 在发布管道/模型后调用：血缘变了，缓存必须立刻失效。
func (c *ttlCache[T]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]*cacheItem[T]{}
	c.order = nil
}

// Stats 返回命中率，供 /metrics 与运维页展示。
func (c *ttlCache[T]) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.hits + c.miss
	rate := 0.0
	if total > 0 {
		rate = float64(c.hits) / float64(total)
	}
	return map[string]any{
		"hits": c.hits, "misses": c.miss, "hitRate": rate,
		"entries": len(c.items), "capacity": c.max,
	}
}

func (c *ttlCache[T]) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// ---------- 存储接口 ----------

// Store 是图数据的来源。
type Store interface {
	TableEdges(ctx context.Context, workspaceID string) ([]Edge, error)
	ColumnEdges(ctx context.Context, workspaceID string) ([]Edge, error)
}
