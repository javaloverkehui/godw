package service

import (
	"context"
	"strings"
	"time"

	"github.com/godw/godw/internal/lineage/graph"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 血缘图服务：统一提供「整图 / 聚焦子图 / 路径 / DOT 导出」四种形态。
// 缓存放在服务层而不是 HTTP 层，导出与分享走同一份数据，避免两边不一致。

var lineageCache = graph.NewCache(5*time.Minute, 256)
var lineageColumnCache = graph.NewColumnCache(5*time.Minute, 128)

// lineageEdges 从仓储加载表级血缘边，并补上列级映射。
func (s *Service) lineageEdges(ctx context.Context, workspaceID string) ([]graph.Edge, error) {
	edges, err := s.Repo.ListLineage(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]graph.Edge, 0, len(edges))
	for _, e := range edges {
		out = append(out, graph.Edge{Source: e.Upstream, Target: e.Downstream, Confidence: 1})
	}

	// 列级映射：按 (下游表, 上游表) 聚合，用于边上的列配对展示。
	cols, cerr := s.Repo.AllColumnEdges(ctx, workspaceID)
	if cerr == nil {
		pair := map[string][]graph.ColumnPair{}
		for _, c := range cols {
			key := c.UpstreamTable + "\x00" + c.DownstreamTable
			pair[key] = append(pair[key], graph.ColumnPair{From: c.UpstreamColumn, To: c.DownstreamColumn})
		}
		for i := range out {
			key := out[i].Source + "\x00" + out[i].Target
			if pairs, ok := pair[key]; ok {
				out[i].Columns = pairs
			}
		}
	}
	return out, nil
}

// lineageLayers 返回节点到层的映射，供前端着色。
func (s *Service) lineageLayers(ctx context.Context, workspaceID string) map[string]string {
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, m := range models {
		out[m.Name] = m.Layer
	}
	return out
}

// LineageGraph 返回整图（节点数受限）。
func (s *Service) LineageGraph(ctx context.Context, workspaceID string) (graph.Graph, error) {
	if g, ok := lineageCache.Get("all:" + workspaceID); ok {
		return g, nil
	}
	edges, err := s.lineageEdges(ctx, workspaceID)
	if err != nil {
		return graph.Graph{}, err
	}
	g := graph.Builder{Layers: s.lineageLayers(ctx, workspaceID), MaxNodes: 800}.Build(edges)
	lineageCache.Put("all:"+workspaceID, g)
	return g, nil
}

// LineageFocus 返回从某表出发的 N 跳子图。
// direction 缺省为双向：单表视角下没人只想看下游，ODS 表更是只有上游。
func (s *Service) LineageFocus(ctx context.Context, workspaceID, table, direction string, depth int) (graph.Graph, error) {
	key := "focus:" + workspaceID + ":" + table + ":" + direction + ":" + itoaSmall(depth)
	if g, ok := lineageCache.Get(key); ok {
		return g, nil
	}
	edges, err := s.lineageEdges(ctx, workspaceID)
	if err != nil {
		return graph.Graph{}, err
	}
	dir := graph.Both
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "up", "upstream":
		dir = graph.Up
	case "down", "downstream":
		dir = graph.Down
	}
	g := graph.Focus(edges, table, dir, depth)
	// Focus 返回的节点不带层信息，这里补齐。
	layers := s.lineageLayers(ctx, workspaceID)
	for i := range g.Nodes {
		if l, ok := layers[g.Nodes[i].ID]; ok {
			g.Nodes[i].Layer = l
		}
	}
	lineageCache.Put(key, g)
	return g, nil
}

