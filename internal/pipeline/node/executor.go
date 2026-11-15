package node

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/executor/httprunner"
	"github.com/godw/godw/internal/executor/shellrunner"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/quality"
	"github.com/godw/godw/internal/sqlx"
)

// Resolver 解耦执行器与元数据存储：执行器只需要「按名字拿到数据源、模型定义与质量规则」。
type Resolver interface {
	DatasourceSpec(ctx context.Context, workspaceID, name string) (connector.Spec, error)
	ModelSpec(ctx context.Context, workspaceID, name string) (*model.ModelSpec, error)
	// QualityRules 返回模型上已启用的规则。规则独立于管道版本，
	// 改阈值不必重发管道——这是把规则从 YAML 搬进库的主要动机。
	QualityRules(ctx context.Context, workspaceID, model string) ([]model.QualityRule, error)
}

type Executor struct {
	Pool     *connector.Pool
	Resolver Resolver
	WorkDir  string
	MaxRows  int
}

// Stats 是单个节点的执行结果。
type Stats struct {
	RowsAffected int64            `json:"rowsAffected"`
	DurationMS   int64            `json:"durationMs"`
	Output       string           `json:"output,omitempty"`
	Quality      []quality.Result `json:"quality,omitempty"`
	Message      string           `json:"message,omitempty"`
}

// Run 执行一个节点。ctx 取消时应尽快返回，连接器的查询都支持 ctx。
func (e *Executor) Run(ctx context.Context, workspaceID string, node model.NodeSpec, vars map[string]any) (Stats, error) {
	start := time.Now()
	stats := Stats{}
	var err error

	switch node.Type {
	case model.NodeSource:
		err = e.runSource(ctx, workspaceID, node, vars, &stats)
	case model.NodeSQL:
		err = e.runSQL(ctx, workspaceID, node, vars, &stats)
	case model.NodeQuality:
		err = e.runQuality(ctx, workspaceID, node, vars, &stats)
	case model.NodeSink:
		err = e.runSink(ctx, workspaceID, node, vars, &stats)
	case model.NodeShell:
		out, rerr := shellrunner.Run(ctx, e.WorkDir, str(node.Config["script"]), vars)
		stats.Message = out
		err = rerr
	case model.NodeHTTP:
		out, rerr := httprunner.Run(ctx, node.Config, vars)
		stats.Message = out
		err = rerr
	case model.NodeBranch:
		cond, _ := node.Config["when"].(string)
		stats.Message = "branch: " + cond
	default:
		err = errs.New(errs.CodePipelineInvalid, "未知节点类型: "+node.Type)
	}

	stats.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		return stats, err
	}
	return stats, nil
}

// runSource 只做可读性校验与取样计数，不搬数据。
// ELT 模式下数据留在源库，由后续 sql 节点用 INSERT ... SELECT 落目标表。
func (e *Executor) runSource(ctx context.Context, workspaceID string, node model.NodeSpec, vars map[string]any, stats *Stats) error {
	c, err := e.connector(ctx, workspaceID, node.Datasource)
	if err != nil {
		return err
	}
	sqlText, err := sqlx.Render(node.SQL, vars)
	if err != nil {
		return errs.Wrap(errs.CodeCompileFailed, err, "source 节点 "+node.ID+" 渲染失败")
	}
	limit := e.MaxRows
	if limit <= 0 {
		limit = 1000
	}
	probe := fmt.Sprintf("%s %s", strings.TrimRight(strings.TrimSpace(sqlText), ";"),
		c.Dialect().LimitOffset(int64(limit), 0))
	rows, err := c.Query(ctx, probe)
	if err != nil {
		return errs.Wrap(errs.CodeExecutionFailed, err, "source 节点查询失败: "+node.ID)
	}
	defer rows.Close()
	n := int64(0)
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	stats.RowsAffected = n
	stats.Message = fmt.Sprintf("取样 %d 行（上限 %d）", n, limit)
	return nil
}

