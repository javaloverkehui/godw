package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godw/godw/internal/auth"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/obs/logstore"
	"github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/pipeline/node"
	"github.com/godw/godw/internal/pkg/cryptox"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/quality"
	"github.com/godw/godw/internal/scheduler/queue"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/sqlx"
	"gopkg.in/yaml.v3"
)

// Service 承载全部业务用例。HTTP handler 只负责编解码与鉴权，
// 所有「先校验再落库、先编译再执行」的规则都收敛在这里。
type Service struct {
	Repo   *repo.Repo
	Pool   *connector.Pool
	Cipher *cryptox.Cipher
	Signer *auth.Signer
	Cfg    config.Config
	Log    *slog.Logger
	Exec   *node.Executor
	// CacheTTL 是查询结果缓存时长，0 表示不缓存。
	CacheTTL time.Duration
	// Rules 是建模规范，零值时使用内置默认值。
	Rules modelc.Rules
	// ResourceGroups 是资源组配置，key 为组名。
	ResourceGroups map[string]queue.Group
	// Logs_ 是运行日志管理器，懒加载。
	Logs_ *logstore.Manager
	mu    sync.Mutex
}

// 编译期保证 Service 满足执行器的解耦接口。
var _ node.Resolver = (*Service)(nil)

func New(db *repo.DB, cfg config.Config, log *slog.Logger) (*Service, error) {
	cipher, err := cryptox.New(cfg.Auth.JWTSecret)
	if err != nil {
		return nil, err
	}
	pool := connector.NewPool(10 * time.Minute)
	svc := &Service{
		Repo:     repo.New(db),
		Pool:     pool,
		Cipher:   cipher,
		Signer:   auth.NewSigner(cfg.Auth.JWTSecret, time.Duration(cfg.Auth.TokenTTLMin)*time.Minute),
		Cfg:      cfg,
		Log:      log,
		CacheTTL: time.Duration(cfg.Semantic.CacheTTLSec) * time.Second,
		Rules:    modelc.DefaultRules(),
		ResourceGroups: map[string]queue.Group{
			"default": queue.DefaultGroup(),
			"heavy":   {Name: "heavy", MaxConcurrency: 1, MaxTimeoutSec: 7200, MaxPool: 20},
		},
	}
	svc.Exec = &node.Executor{
		Pool:     pool,
		Resolver: svc,
		WorkDir:  cfg.Executor.WorkDir,
		MaxRows:  cfg.Executor.MaxRows,
	}
	return svc, nil
}

// Bootstrap 初始化默认工作空间与管理员。重复执行安全。
func (s *Service) Bootstrap(ctx context.Context) (*model.Workspace, error) {
	ws, err := s.Repo.EnsureWorkspace(ctx, "default")
	if err != nil {
		return nil, err
	}
	if s.Cfg.Auth.BootstrapAdmin != "" && s.Cfg.Auth.BootstrapPass != "" {
		if _, err := s.Repo.UserByEmail(ctx, ws.ID, s.Cfg.Auth.BootstrapAdmin); err != nil {
			hash, herr := auth.HashPassword(s.Cfg.Auth.BootstrapPass)
			if herr != nil {
				return nil, herr
			}
			if cerr := s.Repo.CreateUser(ctx, model.User{
				ID: ids.New("usr"), WorkspaceID: ws.ID,
				Email: s.Cfg.Auth.BootstrapAdmin, DisplayName: "admin",
				PasswordHash: hash, Role: "admin",
			}); cerr != nil {
				return nil, cerr
			}
			s.Log.Info("已创建初始管理员", "email", s.Cfg.Auth.BootstrapAdmin)
		}
	}
	return ws, nil
}

// ---------- datasource ----------

