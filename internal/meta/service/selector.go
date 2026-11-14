package service

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
)

// 选择器：dbt 风格的模型选择表达式。
//
// 存在的理由：`trigger` 只能按管道名触发，一个模型改了要人工找出所有相关管道。
// 选择器把「我要跑哪个模型」变成一句可复制的表达式，CLI 与界面共用同一套解析。
//
// 语法（可逗号或空格分隔多个项，取并集）：
//
//	dwd_order          精确模型名
//	dwd_*              通配符（仅 * 与 ?）
//	dwd_order+         该模型 + 全部下游
//	+dwd_order         该模型 + 全部上游
//	+dwd_order+        双向闭包
//	tag:core           带该 tag 的模型
//	layer:dwd          某一分层全部模型
//	pipeline:order_daily  某管道写出的模型
//
// 一处必须说清的语义：GoDW 不支持节点级部分运行，选择器解析出的模型集合
// 最终会映射到「写这些模型的管道」，执行仍是整条管道。返回里带 models 与
// pipelines 两份清单，就是为了让人看到这个放大效应，而不是以为只跑了一个节点。

// SelectTerm 是选择器中的一项。
type SelectTerm struct {
	Text  string `json:"text"`
	Kind  string `json:"kind"` // model | tag | layer | pipeline
	Value string `json:"value"`
	Up    bool   `json:"up"`
	Down  bool   `json:"down"`
}

// ParseSelect 解析表达式。空表达式返回空切片而不是错误：
// 调用方可以用「有 select 就按 select，否则按 pipeline」。
func ParseSelect(raw string) ([]SelectTerm, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]SelectTerm, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		t := SelectTerm{Text: f}
		if strings.HasPrefix(f, "+") {
			t.Up = true
			f = strings.TrimPrefix(f, "+")
		}
		if strings.HasSuffix(f, "+") {
			t.Down = true
			f = strings.TrimSuffix(f, "+")
		}
		if f == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "选择器 "+t.Text+" 缺少模型名")
		}
		t.Kind, t.Value = "model", f
		if i := strings.Index(f, ":"); i > 0 {
			switch f[:i] {
			case "tag", "layer", "pipeline", "model":
				t.Kind, t.Value = f[:i], f[i+1:]
			default:
				return nil, errs.New(errs.CodeInvalidArgument,
					"未知的选择器前缀: "+f[:i]+"（支持 tag: / layer: / pipeline: / model:）")
			}
		}
		if t.Value == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "选择器 "+t.Text+" 缺少取值")
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.CodeInvalidArgument, "选择器为空")
	}
	return out, nil
}

// SelectModel 是解析选择器所需的最小模型视图。
type SelectModel struct {
	Name  string
	Layer string
	Tags  []string
}

// SelectEdge 是模型间的直接依赖（上游 → 下游）。
type SelectEdge struct {
	Up   string
	Down string
}