// runSQL 把 SELECT 结果写入目标模型表。目标表必须已在元数据中登记并发布。
func (e *Executor) runSQL(ctx context.Context, workspaceID string, node model.NodeSpec, vars map[string]any, stats *Stats) error {
	sqlText, err := sqlx.Render(node.SQL, vars)
	if err != nil {
		return errs.Wrap(errs.CodeCompileFailed, err, "节点 "+node.ID+" 渲染失败")
	}
	sqlText = strings.TrimRight(strings.TrimSpace(sqlText), ";")

	target, err := e.Resolver.ModelSpec(ctx, workspaceID, node.Model)
	if err != nil {
		return err
	}
	if err := modelValidate(target); err != nil {
		return err
	}

	c, err := e.connector(ctx, workspaceID, target.Datasource)
	if err != nil {
		return err
	}
	table := qualify(target.Schema, target.TableName(), c.Dialect())

	mode := node.WriteMode
	if mode == "" {
		mode = model.WriteAppend
	}
	// DSL 的 upsert 对应执行侧的 merge；两边名字不同是因为
	// 「upsert」是用户视角（有则更新无则插入），「merge」是实现视角（多路径选型）。
	if mode == model.WriteUpsert {
		mode = string(sqlx.Merge)
	}

	// 写入策略统一由 sqlx.PlanWrite 生成语句序列：
	// 能力选择、事务要求、降级告警都在那里收敛，执行器只负责按序执行。
	caps := c.Capabilities()
	plan, err := sqlx.PlanWrite(sqlx.WriteSpec{
		Mode:    sqlx.WriteMode(mode),
		Dialect: c.Dialect(),
		Caps: sqlx.Caps{
			MergeUpsert:      caps.MergeUpsert,
			InsertOverwrite:  caps.InsertOverwrite,
			ReplacePartition: caps.ReplacePartition,
			Transactional:    hasTx(c),
		},
		Table:           table,
		Columns:         colNames(target.Columns),
		Keys:            writeKeys(target),
		Select:          sqlText,
		PartitionColumn: partitionColumn(target),
		PartitionValue:  partitionValue(target, vars),
		SCD2:            sqlx.SCD2Options{BizDate: sqlx.Literal(vars["biz_date"])},
	})
	if err != nil {
		return err
	}
	for _, w := range plan.Warn {
		stats.Message = strings.TrimSpace(stats.Message + " ⚠ " + w)
	}

	if plan.InTx {
		run := func(exec func(string, ...any) (connector.Result, error)) error {
			var last connector.Result
			for _, stmt := range plan.Statements {
				res, err := exec(stmt)
				if err != nil {
					return errs.Wrap(errs.CodeExecutionFailed, err, "写入 "+table+" 失败").
						WithMeta("sql", truncateSQL(stmt))
				}
				last = res
			}
			stats.RowsAffected = last.RowsAffected
			return nil
		}
		if err := e.atomic(ctx, c, run); err != nil {
			return err
		}
	} else {
		var last connector.Result
		for _, stmt := range plan.Statements {
			res, err := c.Exec(ctx, stmt)
			if err != nil {
				return errs.Wrap(errs.CodeExecutionFailed, err, "写入 "+table+" 失败").
					WithMeta("sql", truncateSQL(stmt))
			}
			last = res
		}
		stats.RowsAffected = last.RowsAffected
	}

	stats.Message = strings.TrimSpace(fmt.Sprintf("%s → %s（%s/%s）%s", node.ID, table, mode, plan.Path, stats.Message))
	return nil
}

func (e *Executor) runQuality(ctx context.Context, workspaceID string, node model.NodeSpec, vars map[string]any, stats *Stats) error {
	spec, err := e.Resolver.ModelSpec(ctx, workspaceID, node.Model)
	if err != nil {
		return err
	}
	c, err := e.connector(ctx, workspaceID, spec.Datasource)
	if err != nil {
		return err
	}
	var rules []quality.Rule
	if rawRules, ok := node.Config["rules"].([]any); ok && len(rawRules) > 0 {
		// 内联规则：旧写法，仍支持。适合「这条管道临时多一道校验」，
		// 但规则不复用、改一个阈值要重发整条管道，长期应迁到模型规则里。
		for _, raw := range rawRules {
			m, ok := raw.(map[string]any)
			if !ok {
				return errs.New(errs.CodePipelineInvalid, "规则格式错误")
			}
			rule := model.QualityRule{
				Name:     str(m["name"]),
				Kind:     str(m["kind"]),
				Column:   str(m["column"]),
				Severity: str(m["severity"]),
				Config:   m,
			}
			if rule.Severity == "" {
				rule.Severity = "error"
			}
			built, err := quality.BuildWith(rule, spec.Schema, spec.TableName(), c.Dialect())
			if err != nil {
				return err
			}
			rules = append(rules, built)
		}
	} else {
		// 默认路径：规则挂在模型上，所有产出该模型的管道共用一套。
		stored, err := e.Resolver.QualityRules(ctx, workspaceID, node.Model)
		if err != nil {
			return err
		}
		for _, rule := range stored {
			if !quality.IsEnabled(rule) {
				continue
			}
			if rule.ModelName == "" {
				rule.ModelName = node.Model
			}
			built, err := quality.BuildWith(rule, spec.Schema, spec.TableName(), c.Dialect())
			if err != nil {
				return err
			}
			rules = append(rules, built)
		}
		if len(rules) == 0 {
			return errs.New(errs.CodePipelineInvalid,
				"质量节点 "+node.ID+" 没有可用规则：请先在模型的「质量规则」中配置，或在节点内联 config.rules")
		}
	}

	// 只校验本次运行的分区。
	// 不限作用域的话，历史上任意一行脏数据都会让今天的日更永远失败，
	// 用户最后只能「放行」了事——门禁也就没有意义了。
	if scope := qualityScope(spec, vars); scope != "" {
		rules = quality.Scope(rules, spec.Schema, spec.TableName(), c.Dialect(), scope)
	}

	results, err := quality.Run(ctx, c, rules)
	if err != nil {
		return err
	}
	stats.Quality = results
	if err := quality.Gate(results); err != nil {
		// 隔离表：把违规样本落盘，便于修复后重放，而不是只留一条失败记录。
		if qtable := str(node.Config["quarantine"]); qtable != "" {
			if n, qerr := e.quarantine(ctx, c, node, spec, results, qtable, vars); qerr == nil {
				stats.Message = fmt.Sprintf("已隔离 %d 行到 %s；%s", n, qtable, err.Error())
			} else {
				stats.Message = err.Error() + "（隔离失败: " + qerr.Error() + "）"
			}
		}
		if node.ContinueOnFailure {
			stats.Message = strings.TrimSpace(err.Error() + "（已配置为不阻断）" + stats.Message)
			return nil
		}
		return err
	}
	stats.Message = fmt.Sprintf("%d 条规则全部通过", len(results))
	return nil
}