// CreateDatasource 校验连通性后再落库。连不上的数据源不入库，
// 避免留下无法使用的配置。
func (s *Service) CreateDatasource(ctx context.Context, workspaceID, name, typ string, conf map[string]any, secret map[string]string) (*model.Datasource, error) {
	if name == "" || typ == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "name 与 type 不能为空")
	}
	spec := connector.Spec{ID: "probe", Name: name, Type: typ, Config: conf, Secret: secret}
	c, err := connector.Open(ctx, spec)
	if err != nil {
		return nil, err
	}
	_ = c.Close()

	enc, err := s.Cipher.Encrypt(secret)
	if err != nil {
		return nil, err
	}
	ws, err := s.Repo.EnsureWorkspace(ctx, "default")
	if err != nil {
		return nil, err
	}
	ds, err := s.Repo.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: workspaceID, Name: name, Type: typ, Config: conf,
	}, enc)
	if err != nil {
		return nil, err
	}
	if err := s.Repo.UpdateDatasourceStatus(ctx, ds.ID, "ok", ""); err != nil {
		s.Log.Warn("更新数据源状态失败", "err", err, "workspace", ws.Name)
	}
	return ds, nil
}

// TestDatasource 重新探测并记录状态。
func (s *Service) TestDatasource(ctx context.Context, workspaceID, name string) error {
	spec, err := s.DatasourceSpec(ctx, workspaceID, name)
	if err != nil {
		return err
	}
	s.Pool.Invalidate(spec.ID)
	c, err := s.Pool.Acquire(ctx, spec)
	if err != nil {
		ds, _, _ := s.Repo.DatasourceByName(ctx, workspaceID, name)
		if ds != nil {
			_ = s.Repo.UpdateDatasourceStatus(ctx, ds.ID, "error", err.Error())
		}
		return err
	}
	if err := c.Test(ctx); err != nil {
		_ = s.Repo.UpdateDatasourceStatus(ctx, spec.ID, "error", err.Error())
		return errs.Wrap(errs.CodeDatasourceUnreachable, err, "连接测试失败")
	}
	return s.Repo.UpdateDatasourceStatus(ctx, spec.ID, "ok", "")
}

// CollectMetadata 采集库表结构与统计信息。逐表采集，单表失败不影响其他表。
func (s *Service) CollectMetadata(ctx context.Context, workspaceID, name string) (int, error) {
	spec, err := s.DatasourceSpec(ctx, workspaceID, name)
	if err != nil {
		return 0, err
	}
	c, err := s.Pool.Acquire(ctx, spec)
	if err != nil {
		return 0, err
	}

	schemas, err := c.ListSchemas(ctx)
	if err != nil {
		return 0, errs.Wrap(errs.CodeMetadataCollect, err, "列出 schema 失败")
	}
	total := 0
	var failures []string
	for _, schema := range schemas {
		tables, err := c.ListTables(ctx, schema)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", schema, err))
			continue
		}
		for _, t := range tables {
			full, derr := c.DescribeTable(ctx, schema, t.Name)
			if derr != nil {
				failures = append(failures, fmt.Sprintf("%s.%s: %v", schema, t.Name, derr))
				continue
			}
			// 源表主键必须跟着列一起落库。丢了它，脚手架就只能靠「列名像 id」猜，
			// 用户在源表里已经写清楚的约束会被要求重新回答一遍。
			pk := make(map[string]bool, len(full.PrimaryKey))
			for _, pkCol := range full.PrimaryKey {
				pk[strings.ToLower(pkCol)] = true
			}
			cols := make([]model.ColumnSpec, len(full.Columns))
			for i, col := range full.Columns {
				cols[i] = model.ColumnSpec{
					Name: col.Name, Type: col.Type, Nullable: col.Nullable, Comment: col.Comment,
					PrimaryKey: pk[strings.ToLower(col.Name)],
				}
			}
			full.RowCount = t.RowCount
			full.SizeBytes = t.SizeBytes
			full.Type = t.Type
			full.Comment = t.Comment
			if err := s.Repo.UpsertDataset(ctx, model.Dataset{
				DatasourceID: spec.ID, Schema: schema, Name: t.Name, Type: t.Type,
				Comment: t.Comment, RowCount: t.RowCount, SizeBytes: t.SizeBytes, Columns: cols,
			}, workspaceID); err != nil {
				failures = append(failures, fmt.Sprintf("%s.%s: %v", schema, t.Name, err))
				continue
			}
			total++
		}
	}
	if len(failures) > 0 {
		s.Log.Warn("部分表采集失败", "count", len(failures), "sample", failures[0])
	}
	return total, nil
}

