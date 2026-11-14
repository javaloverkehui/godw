package service

import (
	"context"
	"sort"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

// 本文件承载建模规范相关的用例：规范自检、脚手架、结构对比、数据字典。
// 它们都依赖「已发布模型集合」，所以放在 service 层而不是纯编译器里。

// ModelCheckResult 是模型规范与类型的联合自检结果。
type ModelCheckResult struct {
	Valid      bool                   `json:"valid"`
	Violations []modelc.RuleViolation `json:"violations"`
	Diff       *modelc.DiffResult     `json:"diff,omitempty"`
}

// CheckModel 对模型做「规范 + 类型 + 变更影响」三重检查，不落库。
// 前端设计器每次保存前调用，把问题挡在发布之前。
func (s *Service) CheckModel(ctx context.Context, workspaceID string, spec model.ModelSpec) (*ModelCheckResult, error) {
	out := &ModelCheckResult{Valid: true}
	out.Violations = append(out.Violations, modelc.ValidateViolations(spec)...)
	out.Violations = append(out.Violations, s.Rules.Check(spec)...)

	engine := "postgres"
	if c, err := s.dialectFor(ctx, workspaceID, spec.Datasource); err == nil {
		engine = c
	}
	out.Violations = append(out.Violations, modelc.ValidateTypes(spec, engine)...)

	if old, err := s.Repo.PublishedModelSpec(ctx, workspaceID, spec.Name); err == nil {
		d := modelc.Diff(*old, spec)
		out.Diff = &d
	}
	out.Valid = len(out.Violations) == 0
	return out, nil
}

// dialectFor 返回数据源对应的引擎名，未知时退回 postgres 以保持行为可预测。
func (s *Service) dialectFor(ctx context.Context, workspaceID, dsName string) (string, error) {
	ds, _, err := s.Repo.DatasourceByName(ctx, workspaceID, dsName)
	if err != nil {
		return "", err
	}
	if ds.Type == "" {
		return "postgres", nil
	}
	return strings.ToLower(ds.Type), nil
}

// ScaffoldModel 从上游表结构生成模型骨架。
func (s *Service) ScaffoldModel(ctx context.Context, workspaceID string, opt modelc.ScaffoldOptions) (*modelc.ScaffoldResult, error) {
	if opt.Datasource == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "必须指定 datasource")
	}
	if opt.Engine == "" {
		if eng, err := s.dialectFor(ctx, workspaceID, opt.Datasource); err == nil {
			opt.Engine = eng
		}
	}
	// 上游没给列时，从元数据 catalog 里找同名表，省去用户手填。
	if len(opt.Upstream) == 0 && opt.SourceName != "" {
		cols, err := s.Repo.DatasetColumnsByName(ctx, workspaceID, opt.SourceName)
		if err == nil && len(cols) > 0 {
			opt.Upstream = cols
		}
	}
	res, err := modelc.Scaffold(opt)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "生成模型脚手架失败")
	}
	return &res, nil
}

// ModelDiff 返回当前发布版本与给定定义的差异，并附迁移方案。
func (s *Service) ModelDiff(ctx context.Context, workspaceID string, spec model.ModelSpec) (map[string]any, error) {
	old, err := s.Repo.PublishedModelSpec(ctx, workspaceID, spec.Name)
	if err != nil {
		return nil, err
	}
	engine, _ := s.dialectFor(ctx, workspaceID, spec.Datasource)
	if engine == "" {
		engine = "postgres"
	}
	d, ok := dialect.Get(engine)
	if !ok {
		d = dialect.Postgres{}
	}
	diff := modelc.Diff(*old, spec)
	plan := modelc.PlanMigration(*old, spec, d)
	return map[string]any{"diff": diff, "plan": plan, "engine": engine}, nil
}

// RollbackPreview 返回回滚到指定版本会发生什么，避免「一滚了之」。
func (s *Service) RollbackPreview(ctx context.Context, workspaceID, name string, version int) (*modelc.DiffResult, error) {
	cur, err := s.Repo.PublishedModelSpec(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	versions, err := s.Repo.ModelVersions(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.Version == version {
			d := modelc.Diff(*cur, v.Spec)
			return &d, nil
		}
	}
	return nil, errs.New(errs.CodeNotFound, "指定版本不存在")
}

// Dictionary 生成数据字典（含上下游与脱敏标记）。
func (s *Service) Dictionary(ctx context.Context, workspaceID string) ([]modelc.DictEntry, error) {
	models, err := s.Repo.ListModels(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	specs := make([]model.ModelSpec, 0, len(models))
	for _, m := range models {
		if m.Spec != nil {
			specs = append(specs, *m.Spec)
		}
	}

	edges, err := s.Repo.ListLineage(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	up := map[string][]string{}
	down := map[string][]string{}
	for _, e := range edges {
		if strings.HasPrefix(e.Upstream, "datasource.") {
			continue
		}
		up[e.Downstream] = append(up[e.Downstream], e.Upstream)
		down[e.Upstream] = append(down[e.Upstream], e.Downstream)
	}
	for k := range up {
		up[k] = dedupeStrings(up[k])
	}
	for k := range down {
		down[k] = dedupeStrings(down[k])
	}

	policies, err := s.Repo.AllColumnPolicies(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	pol := map[string]map[string]model.ColumnPolicy{}
	for _, p := range policies {
		if pol[p.TableName] == nil {
			pol[p.TableName] = map[string]model.ColumnPolicy{}
		}
		pol[p.TableName][p.ColumnName] = p
	}

	engine := "postgres"
	if len(specs) > 0 {
		if e, err := s.dialectFor(ctx, workspaceID, specs[0].Datasource); err == nil {
			engine = e
		}
	}
	return modelc.BuildDictionary(modelc.DictInput{
		Specs: specs, Upstream: up, Downstream: down, Policies: pol, Engine: engine,
	}), nil
}

// DictionaryMarkdown 导出可直接入库的 Markdown 字典。
func (s *Service) DictionaryMarkdown(ctx context.Context, workspaceID string) (string, error) {
	entries, err := s.Dictionary(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	return modelc.DictionaryMarkdown(entries), nil
}

// LayerRules 返回当前生效的规范，供设置页展示。
func (s *Service) LayerRules() map[string]any {
	r := s.Rules
	if len(r.Layers) == 0 {
		r = modelc.DefaultRules()
	}
	out := map[string]any{}
	for k, v := range r.Layers {
		out[k] = v
	}
	return map[string]any{"layers": out, "immutableRule": "reverse_dependency"}
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