// ExpandSelect 把表达式展开成模型名集合。
// pipelines 用于 pipeline: 前缀：管道名 → 该管道写出的模型列表。
func ExpandSelect(terms []SelectTerm, models []SelectModel, edges []SelectEdge,
	pipelines map[string][]string) ([]string, error) {

	byName := make(map[string]SelectModel, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}
	down := map[string][]string{}
	up := map[string][]string{}
	for _, e := range edges {
		if e.Up == "" || e.Down == "" || e.Up == e.Down {
			continue
		}
		if _, ok := byName[e.Up]; !ok {
			continue
		}
		if _, ok := byName[e.Down]; !ok {
			continue
		}
		down[e.Up] = appendUnique(down[e.Up], e.Down)
		up[e.Down] = appendUnique(up[e.Down], e.Up)
	}

	selected := map[string]bool{}
	for _, t := range terms {
		base := map[string]bool{}
		switch t.Kind {
		case "model":
			for _, m := range models {
				if ok, _ := path.Match(t.Value, m.Name); ok {
					base[m.Name] = true
				}
			}
			if len(base) == 0 {
				return nil, errs.New(errs.CodeNotFound, "没有匹配的模型: "+t.Value).
					WithMeta("selector", t.Text)
			}
		case "tag":
			for _, m := range models {
				for _, tag := range m.Tags {
					if strings.EqualFold(tag, t.Value) {
						base[m.Name] = true
					}
				}
			}
			if len(base) == 0 {
				return nil, errs.New(errs.CodeNotFound, "没有模型带 tag: "+t.Value)
			}
		case "layer":
			for _, m := range models {
				if strings.EqualFold(m.Layer, t.Value) {
					base[m.Name] = true
				}
			}
			if len(base) == 0 {
				return nil, errs.New(errs.CodeNotFound, "分层下没有模型: "+t.Value)
			}
		case "pipeline":
			list, ok := pipelines[t.Value]
			if !ok {
				return nil, errs.New(errs.CodeNotFound, "管道不存在或未发布: "+t.Value)
			}
			for _, m := range list {
				if _, ok := byName[m]; ok {
					base[m] = true
				}
			}
			if len(base) == 0 {
				return nil, errs.New(errs.CodeNotFound, "管道没有写任何已发布模型: "+t.Value)
			}
		}

		for name := range base {
			selected[name] = true
			if t.Up {
				for _, n := range closure(name, up) {
					selected[n] = true
				}
			}
			if t.Down {
				for _, n := range closure(name, down) {
					selected[n] = true
				}
			}
		}
	}

	out := make([]string, 0, len(selected))
	for n := range selected {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// closure 广度优先收集可达节点（不含起点，起点已单独加入）。
func closure(start string, adj map[string][]string) []string {
	seen := map[string]bool{start: true}
	var out []string
	queue := append([]string{}, adj[start]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, adj[cur]...)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// SelectResult 是解析结果，同时也是触发接口的响应体。
type SelectResult struct {
	Select    string   `json:"select"`
	Models    []string `json:"models"`
	Pipelines []string `json:"pipelines"`
	// Uncovered 是被选中但没有管道写出的模型：它们不会被任何一次触发覆盖。
	// 不报错是因为「先跑下游」是常见诉求，但必须让人看到漏了哪些。
	Uncovered []string `json:"uncovered"`
}

// ResolveSelect 解析表达式并映射到管道。
func (s *Service) ResolveSelect(ctx context.Context, workspaceID, expr string) (*SelectResult, error) {
	terms, err := ParseSelect(expr)
	if err != nil {
		return nil, err
	}
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	refs := make([]SelectModel, 0, len(models))
	for _, m := range models {
		ref := SelectModel{Name: m.Name, Layer: m.Layer}
		if m.Spec != nil {
			ref.Tags = m.Spec.Tags
			if ref.Layer == "" {
				ref.Layer = m.Spec.Layer
			}
		}
		refs = append(refs, ref)
	}

	edges, err := s.Repo.ListLineage(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	sel := make([]SelectEdge, 0, len(edges))
	for _, e := range edges {
		if strings.HasPrefix(e.Upstream, "datasource.") {
			continue
		}
		sel = append(sel, SelectEdge{Up: e.Upstream, Down: e.Downstream})
	}

	pipes, err := s.Repo.ListPipelines(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// 只有已发布的管道能触发；未发布的列进来会让「解析成功但触发失败」。
	byPipeline := map[string][]string{}
	pipelineModels := map[string][]string{}
	for _, p := range pipes {
		if p.ActiveVersion == 0 || p.Spec == nil {
			continue
		}
		var written []string
		for _, n := range p.Spec.Nodes {
			if n.Model == "" || n.Type == "quality" {
				continue
			}
			written = appendUnique(written, n.Model)
		}
		pipelineModels[p.Name] = written
		byPipeline[p.Name] = written
	}

	chosen, err := ExpandSelect(terms, refs, sel, byPipeline)
	if err != nil {
		return nil, err
	}

	inSet := map[string]bool{}
	for _, m := range chosen {
		inSet[m] = true
	}
	// 显式初始化：nil 切片会序列化成 null，前端对 .length 取值会直接抛。
	hitPipes, uncovered := []string{}, []string{}
	covered := map[string]bool{}
	for name, written := range pipelineModels {
		for _, m := range written {
			if inSet[m] {
				hitPipes = append(hitPipes, name)
				for _, w := range written {
					covered[w] = true
				}
				break
			}
		}
	}
	for _, m := range chosen {
		if !covered[m] {
			uncovered = append(uncovered, m)
		}
	}
	sort.Strings(hitPipes)

	return &SelectResult{
		Select: expr, Models: chosen, Pipelines: hitPipes, Uncovered: uncovered,
	}, nil
}

// TriggerSelect 按表达式触发所有命中管道，返回每个管道的实例。
// 单个管道触发失败不中断整批：批里一条坏管道不该挡住其它九条，
// 失败项原样带回给调用方逐条处理。
func (s *Service) TriggerSelect(ctx context.Context, workspaceID, expr, bizDate string,
	params map[string]any) ([]SelectRun, *SelectResult, error) {

	res, err := s.ResolveSelect(ctx, workspaceID, expr)
	if err != nil {
		return nil, nil, err
	}
	out := make([]SelectRun, 0, len(res.Pipelines))
	for _, name := range res.Pipelines {
		inst, created, err := s.Trigger(ctx, workspaceID, name, bizDate, "select", params)
		item := SelectRun{Pipeline: name}
		if err != nil {
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		item.InstanceID = inst.ID
		item.Created = created
		out = append(out, item)
	}
	return out, res, nil
}

// SelectRun 是单个管道的触发结果。
type SelectRun struct {
	Pipeline   string `json:"pipeline"`
	InstanceID string `json:"instanceId,omitempty"`
	Created    bool   `json:"created"`
	Error      string `json:"error,omitempty"`
}

func (r SelectResult) String() string {
	return fmt.Sprintf("%s → %d 个模型 / %d 条管道", r.Select, len(r.Models), len(r.Pipelines))
}