// ---------- model ----------

// ApplyModel 解析 YAML → 校验 → 编译 DDL → 落库（可选发布与执行 DDL）。
func (s *Service) ApplyModel(ctx context.Context, workspaceID, actor string, raw []byte, publish, applyDDL, force bool) (*model.Model, error) {
	var spec model.ModelSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, errs.Wrap(errs.CodeModelInvalid, err, "模型 YAML 解析失败")
	}
	return s.ApplyModelSpec(ctx, workspaceID, actor, spec, publish, applyDDL, force)
}

func (s *Service) ApplyModelSpec(ctx context.Context, workspaceID, actor string, spec model.ModelSpec, publish, applyDDL, force bool) (*model.Model, error) {
	if err := modelc.Validate(spec); err != nil {
		return nil, err
	}

	ds, _, err := s.Repo.DatasourceByName(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return nil, err
	}
	spec2, err := s.DatasourceSpec(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return nil, err
	}
	c, err := s.Pool.Acquire(ctx, spec2)
	if err != nil {
		return nil, err
	}

	// 建模规范：分层前缀、必填列、粒度声明。违规直接拒绝，不做「先存后改」。
	if vs := s.Rules.Check(spec); len(vs) > 0 {
		return nil, modelc.Enforce(vs)
	}
	// 类型映射：引擎不支持或有损未声明时拒绝，避免上线后 DDL 跑不通。
	if vs := modelc.ValidateTypes(spec, c.Dialect().Name()); len(vs) > 0 {
		return nil, modelc.Enforce(vs)
	}

	// 与已发布版本对比：breaking 变更必须带 force，且留下操作记录。
	var diff modelc.DiffResult
	if old, oerr := s.Repo.PublishedModelSpec(ctx, workspaceID, spec.Name); oerr == nil {
		diff = modelc.Diff(*old, spec)
		if diff.HasBreaking() && !force {
			return nil, errs.New(errs.CodeModelImmutable,
				"检测到破坏性结构变更，需确认后重试").
				WithMeta("breaking", diff.Breaking).
				WithMeta("changes", diff.Changes)
		}
	}

	ddl, err := modelc.Compile(spec, c.Dialect())
	if err != nil {
		return nil, err
	}

	if applyDDL {
		statements := splitDDL(ddl)
		for _, stmt := range statements {
			if _, err := c.Exec(ctx, stmt); err != nil {
				return nil, errs.Wrap(errs.CodeExecutionFailed, err, "执行建表 DDL 失败").
					WithMeta("sql", stmt)
			}
		}
	}

	m, err := s.Repo.SaveModel(ctx, workspaceID, spec, ds.ID, ddl, actor, publish)
	if err != nil {
		return nil, err
	}
	if publish {
		// 血缘：模型依赖其数据源。下游名统一用模型名，
		// 不用 layer.name，否则同一张表会出现两个节点，图会重复。
		if err := s.Repo.AddLineage(ctx, workspaceID, spec.Name, []string{"datasource." + spec.Datasource}); err != nil {
			s.Log.Warn("写入血缘失败", "err", err)
		}
	}
	return m, nil
}

