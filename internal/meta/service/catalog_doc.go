package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/quality"
)

// 数据文档站：把散在字典、质量、血缘、运行记录里的信息按「模型」聚成一页。
//
// 不新增存储，全部来自已有接口的数据源；只做一次聚合查询（见 repo.LatestNodeRuns），
// 避免一屏几十个模型打出几十次往返。
//
// 一页要回答四个问题：这张表是什么（字段/注释/分级）、谁在用（上下游）、
// 有没有守住（质量规则与最近结果）、新不新（最近一次成功与新鲜度）。

// CatalogColumn 是文档站的字段行。
type CatalogColumn struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Physical       string `json:"physical,omitempty"`
	Nullable       bool   `json:"nullable"`
	Comment        string `json:"comment,omitempty"`
	Classification string `json:"classification,omitempty"`
	Mask           string `json:"mask,omitempty"`
}

// CatalogQuality 是文档站展示的一条质量规则（来自管道质量节点）。
type CatalogQuality struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Column   string `json:"column,omitempty"`
	Severity string `json:"severity"`
	// ThresholdMin 仅 freshness 规则有：多少分钟算过期。
	ThresholdMin float64 `json:"thresholdMin,omitempty"`
	// LastPassed 为 nil 表示还没有跑过这条规则。
	LastPassed *bool  `json:"lastPassed,omitempty"`
	LastAt     string `json:"lastAt,omitempty"`
	Observed   string `json:"observed,omitempty"`
}

// CatalogRun 是最近一次运行摘要。
// 带上 pipeline / node：文档站要能一键跳到「这次运行写的 SQL」，
// 只给实例 id 的话前端还得反查节点名。
type CatalogRun struct {
	InstanceID string `json:"instanceId"`
	Pipeline   string `json:"pipeline,omitempty"`
	Node       string `json:"node,omitempty"`
	Status     string `json:"status"`
	BizDate    string `json:"bizDate"`
	FinishedAt string `json:"finishedAt"`
}

// CatalogFreshness 是新鲜度状态。
//
// status 的三种取值必须区分清楚：
//   - fresh/stale：有 freshness 规则，按「最近一次成功距今 vs 阈值」判定；
//   - unknown：没有规则，只给时间不装懂。
//
// 真正的数据新鲜度要查库（MAX(时间列)），一屏几十个模型逐个查库不现实；
// 这里用的是运行侧证据，界面上也这么写。
type CatalogFreshness struct {
	Status       string  `json:"status"`
	AgeMinutes   float64 `json:"ageMinutes"`
	ThresholdMin float64 `json:"thresholdMin,omitempty"`
	Basis        string  `json:"basis"`
}

// CatalogDoc 是一个模型的完整文档。
type CatalogDoc struct {
	Name        string           `json:"name"`
	Layer       string           `json:"layer"`
	Table       string           `json:"table"`
	Description string           `json:"description"`
	Owner       string           `json:"owner"`
	Tags        []string         `json:"tags"`
	Version     int              `json:"version"`
	Versions    int              `json:"versions"`
	Materializ  string           `json:"materialization,omitempty"`
	DDL         string           `json:"ddl,omitempty"`
	Columns     []CatalogColumn  `json:"columns"`
	Upstream    []string         `json:"upstream"`
	Downstream  []string         `json:"downstream"`
	Quality     []CatalogQuality `json:"quality"`
	LastRun     *CatalogRun      `json:"lastRun,omitempty"`
	Freshness   CatalogFreshness `json:"freshness"`
}

// CatalogPage 是接口返回体。
type CatalogPage struct {
	GeneratedAt string       `json:"generatedAt"`
	Layers      []string     `json:"layers"`
	Entries     []CatalogDoc `json:"entries"`
}

var layerOrder = map[string]int{"ods": 0, "dim": 1, "dwd": 2, "dws": 3, "ads": 4}

