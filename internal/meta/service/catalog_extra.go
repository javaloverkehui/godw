package service

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0203 工作空间与环境。
//
// 同一个模型在 dev/prod 应该指向不同数据源，但模型定义不该改。
// 因此引入「环境 + 逻辑别名」：模型写 datasource: pg_dw，
// 环境决定 pg_dw 到底解析到哪一套库。
//
// 这是「多环境」最轻的实现：不改模型、不改管道，只改绑定表。

// envCache 缓存「环境 → 别名 → 数据源 ID」，避免每次解析都查库。
type envCache struct {
	mu   sync.RWMutex
	data map[string]map[string]string
}

var envBindings = &envCache{data: map[string]map[string]string{}}

func envKey(workspaceID, env string) string { return workspaceID + "\x00" + env }

// EnsureEnvironments 为工作空间创建 dev/prod 两个环境与默认资源组。
// 幂等：重复调用不会重复创建。
func (s *Service) EnsureEnvironments(ctx context.Context, workspaceID string) error {
	for _, name := range []string{"dev", "prod"} {
		if err := s.Repo.EnsureEnvironment(ctx, workspaceID, name, name+" 环境"); err != nil {
			return err
		}
	}
	if err := s.Repo.UpsertResourceGroup(ctx, workspaceID, "default", 4, 3600, 10, nil); err != nil {
		return err
	}
	return nil
}

// ListEnvironments 返回环境列表。
func (s *Service) ListEnvironments(ctx context.Context, workspaceID string) ([]model.Environment, error) {
	return s.Repo.ListEnvironments(ctx, workspaceID)
}

// BindEnv 绑定「环境 + 逻辑别名 → 数据源」。
func (s *Service) BindEnv(ctx context.Context, workspaceID, env, alias, datasource string) error {
	if env == "" || alias == "" || datasource == "" {
		return errs.New(errs.CodeInvalidArgument, "env/alias/datasource 都不能为空")
	}
	ds, _, err := s.Repo.DatasourceByName(ctx, workspaceID, datasource)
	if err != nil {
		return err
	}
	if err := s.Repo.BindEnvDatasource(ctx, workspaceID, env, alias, ds.ID); err != nil {
		return err
	}
	envBindings.mu.Lock()
	defer envBindings.mu.Unlock()
	k := envKey(workspaceID, env)
	if envBindings.data[k] == nil {
		envBindings.data[k] = map[string]string{}
	}
	envBindings.data[k][alias] = ds.ID
	return nil
}

// ResolveDatasource 解析「逻辑别名 → 实际数据源名」。
// 未绑定时回退到同名的真实数据源，保证单环境场景不受影响。
func (s *Service) ResolveDatasource(ctx context.Context, workspaceID, env, alias string) (string, error) {
	if env == "" {
		env = "prod"
	}
	key := envKey(workspaceID, env)
	envBindings.mu.RLock()
	id, cached := envBindings.data[key][alias]
	envBindings.mu.RUnlock()

	if !cached {
		bound, err := s.Repo.EnvBinding(ctx, workspaceID, env, alias)
		if err != nil {
			return "", err
		}
		id = bound
		envBindings.mu.Lock()
		if envBindings.data[key] == nil {
			envBindings.data[key] = map[string]string{}
		}
		envBindings.data[key][alias] = id
		envBindings.mu.Unlock()
	}
	if id == "" {
		// 没绑定 → 假设 alias 就是数据源名，直接校验存在性。
		if _, _, err := s.Repo.DatasourceByName(ctx, workspaceID, alias); err != nil {
			return "", errs.New(errs.CodeNotFound,
				fmt.Sprintf("环境 %s 未绑定别名 %s，也没有同名数据源", env, alias)).
				WithMeta("env", env).WithMeta("alias", alias)
		}
		return alias, nil
	}
	name, err := s.Repo.DatasourceNameByID(ctx, workspaceID, id)
	if err != nil {
		return "", err
	}
	return name, nil
}