func (s *Service) PreviewModelDDL(ctx context.Context, workspaceID string, spec model.ModelSpec) (string, error) {
	if err := modelc.Validate(spec); err != nil {
		return "", err
	}
	spec2, err := s.DatasourceSpec(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return "", err
	}
	c, err := s.Pool.Acquire(ctx, spec2)
	if err != nil {
		return "", err
	}
	return modelc.Compile(spec, c.Dialect())
}

// ---------- pipeline ----------

func (s *Service) ApplyPipeline(ctx context.Context, workspaceID, actor string, raw []byte, publish bool) (*model.Pipeline, error) {
	spec, err := compile.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := compile.Validate(spec); err != nil {
		return nil, err
	}
	// 提交即校验依赖的模型已发布，避免上线后才发现缺表。
	for _, n := range spec.Nodes {
		if n.Model == "" || n.Type == model.NodeQuality {
			continue
		}
		if _, err := s.Repo.PublishedModelSpec(ctx, workspaceID, n.Model); err != nil {
			return nil, errs.New(errs.CodePipelineInvalid, "节点 "+n.ID+" 引用的模型未发布: "+n.Model)
		}
	}
	p, err := s.Repo.SavePipeline(ctx, workspaceID, *spec, actor, publish)
	if err != nil {
		return nil, err
	}
	if publish {
		s.recordLineage(ctx, workspaceID, *spec)
		s.RecordColumnLineage(ctx, workspaceID, *spec)
	}
	return p, nil
}

func (s *Service) recordLineage(ctx context.Context, workspaceID string, spec model.PipelineSpec) {
	byID := map[string]model.NodeSpec{}
	for _, n := range spec.Nodes {
		byID[n.ID] = n
	}
	for _, n := range spec.Nodes {
		if n.Model == "" {
			continue
		}
		var ups []string
		for _, dep := range n.DependsOn {
			if up, ok := byID[dep]; ok && up.Model != "" {
				ups = append(ups, up.Model)
			}
		}
		if len(ups) == 0 {
			continue
		}
		if err := s.Repo.ReplaceLineage(ctx, workspaceID, n.Model, ups); err != nil {
			s.Log.Warn("写入血缘失败", "err", err, "model", n.Model)
		}
	}
}

// ---------- resolver ----------

func (s *Service) DatasourceSpec(ctx context.Context, workspaceID, name string) (connector.Spec, error) {
	ds, enc, err := s.Repo.DatasourceByName(ctx, workspaceID, name)
	if err != nil {
		return connector.Spec{}, err
	}
	secret := map[string]string{}
	if err := s.Cipher.Decrypt(enc, &secret); err != nil {
		return connector.Spec{}, errs.Wrap(errs.CodeInternal, err, "数据源凭证解密失败")
	}
	return connector.Spec{ID: ds.ID, Name: ds.Name, Type: ds.Type, Config: ds.Config, Secret: secret}, nil
}

func (s *Service) ModelSpec(ctx context.Context, workspaceID, name string) (*model.ModelSpec, error) {
	return s.Repo.PublishedModelSpec(ctx, workspaceID, name)
}

// ---------- helpers ----------