// quarantine 把不满足条件的行写入隔离表。使用 CREATE TABLE IF NOT EXISTS + INSERT，
// 保持幂等：重复跑同一天会先删当天分区再写，避免样本堆积。
func (e *Executor) quarantine(ctx context.Context, c connector.Connector, node model.NodeSpec,
	spec *model.ModelSpec, results []quality.Result, table string, vars map[string]any) (int64, error) {
	var failing []string
	for _, r := range results {
		if r.Passed {
			continue
		}
		n, ok := nodeRuleByResult(node, r.RuleName)
		if !ok || n.Column == "" {
			continue
		}
		q := c.Dialect().QuoteIdent(n.Column)
		failing = append(failing, q)
	}
	if len(failing) == 0 {
		return 0, nil
	}
	src := qualify(spec.Schema, spec.TableName(), c.Dialect())
	dst := c.Dialect().QuoteIdent(table)
	cols := make([]string, 0, len(spec.Columns))
	for _, col := range spec.Columns {
		cols = append(cols, c.Dialect().QuoteIdent(col.Name))
	}
	colList := strings.Join(cols, ", ")

	// 隔离表结构随源表，一次性建好。
	if _, err := c.Exec(ctx, fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s AS SELECT * FROM %s WHERE 1=0", dst, src)); err != nil {
		return 0, err
	}
	// 幂等：按分区列限定当天，重复跑不会堆积样本。
	where := ""
	if len(spec.PartitionBy) > 0 {
		col := partitionColumn(spec)
		if v := partitionValue(spec, vars); col != "" && v != nil {
			where = " WHERE " + c.Dialect().QuoteIdent(col) + " = " + sqlLiteral(v)
			// 先删同分区旧样本，保证隔离表与「本次跑数」一致。
			if _, derr := c.Exec(ctx, fmt.Sprintf("DELETE FROM %s%s", dst, where)); derr != nil {
				return 0, derr
			}
		}
	}
	res, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s%s",
		dst, colList, colList, src, where))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected, nil
}

// nodeRuleByResult 按规则名找回节点配置里的原始规则。
func nodeRuleByResult(node model.NodeSpec, name string) (model.QualityRule, bool) {
	raw, ok := node.Config["rules"].([]any)
	if !ok {
		return model.QualityRule{}, false
	}
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if str(m["name"]) == name {
			return model.QualityRule{Name: name, Column: str(m["column"]), Kind: str(m["kind"])}, true
		}
	}
	return model.QualityRule{}, false
}

