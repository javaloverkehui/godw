package compile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/sqlx"
	"gopkg.in/yaml.v3"
)

// Parse 严格解析管道 YAML：未知字段直接报错，避免拼写错误静默生效。
func Parse(data []byte) (*model.PipelineSpec, error) {
	var spec model.PipelineSpec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, errs.Wrap(errs.CodePipelineInvalid, err, "管道 YAML 解析失败")
	}
	if spec.Kind != "" && spec.Kind != "Pipeline" {
		return nil, errs.New(errs.CodePipelineInvalid, "kind 必须为 Pipeline")
	}
	return &spec, nil
}

// Validate 校验结构与图。返回的问题一次性列出，避免用户逐个试错。
func Validate(spec *model.PipelineSpec) error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if spec.Name == "" {
		add("name 不能为空")
	}
	if len(spec.Nodes) == 0 {
		add("nodes 不能为空")
	}

	ids := map[string]model.NodeSpec{}
	for i, n := range spec.Nodes {
		if n.ID == "" {
			add("nodes[%d].id 不能为空", i)
			continue
		}
		if _, dup := ids[n.ID]; dup {
			add("节点 ID 重复: %s", n.ID)
		}
		ids[n.ID] = n
		if !model.ValidNodeType(n.Type) {
			add("节点 %s 的类型 %q 不受支持", n.ID, n.Type)
		}
		if !model.ValidWriteMode(n.WriteMode) {
			add("节点 %s 的 write_mode %q 不受支持", n.ID, n.WriteMode)
		}
		switch n.Type {
		case model.NodeSQL, model.NodeSource:
			if strings.TrimSpace(n.SQL) == "" {
				add("节点 %s 缺少 sql", n.ID)
			}
		case model.NodeQuality:
			if n.Model == "" {
				add("质量节点 %s 必须指定 model", n.ID)
			}
		case model.NodeSink:
			if n.Model == "" && n.Config["path"] == nil {
				add("输出节点 %s 需指定 model 或 config.path", n.ID)
			}
		}
	}

	for _, n := range spec.Nodes {
		for _, dep := range n.DependsOn {
			if dep == n.ID {
				add("节点 %s 依赖自身", n.ID)
			} else if _, ok := ids[dep]; !ok {
				add("节点 %s 依赖不存在的节点 %s", n.ID, dep)
			}
		}
	}

	if cycle := findCycle(spec.Nodes); len(cycle) > 0 {
		add("检测到环: %s", strings.Join(cycle, " → "))
	}

	// 参数引用必须在 params 中声明，避免运行期才发现缺值。
	declared := map[string]bool{"biz_date": true, "biz_ts": true}
	for _, p := range spec.Params {
		declared[p.Name] = true
	}
	for _, n := range spec.Nodes {
		for _, v := range sqlx.Vars(n.SQL) {
			if !declared[v] {
				add("节点 %s 引用了未声明的参数 ${%s}", n.ID, v)
			}
		}
	}

	if len(problems) > 0 {
		return errs.New(errs.CodePipelineInvalid, "管道定义不合法").WithMeta("problems", problems)
	}
	return nil
}

// findCycle 返回环上的节点序列，无环返回 nil。
func findCycle(nodes []model.NodeSpec) []string {
	graph := map[string][]string{}
	for _, n := range nodes {
		graph[n.ID] = n.DependsOn
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var found []string

	var dfs func(id string) bool
	dfs = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range graph[id] {
			switch color[dep] {
			case gray:
				// 截取环部分
				for i, s := range stack {
					if s == dep {
						found = append(append([]string{}, stack[i:]...), dep)
						return true
					}
				}
			case white:
				if dfs(dep) {
					return true
				}
			}
		}
		color[id] = black
		stack = stack[:len(stack)-1]
		return false
	}
	for _, n := range nodes {
		if color[n.ID] == white && dfs(n.ID) {
			return found
		}
	}
	return nil
}

// Level 为每个节点计算执行层级：无依赖为 0，否则为依赖最大层 + 1。
// 同一层的节点可以并发执行，调度器据此派发。
func Level(nodes []model.NodeSpec) map[string]int {
	deps := map[string][]string{}
	for _, n := range nodes {
		deps[n.ID] = n.DependsOn
	}
	memo := map[string]int{}
	var depth func(id string, seen map[string]bool) int
	depth = func(id string, seen map[string]bool) int {
		if v, ok := memo[id]; ok {
			return v
		}
		if seen[id] {
			return 0
		}
		seen[id] = true
		max := -1
		for _, d := range deps[id] {
			if lv := depth(d, seen); lv > max {
				max = lv
			}
		}
		memo[id] = max + 1
		return memo[id]
	}
	for _, n := range nodes {
		depth(n.ID, map[string]bool{})
	}
	return memo
}

// Topo 返回拓扑序；同层按 ID 排序保证结果稳定（便于快照对比）。
func Topo(nodes []model.NodeSpec) []model.NodeSpec {
	lv := Level(nodes)
	out := make([]model.NodeSpec, len(nodes))
	copy(out, nodes)
	sort.SliceStable(out, func(i, j int) bool {
		if lv[out[i].ID] != lv[out[j].ID] {
			return lv[out[i].ID] < lv[out[j].ID]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Stages 把节点按层分组，用于「同层并发」执行计划与画布布局。
func Stages(nodes []model.NodeSpec) [][]model.NodeSpec {
	lv := Level(nodes)
	maxLv := 0
	for _, v := range lv {
		if v > maxLv {
			maxLv = v
		}
	}
	out := make([][]model.NodeSpec, maxLv+1)
	for _, n := range nodes {
		l := lv[n.ID]
		out[l] = append(out[l], n)
	}
	for i := range out {
		sort.Slice(out[i], func(a, b int) bool { return out[i][a].ID < out[i][b].ID })
	}
	return out
}

// Render 渲染所有节点 SQL，并返回按节点 ID 索引的结果。
func Render(spec *model.PipelineSpec, vars map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range spec.Nodes {
		if strings.TrimSpace(n.SQL) == "" {
			continue
		}
		rendered, err := sqlx.Render(n.SQL, vars)
		if err != nil {
			return nil, errs.Wrap(errs.CodeCompileFailed, err, "节点 "+n.ID+" 渲染失败")
		}
		out[n.ID] = rendered
	}
	return out, nil
}

// Upstream 返回指定节点的所有直接上游。
func Upstream(nodes []model.NodeSpec, id string) []string {
	for _, n := range nodes {
		if n.ID == id {
			return n.DependsOn
		}
	}
	return nil
}