func splitDDL(ddl string) []string {
	// 编译器生成的 DDL 每句以分号结尾且不含函数体，简单切分即可。
	parts := strings.Split(ddl, ";\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RenderVars 构造执行变量：业务日期是必需项，其余来自实例参数。
func RenderVars(bizDate string, params map[string]any) map[string]any {
	vars := map[string]any{"biz_date": bizDate, "biz_ts": bizDate + " 00:00:00"}
	for k, v := range params {
		vars[k] = v
	}
	// 分区列常与业务日期同名，补一份避免模型里写 ${dt} 时缺值。
	if _, ok := vars["dt"]; !ok {
		vars["dt"] = bizDate
	}
	return vars
}

var _ = sqlx.Fingerprint
var _ = quality.Gate

// ---------- metric ----------

// ApplyMetric 保存指标定义。指标只能引用已发布模型，避免指向不存在的表。
func (s *Service) ApplyMetric(ctx context.Context, workspaceID string, m semantic.Metric) (*semantic.Metric, error) {
	if err := semantic.Validate(m); err != nil {
		return nil, err
	}
	if m.TimeColumn == "" {
		m.TimeColumn = "dt"
	}
	if m.DisplayName == "" {
		m.DisplayName = m.Name
	}
	if _, err := s.Repo.PublishedModelSpec(ctx, workspaceID, m.ModelName); err != nil {
		return nil, errs.New(errs.CodeInvalidArgument, "指标引用的模型未发布: "+m.ModelName)
	}
	return s.Repo.SaveMetric(ctx, workspaceID, m)
}

// QueryMetric 解析指标绑定的模型，在其数据源上执行聚合并返回序列。
func (s *Service) QueryMetric(ctx context.Context, workspaceID, name string, q semantic.Query) (*semantic.Series, error) {
	return s.QueryMetricFor(ctx, workspaceID, name, q, "", "")
}

// QueryMetricFor 在 QueryMetric 基础上注入行级权限并启用结果缓存。
//
// 顺序很重要：先拼行过滤再算缓存键，否则不同用户会命中同一份缓存，
// 造成越权读取。cache 键里带上 user 与 role，宁可降低命中率也不能泄漏。
func (s *Service) QueryMetricFor(ctx context.Context, workspaceID, name string, q semantic.Query, user, role string) (*semantic.Series, error) {
	m, err := s.Repo.MetricByName(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, m.ModelName)
	if err != nil {
		return nil, err
	}
	if role != "" {
		filters, ferr := s.Repo.RowFiltersFor(ctx, workspaceID, m.ModelName, role)
		if ferr == nil && len(filters) > 0 {
			if cond := ApplyRowFilters(filters, user, role); cond != "" {
				q.ExtraWhere = append(q.ExtraWhere, cond)
			}
		}
	}

	key := cacheKey("metric", workspaceID, name, q, user, role)
	if s.CacheTTL > 0 {
		if raw, hit, err := s.Repo.CacheGet(ctx, key); err == nil && hit {
			var cached semantic.Series
			if json.Unmarshal(raw, &cached) == nil {
				cached.Cached = true
				return &cached, nil
			}
		}
	}

	ds, err := s.DatasourceSpec(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return nil, err
	}
	c, err := s.Pool.Acquire(ctx, ds)
	if err != nil {
		return nil, err
	}
	series, err := semantic.Run(ctx, c, *m, *spec, q)
	if err != nil {
		return nil, err
	}
	if s.CacheTTL > 0 {
		if raw, err := json.Marshal(series); err == nil {
			_ = s.Repo.CachePut(ctx, key, workspaceID, raw, s.CacheTTL)
		}
	}
	return series, nil
}

func cacheKey(kind, workspaceID, name string, q semantic.Query, user, role string) string {
	// 用结构体序列化后哈希，避免手工拼字符串漏字段。
	payload, _ := json.Marshal(struct {
		Kind, WS, Name, User, Role string
		Q                          semantic.Query
	}{kind, workspaceID, name, user, role, q})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ---------- dashboard ----------

func (s *Service) ApplyDashboard(ctx context.Context, workspaceID string, d semantic.Dashboard) (*semantic.Dashboard, error) {
	if err := semantic.ValidateDashboard(d); err != nil {
		return nil, err
	}
	for _, c := range d.Cards {
		// 二选一已在 ValidateDashboard 里校验过，这里只确认被引用的对象真的存在。
		if c.Chart != "" {
			if _, err := s.Repo.GetChart(ctx, workspaceID, c.Chart); err != nil {
				return nil, errs.New(errs.CodeInvalidArgument, "卡片引用了不存在的图表: "+c.Chart)
			}
			continue
		}
		if _, err := s.Repo.MetricByName(ctx, workspaceID, c.Metric); err != nil {
			return nil, errs.New(errs.CodeInvalidArgument, "卡片引用了不存在的指标: "+c.Metric)
		}
	}
	return s.Repo.SaveDashboard(ctx, workspaceID, d)
}

// QueryDashboard 一次性返回看板所有卡片的数据，减少前端请求数。
func (s *Service) QueryDashboard(ctx context.Context, workspaceID, name string) (*semantic.DashboardData, error) {
	return s.QueryDashboardFor(ctx, workspaceID, name, "", "", semantic.RunOptions{})
}

// QueryDashboardWith 支持看板级全局筛选（时间窗口 / 维度切片）。
// 分享、导出、订阅走上面的三参版本：它们要的是看板定义时的原貌，不该被页面筛选影响。
func (s *Service) QueryDashboardWith(ctx context.Context, workspaceID, name string, opt semantic.RunOptions) (*semantic.DashboardData, error) {
	return s.QueryDashboardFor(ctx, workspaceID, name, "", "", opt)
}

// QueryDashboardFor 额外带上访问者身份：图表卡要走数据集查询，而行级/列级策略按用户生效，
// 不传身份等于给图表卡开了一个绕过 RLS 的口子。
func (s *Service) QueryDashboardFor(ctx context.Context, workspaceID, name, user, role string, opt semantic.RunOptions) (*semantic.DashboardData, error) {
	d, err := s.Repo.DashboardByName(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	resolve := func(metricName string) (semantic.Metric, model.ModelSpec, connector.Connector, error) {
		m, err := s.Repo.MetricByName(ctx, workspaceID, metricName)
		if err != nil {
			return semantic.Metric{}, model.ModelSpec{}, nil, err
		}
		spec, err := s.Repo.PublishedModelSpec(ctx, workspaceID, m.ModelName)
		if err != nil {
			return semantic.Metric{}, model.ModelSpec{}, nil, err
		}
		ds, err := s.DatasourceSpec(ctx, workspaceID, spec.Datasource)
		if err != nil {
			return semantic.Metric{}, model.ModelSpec{}, nil, err
		}
		c, err := s.Pool.Acquire(ctx, ds)
		if err != nil {
			return semantic.Metric{}, model.ModelSpec{}, nil, err
		}
		return *m, *spec, c, nil
	}
	resolveChart := func(chartName string, o semantic.RunOptions) (*semantic.CardChart, error) {
		extra := s.chartCardFilters(ctx, workspaceID, chartName, o)
		res, err := s.ChartQueryWith(ctx, workspaceID, chartName, user, role, extra)
		if err != nil {
			return nil, err
		}
		return &semantic.CardChart{Type: res.Chart.Type, Series: res.Series, Points: res.Points}, nil
	}
	data := semantic.RunCards(ctx, resolve, resolveChart, *d, opt)
	return &data, nil
}

// chartCardFilters 把看板的全局时间窗口翻译成图表查询条件。
// 只在数据集存在 filterable 的 time 维度时生效——宁可不筛，也不要因为
// 一条时间条件把整张图变成报错（未开启筛选的维度会被 BuildQuery 拒绝）。
func (s *Service) chartCardFilters(ctx context.Context, workspaceID, chartName string, opt semantic.RunOptions) []semantic.Filter {
	if opt.Days <= 0 {
		return nil
	}
	c, err := s.Repo.GetChart(ctx, workspaceID, chartName)
	if err != nil {
		return nil
	}
	d, err := s.Repo.GetDataset(ctx, workspaceID, c.Dataset)
	if err != nil {
		return nil
	}
	for _, dim := range d.Dimensions {
		if dim.Type == "time" && dim.Filterable {
			return []semantic.Filter{{
				Field: dim.Name,
				Op:    ">=",
				Value: time.Now().AddDate(0, 0, -opt.Days).Format("2006-01-02"),
			}}
		}
	}
	return nil
}