// runSink 把 SQL 结果导出为 CSV，供下游系统或人工核对。
func (e *Executor) runSink(ctx context.Context, workspaceID string, node model.NodeSpec, vars map[string]any, stats *Stats) error {
	path := str(node.Config["path"])
	if path == "" && node.Model != "" {
		path = node.Model + ".csv"
	}
	if path == "" {
		return errs.New(errs.CodePipelineInvalid, "sink 节点 "+node.ID+" 未指定输出路径")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.WorkDir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	sqlText, err := sqlx.Render(node.SQL, vars)
	if err != nil {
		return errs.Wrap(errs.CodeCompileFailed, err, "sink 节点渲染失败")
	}
	if strings.TrimSpace(sqlText) == "" {
		return errs.New(errs.CodePipelineInvalid, "sink 节点 "+node.ID+" 缺少 sql")
	}
	c, err := e.connector(ctx, workspaceID, node.Datasource)
	if err != nil {
		return err
	}
	rows, err := c.Query(ctx, strings.TrimRight(strings.TrimSpace(sqlText), ";"))
	if err != nil {
		return errs.Wrap(errs.CodeExecutionFailed, err, "sink 查询失败")
	}
	defer rows.Close()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	cols := rows.Columns()
	header := make([]string, len(cols))
	for i, col := range cols {
		header[i] = col.Name
	}
	if err := w.Write(header); err != nil {
		return err
	}
	n := int64(0)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		rec := make([]string, len(vals))
		for i, v := range vals {
			rec[i] = fmt.Sprint(v)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	stats.RowsAffected = n
	stats.Output = path
	stats.Message = fmt.Sprintf("导出 %d 行 → %s", n, path)
	return nil
}

// atomic 有事务能力就走事务，否则逐条执行（ClickHouse 等）。
func (e *Executor) atomic(ctx context.Context, c connector.Connector, fn func(func(string, ...any) (connector.Result, error)) error) error {
	if tx, ok := c.(connector.Transactor); ok {
		return tx.WithTx(ctx, fn)
	}
	return fn(func(sql string, args ...any) (connector.Result, error) {
		return c.Exec(ctx, sql, args...)
	})
}

func (e *Executor) connector(ctx context.Context, workspaceID, name string) (connector.Connector, error) {
	if name == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "节点未指定 datasource")
	}
	spec, err := e.Resolver.DatasourceSpec(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	return e.Pool.Acquire(ctx, spec)
}

func modelValidate(spec *model.ModelSpec) error {
	if spec == nil {
		return errs.New(errs.CodeModelInvalid, "模型定义为空")
	}
	if spec.Datasource == "" {
		return errs.New(errs.CodeModelInvalid, "模型 "+spec.Name+" 未绑定数据源")
	}
	if len(spec.Columns) == 0 {
		return errs.New(errs.CodeModelInvalid, "模型 "+spec.Name+" 没有列定义")
	}
	return nil
}

func qualify(schema, table string, d dialect.Dialect) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

func sqlLiteral(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return fmt.Sprint(v)
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---------- 写入策略辅助 ----------

func hasTx(c connector.Connector) bool {
	_, ok := c.(connector.Transactor)
	return ok
}

func colNames(cols []model.ColumnSpec) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// writeKeys 优先唯一键（业务幂等口径），其次主键。
func writeKeys(spec *model.ModelSpec) []string {
	if len(spec.UniqueKey) > 0 {
		return spec.UniqueKey
	}
	return spec.PrimaryKey
}

// partitionColumn 解析分区表达式取列名；解析不了就退化为原始字符串，
// 让 PlanWrite 报「分区列不存在」而不是静默跳过。
func partitionColumn(spec *model.ModelSpec) string {
	if len(spec.PartitionBy) == 0 {
		return ""
	}
	raw := spec.PartitionBy[0]
	if i := strings.Index(raw, "("); i > 0 {
		inner := strings.TrimSuffix(raw[i+1:], ")")
		if j := strings.LastIndex(inner, ","); j >= 0 {
			inner = inner[j+1:]
		}
		return strings.Trim(strings.TrimSpace(inner), `"'`)
	}
	return strings.TrimSpace(raw)
}

// partitionValue 取分区值：优先同名变量，其次 biz_date。
func partitionValue(spec *model.ModelSpec, vars map[string]any) any {
	col := partitionColumn(spec)
	if col == "" {
		return nil
	}
	if v, ok := vars[col]; ok && v != nil {
		return v
	}
	return vars["biz_date"]
}

// truncateSQL 只保留前 500 字符，避免错误响应里塞进整条 SQL。
func truncateSQL(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// qualityScope 构造质量规则的数据范围：分区列 = 本次业务日期。
//
// 没有分区列时返回空串，退回全表校验——这是有意的：宁可校验得宽一点，
// 也不要猜一个不存在的列名把整条 SQL 弄坏。
func qualityScope(spec *model.ModelSpec, vars map[string]any) string {
	col := partitionColumn(spec)
	if col == "" {
		return ""
	}
	v := partitionValue(spec, vars)
	if v == nil || v == "" {
		return ""
	}
	return col + " = " + sqlx.Literal(v)
}