// LineagePaths 返回两表之间的路径。
func (s *Service) LineagePaths(ctx context.Context, workspaceID, from, to string, limit int) ([][]string, error) {
	edges, err := s.lineageEdges(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return graph.Paths(edges, from, to, limit), nil
}

// LineageDOT 导出 Graphviz。
func (s *Service) LineageDOT(ctx context.Context, workspaceID, table string) (string, error) {
	var g graph.Graph
	var err error
	if table == "" {
		g, err = s.LineageGraph(ctx, workspaceID)
	} else {
		g, err = s.LineageFocus(ctx, workspaceID, table, "down", 3)
	}
	if err != nil {
		return "", err
	}
	return graph.DOT(g), nil
}

// ColumnGraph 返回以「表.列」为中心的列级子图。
//
// 未指定 column 时自动挑一列：优先选该模型产出、上游最多的列，
// 让「点开一张表」也有内容可看，而不是要求用户先猜列名。
func (s *Service) ColumnGraph(ctx context.Context, workspaceID, table, column, direction string, depth int) (graph.ColumnGraph, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return graph.ColumnGraph{}, errs.New(errs.CodeInvalidArgument, "缺少 table")
	}
	key := "colgraph:" + workspaceID + ":" + table + ":" + column + ":" + direction + ":" + itoaSmall(depth)
	if g, ok := lineageColumnCache.Get(key); ok {
		return g, nil
	}

	cols, err := s.Repo.AllColumnEdges(ctx, workspaceID)
	if err != nil {
		return graph.ColumnGraph{}, err
	}
	links := make([]graph.ColumnLink, 0, len(cols))
	for _, c := range cols {
		links = append(links, graph.ColumnLink{
			Upstream:   graph.ColumnRef{Table: c.UpstreamTable, Column: c.UpstreamColumn},
			Downstream: graph.ColumnRef{Table: c.DownstreamTable, Column: c.DownstreamColumn},
		})
	}

	specs := s.modelSpecs(ctx, workspaceID)
	if column == "" {
		column = pickFocusColumn(links, table, specs)
	}
	if column == "" {
		// 整张表没有任何列级血缘：返回空图，不造一个 column 为空的假节点。
		return graph.ColumnGraph{
			Focus: graph.ColumnRef{Table: table},
			Nodes: []graph.ColumnNode{},
			Edges: []graph.ColumnEdge{},
		}, nil
	}

	dir := graph.Both
	switch strings.ToLower(direction) {
	case "up", "upstream":
		dir = graph.Up
	case "down", "downstream":
		dir = graph.Down
	}
	g := graph.ColumnFocus(links, graph.ColumnRef{Table: table, Column: column}, dir, depth)
	g = enrichColumns(g, specs)
	lineageColumnCache.Put(key, g)
	return g, nil
}

// pickFocusColumn 选默认聚焦列：优先普通字段，其次分区，最后主键。
// 只看「被引用次数最多」会稳定选中 dt——它是每张表都有的分区列，
// 但没人想打开血缘页先看到日期。
func pickFocusColumn(links []graph.ColumnLink, table string, specs map[string]*model.ModelSpec) string {
	count := map[string]int{}
	order := []string{}
	for _, l := range links {
		if l.Downstream.Table == table {
			if count[l.Downstream.Column] == 0 {
				order = append(order, l.Downstream.Column)
			}
			count[l.Downstream.Column]++
		}
	}
	if len(order) == 0 {
		// 这张表只有作为上游出现（例如只被引用的维表），退而取它的任意一列。
		for _, l := range links {
			if l.Upstream.Table == table {
				return l.Upstream.Column
			}
		}
		return ""
	}
	spec := specs[table]
	score := func(col string) int {
		if spec == nil {
			return 2
		}
		for _, p := range spec.PartitionBy {
			if strings.EqualFold(p, col) {
				return 1
			}
		}
		for _, k := range spec.PrimaryKey {
			if strings.EqualFold(k, col) {
				return 0
			}
		}
		return 3
	}
	best := order[0]
	for _, c := range order {
		if score(c) > score(best) || (score(c) == score(best) && count[c] > count[best]) {
			best = c
		}
	}
	return best
}

func (s *Service) modelSpecs(ctx context.Context, workspaceID string) map[string]*model.ModelSpec {
	out := map[string]*model.ModelSpec{}
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return out
	}
	for _, m := range models {
		if m.Spec != nil {
			out[m.Name] = m.Spec
		}
	}
	return out
}

// enrichColumns 补齐层的着色信息与列的语义信息（主键/分区/类型）。
// 表不在本工作区模型里时标记 External，前端不再声称「可下钻到字段」。
func enrichColumns(g graph.ColumnGraph, specs map[string]*model.ModelSpec) graph.ColumnGraph {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		spec, ok := specs[n.Table]
		if !ok {
			n.External = true
			continue
		}
		n.Layer = spec.Layer
		n.Kind = "field"
		for _, k := range spec.PrimaryKey {
			if strings.EqualFold(k, n.Column) {
				n.Kind = "key"
			}
		}
		for _, p := range spec.PartitionBy {
			if strings.EqualFold(p, n.Column) && n.Kind == "field" {
				n.Kind = "partition"
			}
		}
		for _, c := range spec.Columns {
			if strings.EqualFold(c.Name, n.Column) {
				n.Type = c.Type
				break
			}
		}
	}
	return g
}

// InvalidateLineage 在发布管道或模型后调用，让图立刻反映最新结构。
func (s *Service) InvalidateLineage() {
	lineageCache.Invalidate()
	lineageColumnCache.Invalidate()
}

// LineageCacheStats 返回缓存命中率。
func (s *Service) LineageCacheStats() map[string]any {
	return map[string]any{
		"table":  lineageCache.Stats(),
		"column": lineageColumnCache.Stats(),
	}
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