// CatalogDocs 组装文档站数据。
func (s *Service) CatalogDocs(ctx context.Context, workspaceID string) (*CatalogPage, error) {
	entries, err := s.Dictionary(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]model.Model, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}

	versions, err := s.Repo.ModelVersionCounts(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	pipes, err := s.Repo.ListPipelines(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// 节点 → 模型：质量结果与最近运行都靠它。
	pipelineName := map[string]string{}
	pipelineNodeModel := map[string]map[string]string{}
	qualityNodeModel := map[string]map[string]string{}

	// 规则的主源是模型上的规则表（一次取全工作区，避免按模型 N+1）。
	// 管道里内联的 config.rules 仍要展示：它是旧写法，且同名以库为准。
	storedRules, err := s.Repo.ListQualityRulesByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	rules := map[string][]CatalogQuality{}
	ruleSeen := map[string]map[string]bool{}
	addRule := func(modelName string, q CatalogQuality) {
		if ruleSeen[modelName] == nil {
			ruleSeen[modelName] = map[string]bool{}
		}
		if ruleSeen[modelName][q.Name] {
			return
		}
		ruleSeen[modelName][q.Name] = true
		rules[modelName] = append(rules[modelName], q)
	}
	for modelName, list := range storedRules {
		for _, q := range list {
			// 停用的规则不算契约：文档站展示「现在会拦什么」。
			if !quality.IsEnabled(q) {
				continue
			}
			cq := CatalogQuality{Name: q.Name, Kind: q.Kind, Column: q.Column, Severity: q.Severity}
			if cq.Severity == "" {
				cq.Severity = "error"
			}
			if q.Kind == quality.KindFreshness {
				cq.ThresholdMin, _ = quality.FreshnessWindow(q.Config)
			}
			addRule(modelName, cq)
		}
	}
	for _, p := range pipes {
		if p.Spec == nil {
			continue
		}
		m := map[string]string{}
		qm := map[string]string{}
		for _, n := range p.Spec.Nodes {
			// 只有真正写表的节点才算「这个模型的运行」。质量/分支节点也带 model，
			// 但它们跑完不代表数据刷新了，拿它们当最近运行会给出错误的时间。
			if n.Model != "" && (n.Type == model.NodeSQL || n.Type == model.NodeSink) {
				m[n.ID] = n.Model
			}
			if n.Type != model.NodeQuality || n.Model == "" {
				continue
			}
			qm[n.ID] = n.Model
			for _, q := range parseQualityRules(n.Config) {
				addRule(n.Model, q)
			}
		}
		pipelineName[p.ID] = p.Name
		pipelineNodeModel[p.ID] = m
		qualityNodeModel[p.ID] = qm
	}

	runs, err := s.Repo.LatestNodeRuns(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	snapshots, err := s.Repo.LatestQualityResults(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// 结果按「模型 + 规则名」归属：规则可能来自库、也可能内联，
	// 但执行结果只记 (管道, 节点, 规则名)，模型要从质量节点反查。
	lastResult := make(map[string]CatalogQuality, len(snapshots))
	lastResultAt := make(map[string]time.Time, len(snapshots))
	for _, q := range snapshots {
		modelName := qualityNodeModel[q.PipelineID][q.NodeID]
		if modelName == "" {
			continue
		}
		key := modelName + "\x00" + q.RuleName
		if prev, ok := lastResultAt[key]; ok && !q.At.After(prev) {
			continue
		}
		lastResultAt[key] = q.At
		passed := q.Passed
		lastResult[key] = CatalogQuality{
			LastPassed: &passed, LastAt: q.At.Format(time.RFC3339), Observed: q.Observed,
		}
	}
	lastRun := map[string]CatalogRun{}
	lastAt := map[string]time.Time{}
	for _, r := range runs {
		name := pipelineNodeModel[r.PipelineID][r.NodeID]
		if name == "" {
			continue
		}
		// 同一模型可能被多条管道写：取最晚的那次。
		if prev, ok := lastAt[name]; ok && !r.FinishedAt.After(prev) {
			continue
		}
		lastAt[name] = r.FinishedAt
		lastRun[name] = CatalogRun{
			InstanceID: r.InstanceID, Pipeline: pipelineName[r.PipelineID], Node: r.NodeID,
			Status: r.Status, BizDate: r.BizDate,
			FinishedAt: r.FinishedAt.Format(time.RFC3339),
		}
	}

	now := time.Now()
	out := make([]CatalogDoc, 0, len(entries))
	for _, e := range entries {
		doc := CatalogDoc{
			Name: e.Name, Layer: e.Layer, Table: e.Table,
			Description: e.Description, Owner: e.Owner,
			// 空 tags / 血缘序列化成 null 会让前端多写一层兜底，统一给空切片。
			Tags:       append([]string{}, e.Tags...),
			Upstream:   append([]string{}, e.Upstream...),
			Downstream: append([]string{}, e.Downstream...),
			Columns:    make([]CatalogColumn, 0, len(e.Columns)),
			Quality:    []CatalogQuality{},
		}
		if m, ok := byName[e.Name]; ok {
			doc.Version = m.ActiveVersion
			doc.DDL = m.DDL
			if m.Spec != nil {
				doc.Materializ = m.Spec.Materialization
				if doc.Description == "" {
					doc.Description = m.Spec.Description
				}
				if doc.Owner == "" {
					doc.Owner = m.Spec.Owner
				}
				if len(doc.Tags) == 0 {
					doc.Tags = m.Spec.Tags
				}
			}
		}
		doc.Versions = versions[e.Name]
		for _, c := range e.Columns {
			doc.Columns = append(doc.Columns, CatalogColumn{
				Name: c.Name, Type: c.Type, Physical: c.Physical, Nullable: c.Nullable,
				Comment: c.Comment, Classification: c.Classification, Mask: c.Mask,
			})
		}
		doc.Quality = append(doc.Quality, rules[e.Name]...)
		for i := range doc.Quality {
			if snap, ok := lastResult[e.Name+"\x00"+doc.Quality[i].Name]; ok {
				doc.Quality[i].LastPassed = snap.LastPassed
				doc.Quality[i].LastAt = snap.LastAt
				doc.Quality[i].Observed = snap.Observed
			}
		}
		if r, ok := lastRun[e.Name]; ok {
			rc := r
			doc.LastRun = &rc
			age := now.Sub(lastAt[e.Name]).Minutes()
			doc.Freshness = CatalogFreshness{AgeMinutes: round1(age), Basis: "last_success"}
			if r.Status != "success" {
				doc.Freshness.Basis = "last_" + r.Status
			}
			doc.Freshness.Status = "unknown"
			if th, _ := freshnessThreshold(doc.Quality); th > 0 {
				doc.Freshness.ThresholdMin = th
				if r.Status == "success" && age <= th {
					doc.Freshness.Status = "fresh"
				} else {
					doc.Freshness.Status = "stale"
				}
			}
		} else {
			doc.Freshness = CatalogFreshness{Status: "unknown", Basis: "no_run"}
		}
		// 有 freshness 规则且跑过结果时，以规则为准：它查的是数据本身（MAX(时间列)），
		// 比「上次任务什么时候成功」更接近真相。运行成功但数据没更新的情况很常见，
		// 只按运行时间推断会给出「新鲜」这种与事实相反的结论。
		for _, q := range doc.Quality {
			if q.Kind != quality.KindFreshness || q.LastPassed == nil {
				continue
			}
			doc.Freshness.Basis = "quality_rule"
			if q.ThresholdMin > 0 {
				doc.Freshness.ThresholdMin = q.ThresholdMin
			}
			if *q.LastPassed {
				doc.Freshness.Status = "fresh"
			} else {
				doc.Freshness.Status = "stale"
			}
			break
		}
		out = append(out, doc)
	}

	sort.SliceStable(out, func(i, j int) bool {
		li, lj := layerOrder[out[i].Layer], layerOrder[out[j].Layer]
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})

	layers := []string{}
	seen := map[string]bool{}
	for _, d := range out {
		if d.Layer == "" || seen[d.Layer] {
			continue
		}
		seen[d.Layer] = true
		layers = append(layers, d.Layer)
	}
	return &CatalogPage{
		GeneratedAt: now.Format(time.RFC3339),
		Layers:      layers,
		Entries:     out,
	}, nil
}

// freshnessThreshold 取该模型全部 freshness 规则里最严格（最小）的阈值。
// 取最小而不是第一条：两条规则都配了的时候，按宽的算等于悄悄放宽了门禁。
func freshnessThreshold(rules []CatalogQuality) (float64, string) {
	best := 0.0
	label := ""
	for _, r := range rules {
		if r.Kind != quality.KindFreshness {
			continue
		}
		// 规则里没带阈值时按默认 1 天，与执行期一致。
		cfg := map[string]any{}
		if r.ThresholdMin > 0 {
			cfg["max_age_min"] = r.ThresholdMin
		}
		min, text := quality.FreshnessWindow(cfg)
		if best == 0 || min < best {
			best, label = min, text
		}
	}
	return best, label
}

// parseQualityRules 从质量节点的 config.rules 里读出规则清单。
func parseQualityRules(cfg map[string]any) []CatalogQuality {
	raw, ok := cfg["rules"].([]any)
	if !ok {
		return nil
	}
	out := make([]CatalogQuality, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		q := CatalogQuality{
			Name:     str(m["name"]),
			Kind:     str(m["kind"]),
			Column:   str(m["column"]),
			Severity: str(m["severity"]),
		}
		if q.Kind == quality.KindFreshness {
			// 阈值在这里就换算成分钟：前端与文档站不应该各写一套小时/天的换算。
			q.ThresholdMin, _ = quality.FreshnessWindow(m)
		}
		if q.Severity == "" {
			q.Severity = "error"
		}
		out = append(out, q)
	}
	return out
}

func round1(f float64) float64 {
	if f < 0 {
		return 0
	}
	return float64(int(f*10+0.5)) / 10
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