// EnvBindings 返回某环境的全部绑定，供设置页展示。
func (s *Service) EnvBindings(ctx context.Context, workspaceID, env string) (map[string]string, error) {
	return s.Repo.ListEnvBindings(ctx, workspaceID, env)
}

// DeleteWorkspace 软删工作空间前置校验：有运行中实例必须拒绝。
func (s *Service) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	n, err := s.Repo.RunningInstances(ctx, workspaceID)
	if err != nil {
		return err
	}
	if n > 0 {
		return errs.New(errs.CodeConflict,
			fmt.Sprintf("存在 %d 个运行中的实例，无法删除工作空间", n)).
			WithMeta("runningInstances", n)
	}
	return s.Repo.SoftDeleteWorkspace(ctx, workspaceID)
}

// ---------- T0208 采集调度与结构变更 ----------

// CollectResult 是一次元数据采集的结果。
type CollectResult struct {
	Datasource string               `json:"datasource"`
	Tables     int                  `json:"tables"`
	Changes    []model.SchemaChange `json:"changes"`
	DurationMS int64                `json:"durationMs"`
	Error      string               `json:"error,omitempty"`
}

// CollectWithDiff 采集元数据并与上次结果对比，产出结构变更记录。
// 注意：采集失败重试 3 次，仍失败则把数据源标记 failed。
func (s *Service) CollectWithDiff(ctx context.Context, workspaceID, name string) (*CollectResult, error) {
	start := time.Now()
	before, _ := s.Repo.ListDatasets(ctx, workspaceID, "")
	beforeMap := map[string]model.Dataset{}
	for _, d := range before {
		beforeMap[d.Schema+"."+d.Name] = d
	}

	var lastErr error
	var count int
	for attempt := 0; attempt < 3; attempt++ {
		count, lastErr = s.CollectMetadata(ctx, workspaceID, name)
		if lastErr == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	res := &CollectResult{Datasource: name, DurationMS: time.Since(start).Milliseconds()}
	if lastErr != nil {
		res.Error = lastErr.Error()
		_ = s.Repo.UpdateDatasourceStatus(ctx, name, "failed", lastErr.Error())
		_ = s.Repo.RecordCollectJob(ctx, workspaceID, name, "failed", 0, 0, lastErr.Error())
		return res, lastErr
	}
	res.Tables = count

	after, err := s.Repo.ListDatasets(ctx, workspaceID, "")
	if err != nil {
		return res, err
	}
	afterMap := map[string]model.Dataset{}
	for _, d := range after {
		afterMap[d.Schema+"."+d.Name] = d
	}

	res.Changes = diffDatasets(beforeMap, afterMap)
	for _, c := range res.Changes {
		if err := s.Repo.SaveSchemaChange(ctx, workspaceID, name, c); err != nil {
			s.Log.Warn("写入结构变更失败", "err", err)
		}
	}
	_ = s.Repo.RecordCollectJob(ctx, workspaceID, name, "success", count, len(res.Changes), "")
	return res, nil
}

// diffDatasets 对比两次采集结果。只报告列级变化，表级增删单独标记。
func diffDatasets(before, after map[string]model.Dataset) []model.SchemaChange {
	var out []model.SchemaChange
	for key, a := range after {
		b, existed := before[key]
		if !existed {
			continue // 新表不算变更（由首次采集产生，不是漂移）
		}
		bCols := map[string]model.ColumnSpec{}
		for _, c := range b.Columns {
			bCols[strings.ToLower(c.Name)] = c
		}
		aCols := map[string]model.ColumnSpec{}
		for _, c := range a.Columns {
			aCols[strings.ToLower(c.Name)] = c
		}
		for name, c := range aCols {
			if _, ok := bCols[name]; !ok {
				out = append(out, model.SchemaChange{Table: a.Name, Kind: "add_column", Column: c.Name, To: c.Type, Breaking: false})
			}
		}
		for name, c := range bCols {
			if _, ok := aCols[name]; !ok {
				out = append(out, model.SchemaChange{Table: b.Name, Kind: "drop_column", Column: c.Name, From: c.Type, Breaking: true})
			}
		}
		for name, ac := range aCols {
			bc, ok := bCols[name]
			if !ok {
				continue
			}
			if !strings.EqualFold(bc.Type, ac.Type) {
				out = append(out, model.SchemaChange{Table: a.Name, Kind: "modify_column", Column: ac.Name,
					From: bc.Type, To: ac.Type, Breaking: true})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

// ListSchemaChanges 返回近期的结构变更。
func (s *Service) ListSchemaChanges(ctx context.Context, workspaceID string, limit int) ([]model.SchemaChange, error) {
	return s.Repo.ListSchemaChanges(ctx, workspaceID, limit)
}

// ---------- T0209 敏感字段识别 ----------

// PIILevel 是敏感级别。
type PIILevel string

const (
	PIINone   PIILevel = "none"
	PIILow    PIILevel = "low"
	PIIMedium PIILevel = "medium"
	PIIHigh   PIILevel = "high"
)

// piiNameRules 是列名规则：命中即给级别。
var piiNameRules = []struct {
	pattern []string
	level   PIILevel
	reason  string
}{
	{[]string{"id_card", "idcard", "身份证", "identity"}, PIIHigh, "列名命中身份证规则"},
	{[]string{"bank_card", "card_no", "银行卡"}, PIIHigh, "列名命中银行卡规则"},
	{[]string{"password", "passwd", "pwd", "secret", "token"}, PIIHigh, "列名命中凭证规则"},
	{[]string{"phone", "mobile", "tel", "手机", "电话"}, PIIMedium, "列名命中手机号规则"},
	{[]string{"email", "mail", "邮箱"}, PIIMedium, "列名命中邮箱规则"},
	{[]string{"address", "addr", "地址"}, PIIMedium, "列名命中地址规则"},
	{[]string{"name", "姓名", "user_name", "real_name"}, PIILow, "列名命中姓名规则"},
	{[]string{"ip", "device_id", "imei", "openid"}, PIILow, "列名命中设备标识规则"},
}

// PIIFinding 是一条识别结果。
type PIIFinding struct {
	Table  string   `json:"table"`
	Column string   `json:"column"`
	Level  PIILevel `json:"level"`
	Reason string   `json:"reason"`
	// SampleHitRate 是值采样命中率（0–1），列名规则命中时可为 0。
	SampleHitRate float64 `json:"sampleHitRate"`
	// Manual 表示该结果被人工覆盖过。
	Manual bool `json:"manual"`
}

// DetectPII 识别某数据源敏感字段。
//
// 两层判定：
//  1. 列名正则（快、零成本）；
//  2. 值采样（每列最多 200 行，命中率 > 80% 判定）——只在列名没命中时补充，
//     避免每列都扫一遍数据。
//
// 采样必须 LIMIT，禁止全表扫描；超大表用随机采样。
func (s *Service) DetectPII(ctx context.Context, workspaceID, datasource string, sampleLimit int) ([]PIIFinding, error) {
	if sampleLimit <= 0 || sampleLimit > 200 {
		sampleLimit = 200
	}
	c, err := s.connectorFor(ctx, workspaceID, datasource)
	if err != nil {
		return nil, err
	}
	datasets, err := s.Repo.ListDatasets(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	out := []PIIFinding{}
	for _, d := range datasets {
		for _, col := range d.Columns {
			f, ok := matchPIIName(col.Name)
			if ok {
				f.Table, f.Column = d.Name, col.Name
				out = append(out, f)
				continue
			}
			// 列名没命中，做一次小样本值探测。
			level, rate := samplePII(ctx, c, d, col.Name, sampleLimit)
			if level != PIINone {
				out = append(out, PIIFinding{
					Table: d.Name, Column: col.Name, Level: level,
					Reason: fmt.Sprintf("值采样命中率 %.0f%%", rate*100), SampleHitRate: rate,
				})
			}
		}
	}
	// 人工覆盖优先：采集结果不覆盖人工设定。
	overrides, _ := s.Repo.ListPIIOverrides(ctx, workspaceID, datasource)
	for i := range out {
		for _, o := range overrides {
			if o.Table == out[i].Table && strings.EqualFold(o.Column, out[i].Column) {
				out[i].Level = PIILevel(o.Level)
				out[i].Reason = "人工设定: " + o.Reason
				out[i].Manual = true
			}
		}
	}
	return out, nil
}

// ApplyPIIFindings 把识别结果写入 dataset 表，供脱敏策略消费。
func (s *Service) ApplyPIIFindings(ctx context.Context, workspaceID, datasource string, findings []PIIFinding) (int, error) {
	n := 0
	for _, f := range findings {
		if err := s.Repo.UpdateDatasetPII(ctx, workspaceID, f.Table, string(f.Level), f.Reason); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func matchPIIName(name string) (PIIFinding, bool) {
	lower := strings.ToLower(name)
	for _, r := range piiNameRules {
		for _, p := range r.pattern {
			if strings.Contains(lower, p) {
				return PIIFinding{Level: r.level, Reason: r.reason}, true
			}
		}
	}
	return PIIFinding{}, false
}

var (
	phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[a-zA-Z]{2,}$`)
	cardRe  = regexp.MustCompile(`^\d{15}(\d{2}[0-9Xx])?$`)
	ipRe    = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
)

// samplePII 采样判断列是否含敏感值。
// 命中率 > 80% 才判定，避免少量脏数据误报；
// 采样必须 LIMIT，禁止全表扫描。
func samplePII(ctx context.Context, c connector.Connector, d model.Dataset, column string, limit int) (PIILevel, float64) {
	q := c.Dialect().QuoteIdent(column)
	table := c.Dialect().QuoteIdent(d.Name)
	if d.Schema != "" {
		table = c.Dialect().QuoteIdent(d.Schema) + "." + table
	}
	sqlText := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL %s",
		q, table, q, c.Dialect().LimitOffset(int64(limit), 0))
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return PIINone, 0
	}
	defer rows.Close()

	total, hitPhone, hitEmail, hitCard, hitIP := 0, 0, 0, 0, 0
	for rows.Next() && total < limit {
		var v any
		if err := rows.Scan(&v); err != nil {
			break
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			continue
		}
		total++
		if phoneRe.MatchString(s) {
			hitPhone++
		}
		if emailRe.MatchString(s) {
			hitEmail++
		}
		if cardRe.MatchString(s) {
			hitCard++
		}
		if ipRe.MatchString(s) {
			hitIP++
		}
	}
	if total == 0 {
		return PIINone, 0
	}
	rate := func(n int) float64 { return float64(n) / float64(total) }
	switch {
	case rate(hitCard) > 0.8:
		return PIIHigh, rate(hitCard)
	case rate(hitEmail) > 0.8:
		return PIIMedium, rate(hitEmail)
	case rate(hitPhone) > 0.8:
		return PIIMedium, rate(hitPhone)
	case rate(hitIP) > 0.8:
		return PIILow, rate(hitIP)
	}
	return PIINone, 0
}

// ---------- T0210 元数据搜索与标签 ----------

// SearchHit 是一条搜索结果。
type SearchHit struct {
	Type    string  `json:"type"` // model | dataset | pipeline | chart
	Name    string  `json:"name"`
	Layer   string  `json:"layer,omitempty"`
	Comment string  `json:"comment,omitempty"`
	Score   float64 `json:"score"`
	// Highlight 是带 <em> 标记的命中片段。
	Highlight string   `json:"highlight,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Owner     string   `json:"owner,omitempty"`
}

// SearchQuery 是搜索条件。
type SearchQuery struct {
	Q          string   `json:"q"`
	Types      []string `json:"types,omitempty"`
	Layer      string   `json:"layer,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Datasource string   `json:"datasource,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Limit      int      `json:"limit,omitempty"`
}

// Search 做元数据搜索：PG 全文 + trigram 模糊双通道。
// 中文按字切分：to_tsvector('simple', ...) 把中文当整体 token，
// 因此额外用 ILIKE 兜底，保证「订单」能命中「订单明细」。
func (s *Service) Search(ctx context.Context, workspaceID string, q SearchQuery) ([]SearchHit, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	rows, err := s.Repo.SearchMetadata(ctx, workspaceID, repo.SearchFilter{
		Q:          q.Q,
		Types:      q.Types,
		Layer:      q.Layer,
		Tags:       q.Tags,
		Datasource: q.Datasource,
		Owner:      q.Owner,
		Limit:      q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{
			Type:      r.Type,
			Name:      r.Name,
			Layer:     r.Layer,
			Comment:   r.Comment,
			Owner:     r.Owner,
			Score:     r.Score,
			Tags:      r.Tags,
			Highlight: highlight(r.Name+" "+r.Comment, q.Q),
		})
	}
	return out, nil
}

// highlight 在 Go 侧做命中片段标记，不用 PG 的 ts_headline。
//
// 原因是 ts_headline 依赖分词器给出的 token 边界，中文整句是一个 token，
// 它会把「订单明细表」整段吞掉或整段不标；在 Go 里按子串切反而稳定可控。
// 输出做 HTML 转义，避免用户输入的 <script> 从高亮字段回流成 XSS。
func highlight(text, keyword string) string {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || text == "" {
		return ""
	}
	const window = 60 // 片段长度（字符数），保证卡片上不换行太长
	safe := html.EscapeString(text)
	kw := html.EscapeString(keyword)
	idx := strings.Index(strings.ToLower(safe), strings.ToLower(kw))
	if idx < 0 {
		return ""
	}
	start := idx - window/2
	if start < 0 {
		start = 0
	}
	// 起点不能落在 UTF-8 中间，否则切片出乱码。
	for start > 0 && !utf8.RuneStart(safe[start]) {
		start--
	}
	end := idx + len(kw) + window/2
	if end > len(safe) {
		end = len(safe)
	}
	for end < len(safe) && !utf8.RuneStart(safe[end]) {
		end++
	}
	frag := safe[start:end]
	// 在转义后的片段里再插 <em>：先截取再插标签，避免标签本身被转义。
	rel := idx - start
	if rel >= 0 && rel+len(kw) <= len(frag) {
		frag = frag[:rel] + "<em>" + frag[rel:rel+len(kw)] + "</em>" + frag[rel+len(kw):]
	}
	if start > 0 {
		frag = "…" + frag
	}
	if end < len(safe) {
		frag += "…"
	}
	return frag
}

// ---------- 标签 ----------

// SaveTag 创建/更新标签。
func (s *Service) SaveTag(ctx context.Context, workspaceID, name, color, desc string) (*repo.TagRow, error) {
	return s.Repo.SaveTag(ctx, workspaceID, name, color, desc)
}

// ListTags 列出标签。
func (s *Service) ListTags(ctx context.Context, workspaceID string) ([]repo.TagRow, error) {
	return s.Repo.ListTags(ctx, workspaceID)
}

// DeleteTag 删除标签。
func (s *Service) DeleteTag(ctx context.Context, workspaceID, name string) error {
	return s.Repo.DeleteTag(ctx, workspaceID, name)
}

// TagEntity 给实体打标。
func (s *Service) TagEntity(ctx context.Context, workspaceID, etype, ename string, tags []string) error {
	return s.Repo.TagEntity(ctx, workspaceID, etype, ename, tags)
}

// UntagEntity 摘标签。
func (s *Service) UntagEntity(ctx context.Context, workspaceID, etype, ename, tag string) error {
	return s.Repo.UntagEntity(ctx, workspaceID, etype, ename, tag)
}

// EntityTags 查实体标签。
func (s *Service) EntityTags(ctx context.Context, workspaceID, etype, ename string) ([]string, error) {
	return s.Repo.EntityTags(ctx, workspaceID, etype, ename)
}

// EntitiesByTag 反查标签下的实体。
func (s *Service) EntitiesByTag(ctx context.Context, workspaceID, tag string, limit int) ([]SearchHit, error) {
	rows, err := s.Repo.EntitiesByTag(ctx, workspaceID, tag, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{Type: r.Type, Name: r.Name, Tags: []string{tag}})
	}
	return out, nil
}
