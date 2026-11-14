package node_test

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	_ "github.com/godw/godw/internal/connector/postgres"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/executor/shellrunner"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pipeline/node"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

// TestMain 把 shell 解释器指向本机可用的 sh：Windows 没有 /bin/sh。
func TestMain(m *testing.M) {
	if sh, err := exec.LookPath("sh"); err == nil {
		shellrunner.ShellPath = sh
	}
	os.Exit(m.Run())
}

// env 是一套跟着测试生命周期走的真实 PG 环境：独占 schema + 连接池 + 假元数据解析器。
type env struct {
	ctx      context.Context
	schema   string
	admin    *pgx.Conn
	pool     *connector.Pool
	spec     connector.Spec
	exec     *node.Executor
	resolver *fakeResolver
	workDir  string
}

// newEnv 建 schema、建源表/目标表/质量表，并注册进解析器。
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testsupport.DSN())
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}

	schema := fmt.Sprintf("t_node_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("建 schema 失败: %v", err)
	}

	e := &env{ctx: ctx, schema: schema, admin: admin, workDir: t.TempDir()}
	t.Cleanup(func() {
		e.pool.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = admin.Exec(cctx, "DROP SCHEMA "+quoted+" CASCADE")
		_ = admin.Close(cctx)
	})

	q := func(table string) string {
		return pgx.Identifier{schema, table}.Sanitize()
	}
	stmts := []string{
		`CREATE TABLE ` + q("src_orders") + ` (order_id bigint, amount numeric(12,2), dt date, note text)`,
		`INSERT INTO ` + q("src_orders") + ` VALUES
			(1, 10.50, '2025-11-10', 'a'),
			(2, 20.00, '2025-11-10', NULL),
			(3, 30.00, '2025-11-10', 'c'),
			(9, 90.00, '2025-11-11', 'old')`,
		`CREATE TABLE ` + q("dwd_orders") + ` (order_id bigint PRIMARY KEY, amount numeric(12,2), dt date, note text)`,
		`INSERT INTO ` + q("dwd_orders") + ` VALUES (1, 1.00, '2025-11-10', 'v1'), (2, 2.00, '2025-11-10', 'v1'), (9, 9.00, '2025-11-11', 'keep')`,
		`CREATE TABLE ` + q("quality_data") + ` (id bigint PRIMARY KEY, amount bigint, dt date)`,
		`INSERT INTO ` + q("quality_data") + ` VALUES (1, NULL, '2025-11-10'), (2, 5, '2025-11-10'), (3, NULL, '2025-11-11')`,
	}
	for _, stmt := range stmts {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("初始化数据失败: %v\n%s", err, stmt)
		}
	}

	spec := connector.Spec{
		ID:   "node-" + schema,
		Name: "pg",
		Type: "postgres",
		Config: map[string]any{
			"host": "127.0.0.1", "port": 5432, "user": "godw",
			"database": "godw", "sslmode": "disable",
			"max_conns": 2, "min_conns": 0,
		},
		Secret: map[string]string{"password": "godw"},
	}
	e.spec = spec
	e.pool = connector.NewPool(time.Minute)
	e.resolver = &fakeResolver{
		ds: map[string]connector.Spec{"pg": spec},
		models: map[string]*model.ModelSpec{
			"dwd": {
				Name: "dwd", Datasource: "pg", Schema: schema, Table: "dwd_orders",
				Columns: []model.ColumnSpec{
					{Name: "order_id", Type: "bigint"},
					{Name: "amount", Type: "decimal"},
					{Name: "dt", Type: "date"},
					{Name: "note", Type: "text"},
				},
				PrimaryKey:  []string{"order_id"},
				PartitionBy: []string{"dt"},
			},
			"quality": {
				Name: "quality", Datasource: "pg", Schema: schema, Table: "quality_data",
				Columns: []model.ColumnSpec{
					{Name: "id", Type: "bigint"},
					{Name: "amount", Type: "bigint"},
					{Name: "dt", Type: "date"},
				},
				PrimaryKey:  []string{"id"},
				PartitionBy: []string{"dt"},
			},
			"narrow": {
				Name: "narrow", Datasource: "pg", Schema: schema, Table: "dwd_orders",
				Columns:    []model.ColumnSpec{{Name: "order_id", Type: "bigint"}},
				PrimaryKey: []string{"order_id"},
			},
			"quality_nopart": {
				Name: "quality_nopart", Datasource: "pg", Schema: schema, Table: "quality_data",
				Columns: []model.ColumnSpec{
					{Name: "id", Type: "bigint"},
					{Name: "amount", Type: "bigint"},
					{Name: "dt", Type: "date"},
				},
			},
		},
	}
	e.exec = &node.Executor{Pool: e.pool, Resolver: e.resolver, WorkDir: e.workDir, MaxRows: 100}
	return e
}

// count 返回表当前行数，用于断言写入策略的幂等结果。
func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	q := "SELECT count(*) FROM " + pgx.Identifier{e.schema, table}.Sanitize()
	if err := e.admin.QueryRow(e.ctx, q).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// fakeResolver 用内存映射替代元数据库，专注执行器本身的分发与错误处理。
type fakeResolver struct {
	ds       map[string]connector.Spec
	models   map[string]*model.ModelSpec
	dsErr    error
	modelErr error
}

func (r *fakeResolver) DatasourceSpec(_ context.Context, _, name string) (connector.Spec, error) {
	if r.dsErr != nil {
		return connector.Spec{}, r.dsErr
	}
	s, ok := r.ds[name]
	if !ok {
		return connector.Spec{}, errs.New(errs.CodeNotFound, "数据源不存在: "+name)
	}
	return s, nil
}

func (r *fakeResolver) ModelSpec(_ context.Context, _, name string) (*model.ModelSpec, error) {
	if r.modelErr != nil {
		return nil, r.modelErr
	}
	m, ok := r.models[name]
	if !ok {
		return nil, errs.New(errs.CodeNotFound, "模型不存在: "+name)
	}
	return m, nil
}

// ---------- source ----------

// 取样计数、MaxRows 截断、模板渲染与查询失败都要走通。
func TestRunSource(t *testing.T) {
	e := newEnv(t)
	src := model.NodeSpec{
		ID: "src", Type: model.NodeSource, Datasource: "pg",
		SQL: "SELECT * FROM " + pgx.Identifier{e.schema, "src_orders"}.Sanitize() + " WHERE dt = ${dt};",
	}
	stats, err := e.exec.Run(e.ctx, "ws", src, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("source 执行失败: %v", err)
	}
	if stats.RowsAffected != 3 || !strings.Contains(stats.Message, "取样 3 行") {
		t.Fatalf("取样结果异常: %+v", stats)
	}

	// MaxRows 截断：只探测 2 行。
	e.exec.MaxRows = 2
	stats, err = e.exec.Run(e.ctx, "ws", src, map[string]any{"dt": "2025-11-10"})
	if err != nil || stats.RowsAffected != 2 {
		t.Fatalf("MaxRows 截断异常: %+v err=%v", stats, err)
	}

	// MaxRows<=0 回落默认 1000，不应报错。
	e.exec.MaxRows = 0
	if _, err := e.exec.Run(e.ctx, "ws", src, map[string]any{"dt": "2025-11-10"}); err != nil {
		t.Fatalf("默认 MaxRows 不应失败: %v", err)
	}
}

func TestRunSourceErrors(t *testing.T) {
	e := newEnv(t)
	base := model.NodeSpec{ID: "src", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 1"}

	// 缺 datasource。
	s := base
	s.Datasource = ""
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 datasource 应报 CodeInvalidArgument，实际 %v", err)
	}

	// 解析器里没有这个数据源。
	s.Datasource = "missing"
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("未知数据源应报 CodeNotFound，实际 %v", err)
	}

	// 模板变量缺失。
	s = base
	s.SQL = "SELECT ${nope}"
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeCompileFailed) {
		t.Fatalf("渲染失败应报 CodeCompileFailed，实际 %v", err)
	}

	// SQL 本身执行失败。
	s.SQL = "SELECT * FROM no_such_table"
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("查询失败应报 CodeExecutionFailed，实际 %v", err)
	}

	// ctx 已取消：查询必须尽快失败。
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.exec.Run(ctx, "ws", s, nil); err == nil {
		t.Fatal("取消的 ctx 应导致失败")
	}
}

// ---------- sql ----------

const srcSelect = "SELECT order_id, amount, dt, note FROM %s WHERE dt = ${dt}"

func (e *env) selectSrc() string {
	return fmt.Sprintf(srcSelect, pgx.Identifier{e.schema, "src_orders"}.Sanitize())
}

// append：直接追加，不做幂等清理；这里用偏移主键避开目标表已有数据。
func TestRunSQLAppend(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{
		ID: "load", Type: model.NodeSQL, Model: "dwd", WriteMode: model.WriteAppend,
		SQL: "SELECT order_id + 100, amount, dt, note FROM " + pgx.Identifier{e.schema, "src_orders"}.Sanitize() + " WHERE dt = ${dt}",
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if stats.RowsAffected != 3 {
		t.Fatalf("append 应写入 3 行，实际 %+v", stats)
	}
	if got := e.count(t, "dwd_orders"); got != 6 {
		t.Fatalf("append 后应为 6 行（原 3 + 新 3），实际 %d", got)
	}
	if !strings.Contains(stats.Message, "append/append") {
		t.Fatalf("应走 append 路径: %s", stats.Message)
	}
}

// upsert 走 merge：目标已有 1/2/9，源是 1/2/3，最终应为 4 行。
func TestRunSQLUpsert(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{ID: "load", Type: model.NodeSQL, Model: "dwd", WriteMode: model.WriteUpsert, SQL: e.selectSrc()}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	if got := e.count(t, "dwd_orders"); got != 4 {
		t.Fatalf("upsert 后应为 4 行，实际 %d", got)
	}
	if !strings.Contains(stats.Message, "merge/on_conflict") {
		t.Fatalf("应走 on_conflict 路径: %s", stats.Message)
	}
	// 已有行的值必须被更新。
	var amount float64
	if err := e.admin.QueryRow(e.ctx, "SELECT amount FROM "+pgx.Identifier{e.schema, "dwd_orders"}.Sanitize()+" WHERE order_id = 1").Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != 10.5 {
		t.Fatalf("order 1 未更新，amount=%v", amount)
	}
}

// overwrite 走 TRUNCATE + INSERT，必须在事务里执行。
func TestRunSQLOverwrite(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{ID: "load", Type: model.NodeSQL, Model: "dwd", WriteMode: model.WriteOverwrite, SQL: e.selectSrc()}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("overwrite 失败: %v", err)
	}
	if got := e.count(t, "dwd_orders"); got != 3 {
		t.Fatalf("overwrite 后应为 3 行，实际 %d", got)
	}
	if !strings.Contains(stats.Message, "truncate+insert") {
		t.Fatalf("应走 truncate+insert 路径: %s", stats.Message)
	}
}

// overwrite_partition 只替换当天分区，历史分区必须保留。
func TestRunSQLOverwritePartition(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{ID: "load", Type: model.NodeSQL, Model: "dwd", WriteMode: model.WriteOverwritePartition, SQL: e.selectSrc()}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10", "biz_date": "2025-11-10"})
	if err != nil {
		t.Fatalf("overwrite_partition 失败: %v", err)
	}
	if got := e.count(t, "dwd_orders"); got != 4 {
		t.Fatalf("分区覆写后应为 4 行（3 新 + 1 历史），实际 %d", got)
	}
	var keep int
	if err := e.admin.QueryRow(e.ctx, "SELECT count(*) FROM "+pgx.Identifier{e.schema, "dwd_orders"}.Sanitize()+" WHERE order_id = 9").Scan(&keep); err != nil {
		t.Fatal(err)
	}
	if keep != 1 {
		t.Fatalf("历史分区被误删: %d", keep)
	}
	if !strings.Contains(stats.Message, "delete+insert") {
		t.Fatalf("应走 delete+insert 路径: %s", stats.Message)
	}
}

func TestRunSQLModelErrors(t *testing.T) {
	e := newEnv(t)
	base := model.NodeSpec{ID: "load", Type: model.NodeSQL, Model: "dwd", SQL: "SELECT 1"}

	// 解析器报错。
	e.resolver.modelErr = errors.New("metadata down")
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); err == nil {
		t.Fatal("解析器错误应向上返回")
	}
	e.resolver.modelErr = nil

	// 模型不存在。
	base.Model = "missing"
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("模型不存在应报 CodeNotFound，实际 %v", err)
	}

	// 空模型 / 未绑数据源 / 无列。
	e.resolver.models["nil"] = nil
	e.resolver.models["nods"] = &model.ModelSpec{Name: "nods", Columns: []model.ColumnSpec{{Name: "id"}}}
	e.resolver.models["nocol"] = &model.ModelSpec{Name: "nocol", Datasource: "pg"}
	for _, name := range []string{"nil", "nods", "nocol"} {
		base.Model = name
		if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeModelInvalid) {
			t.Fatalf("模型 %s 应报 CodeModelInvalid，实际 %v", name, err)
		}
	}

	// 渲染失败。
	base.Model = "dwd"
	base.SQL = "SELECT ${missing}"
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeCompileFailed) {
		t.Fatalf("渲染失败应报 CodeCompileFailed，实际 %v", err)
	}

	// 目标模型绑定的数据源不存在。
	e.resolver.models["badds"] = &model.ModelSpec{
		Name: "badds", Datasource: "nope", Schema: e.schema, Table: "dwd_orders",
		Columns: []model.ColumnSpec{{Name: "order_id"}},
	}
	base.Model = "badds"
	base.SQL = "SELECT 1"
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("未知数据源应报 CodeNotFound，实际 %v", err)
	}

	// merge 但除主键外没有可更新列。
	base.Model = "narrow"
	base.SQL = "SELECT order_id FROM " + pgx.Identifier{e.schema, "src_orders"}.Sanitize()
	base.WriteMode = model.WriteUpsert
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("无可更新列应报 CodeModelInvalid，实际 %v", err)
	}

	// 未知写入策略。
	base.Model = "dwd"
	base.SQL = e.selectSrc()
	base.WriteMode = "bogus"
	if _, err := e.exec.Run(e.ctx, "ws", base, map[string]any{"dt": "2025-11-10"}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未知策略应报 CodeInvalidArgument，实际 %v", err)
	}
}

// 写入阶段失败必须带出目标表与截断后的 SQL，便于定位。
func TestRunSQLExecFailure(t *testing.T) {
	e := newEnv(t)

	// append（无事务路径）执行失败。
	n := model.NodeSpec{
		ID: "load", Type: model.NodeSQL, Model: "dwd", WriteMode: model.WriteAppend,
		SQL: "SELECT order_id, amount, dt, note FROM no_such_table",
	}
	_, err := e.exec.Run(e.ctx, "ws", n, nil)
	if !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("应报 CodeExecutionFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "写入") {
		t.Fatalf("错误信息缺少目标表: %v", err)
	}

	// overwrite（事务路径）执行失败：TRUNCATE 后 INSERT 失败，整体回滚。
	n.WriteMode = model.WriteOverwrite
	_, err = e.exec.Run(e.ctx, "ws", n, nil)
	if !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("事务写入失败应报 CodeExecutionFailed，实际 %v", err)
	}
	// 回滚后目标表必须保持原样。
	if got := e.count(t, "dwd_orders"); got != 3 {
		t.Fatalf("失败后目标表被破坏，剩余 %d 行", got)
	}

	// 超长 SQL：错误 meta 里只保留前 500 字符。
	long := "SELECT order_id, amount, dt, note FROM no_such_table /* " + strings.Repeat("x", 600) + " */"
	n.SQL = long
	n.WriteMode = model.WriteAppend
	_, err = e.exec.Run(e.ctx, "ws", n, nil)
	meta := errs.From(err).Meta["sql"]
	if !strings.Contains(fmt.Sprint(meta), "(truncated)") {
		t.Fatalf("超长 SQL 未截断: %v", meta)
	}
}

// ---------- quality ----------

func (e *env) qualityNode(rules []any, extra map[string]any) model.NodeSpec {
	cfg := map[string]any{"rules": rules}
	for k, v := range extra {
		cfg[k] = v
	}
	return model.NodeSpec{ID: "q", Type: model.NodeQuality, Model: "quality", Config: cfg}
}

func TestRunQualityPass(t *testing.T) {
	e := newEnv(t)
	rules := []any{
		map[string]any{"name": "id_not_null", "kind": "not_null", "column": "id"},
		map[string]any{"name": "id_unique", "kind": "unique", "column": "id", "severity": "warn"},
	}
	stats, err := e.exec.Run(e.ctx, "ws", e.qualityNode(rules, nil), nil)
	if err != nil {
		t.Fatalf("质量规则应通过: %v", err)
	}
	if len(stats.Quality) != 2 || !strings.Contains(stats.Message, "2 条规则全部通过") {
		t.Fatalf("质量结果异常: %+v", stats)
	}
	// severity 缺省必须补成 error。
	if stats.Quality[0].Severity != "error" {
		t.Fatalf("默认 severity 应为 error，实际 %s", stats.Quality[0].Severity)
	}
}

// 分区收窄：11-10 的 amount 只有 1 个空值，11-11 的脏数据不应影响今天的门禁。
func TestRunQualityScope(t *testing.T) {
	e := newEnv(t)
	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}

	if _, err := e.exec.Run(e.ctx, "ws", e.qualityNode(rules, nil), map[string]any{"biz_date": "2025-11-10"}); !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("当天分区应有失败，实际 %v", err)
	}

	// 全表校验会看到 11-11 的额外空值，错误信息应包含实际观测值。
	_, err := e.exec.Run(e.ctx, "ws", e.qualityNode(rules, nil), nil)
	if !errs.Is(err, errs.CodeQualityGate) || !strings.Contains(err.Error(), "amount_not_null") {
		t.Fatalf("全表校验应失败并指出规则，实际 %v", err)
	}
}

func TestRunQualityContinueOnFailure(t *testing.T) {
	e := newEnv(t)
	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}
	n := e.qualityNode(rules, nil)
	n.ContinueOnFailure = true
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"biz_date": "2025-11-10"})
	if err != nil {
		t.Fatalf("配置不阻断时不应返回错误: %v", err)
	}
	if !strings.Contains(stats.Message, "不阻断") {
		t.Fatalf("消息应说明不阻断: %s", stats.Message)
	}
}

// 隔离成功：只隔离本次分区，重复跑同一天结果幂等。
func TestRunQualityQuarantine(t *testing.T) {
	e := newEnv(t)
	qtable := fmt.Sprintf("node_q_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), `DROP TABLE IF EXISTS `+pgx.Identifier{qtable}.Sanitize())
	})

	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}
	n := e.qualityNode(rules, map[string]any{"quarantine": qtable})
	vars := map[string]any{"biz_date": "2025-11-10"}

	stats, err := e.exec.Run(e.ctx, "ws", n, vars)
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("门禁应失败，实际 %v", err)
	}
	if !strings.Contains(stats.Message, "已隔离 2 行") {
		t.Fatalf("隔离行数异常: %s", stats.Message)
	}
	var n1 int
	if err := e.admin.QueryRow(e.ctx, "SELECT count(*) FROM "+pgx.Identifier{qtable}.Sanitize()).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	if n1 != 2 {
		t.Fatalf("隔离表应有 2 行，实际 %d", n1)
	}

	// 再跑一次：先删当天再写，行数不堆积。
	if _, err := e.exec.Run(e.ctx, "ws", n, vars); !errs.Is(err, errs.CodeQualityGate) {
		t.Fatal("重复跑仍应失败")
	}
	var n2 int
	if err := e.admin.QueryRow(e.ctx, "SELECT count(*) FROM "+pgx.Identifier{qtable}.Sanitize()).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n2 != n1 {
		t.Fatalf("重复跑隔离表行数应稳定：%d -> %d", n1, n2)
	}
}

// 隔离失败：目标表结构不匹配时，错误信息要带「隔离失败」而不是覆盖门禁原因。
func TestRunQualityQuarantineFailure(t *testing.T) {
	e := newEnv(t)
	qtable := fmt.Sprintf("node_qbad_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), `DROP TABLE IF EXISTS `+pgx.Identifier{qtable}.Sanitize())
	})
	if _, err := e.admin.Exec(e.ctx, "CREATE TABLE "+pgx.Identifier{qtable}.Sanitize()+" (wrong int)"); err != nil {
		t.Fatal(err)
	}

	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}
	n := e.qualityNode(rules, map[string]any{"quarantine": qtable})
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"biz_date": "2025-11-10"})
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("门禁原因应保留，实际 %v", err)
	}
	if !strings.Contains(stats.Message, "隔离失败") {
		t.Fatalf("应提示隔离失败: %s", stats.Message)
	}
}

// 失败规则没有列（custom_sql）时无法定位样本，隔离行数应为 0。
func TestRunQualityQuarantineNoColumn(t *testing.T) {
	e := newEnv(t)
	rules := []any{map[string]any{
		"name": "row_count_zero", "kind": "custom_sql",
		"sql":    "SELECT COUNT(*) AS observed, 0 AS expected FROM " + pgx.Identifier{e.schema, "quality_data"}.Sanitize() + " WHERE amount IS NULL",
		"expect": "0",
	}}
	n := e.qualityNode(rules, map[string]any{"quarantine": "node_q_nocol"})
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), `DROP TABLE IF EXISTS `+pgx.Identifier{"node_q_nocol"}.Sanitize())
	})
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("应门禁失败，实际 %v", err)
	}
	if !strings.Contains(stats.Message, "已隔离 0 行") {
		t.Fatalf("无列规则不应隔离数据: %s", stats.Message)
	}
}

func TestRunQualityErrors(t *testing.T) {
	e := newEnv(t)
	base := model.NodeSpec{ID: "q", Type: model.NodeQuality, Model: "quality"}

	// 模型解析失败。
	e.resolver.modelErr = errors.New("metadata down")
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); err == nil {
		t.Fatal("模型错误应向上返回")
	}
	e.resolver.modelErr = nil

	// 数据源不存在。
	e.resolver.models["badds"] = &model.ModelSpec{Name: "badds", Datasource: "nope"}
	base.Model = "badds"
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("数据源错误应返回，实际 %v", err)
	}
	base.Model = "quality"

	// 缺 rules / 空 rules / 类型错误。
	for _, cfg := range []map[string]any{
		nil,
		{"rules": []any{}},
		{"rules": "not-a-list"},
	} {
		base.Config = cfg
		if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodePipelineInvalid) {
			t.Fatalf("rules 配置 %#v 应报 CodePipelineInvalid，实际 %v", cfg, err)
		}
	}

	// 规则不是对象。
	base.Config = map[string]any{"rules": []any{"oops"}}
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("规则格式错误应报 CodePipelineInvalid，实际 %v", err)
	}

	// 规则编译失败（accepted_values 缺少 values）。
	base.Config = map[string]any{"rules": []any{map[string]any{
		"name": "x", "kind": "accepted_values", "column": "amount",
	}}}
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("规则编译失败应报 CodeInvalidArgument，实际 %v", err)
	}

	// 规则执行失败（custom_sql 查询不存在的表）。
	base.Config = map[string]any{"rules": []any{map[string]any{
		"name": "bad", "kind": "custom_sql", "sql": "SELECT 1 AS observed, 0 AS expected FROM no_such_table WHERE true",
	}}}
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("规则执行失败应报 CodeExecutionFailed，实际 %v", err)
	}
}

// ---------- sink ----------

func TestRunSink(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{
		ID: "export", Type: model.NodeSink, Datasource: "pg",
		SQL:    "SELECT order_id, amount, note FROM " + pgx.Identifier{e.schema, "src_orders"}.Sanitize() + " WHERE dt = ${dt} ORDER BY order_id",
		Config: map[string]any{"path": "nested/orders.csv"},
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("sink 失败: %v", err)
	}
	wantPath := filepath.Join(e.workDir, "nested", "orders.csv")
	if stats.Output != wantPath || stats.RowsAffected != 3 {
		t.Fatalf("导出结果异常: %+v", stats)
	}

	f, err := os.Open(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || strings.Join(records[0], ",") != "order_id,amount,note" {
		t.Fatalf("CSV 内容异常: %#v", records)
	}
	// NULL 要按 <nil> 输出，不能整行丢失。
	if records[2][2] != "<nil>" {
		t.Fatalf("NULL 值输出异常: %#v", records[2])
	}
}

// 未显式给 path 时用模型名 + .csv 作为默认文件名。
func TestRunSinkDefaultPath(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{
		ID: "export", Type: model.NodeSink, Model: "dwd", Datasource: "pg",
		SQL: "SELECT count(*) FROM " + pgx.Identifier{e.schema, "dwd_orders"}.Sanitize(),
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Output != filepath.Join(e.workDir, "dwd.csv") {
		t.Fatalf("默认路径错误: %s", stats.Output)
	}
}

func TestRunSinkErrors(t *testing.T) {
	e := newEnv(t)
	base := model.NodeSpec{ID: "export", Type: model.NodeSink, Datasource: "pg", SQL: "SELECT 1"}

	// 既没有 path 也没有 model。
	if _, err := e.exec.Run(e.ctx, "ws", base, nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("缺输出路径应报 CodePipelineInvalid，实际 %v", err)
	}

	// 渲染失败。
	s := base
	s.Config = map[string]any{"path": "x.csv"}
	s.SQL = "SELECT ${missing}"
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeCompileFailed) {
		t.Fatalf("渲染失败应报 CodeCompileFailed，实际 %v", err)
	}

	// SQL 为空。
	s.SQL = "   "
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("空 SQL 应报 CodePipelineInvalid，实际 %v", err)
	}

	// 数据源缺失。
	s.SQL = "SELECT 1"
	s.Datasource = ""
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺数据源应报 CodeInvalidArgument，实际 %v", err)
	}

	// 查询失败。
	s.Datasource = "pg"
	s.SQL = "SELECT * FROM no_such_table"
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("查询失败应报 CodeExecutionFailed，实际 %v", err)
	}

	// 输出路径是目录：创建文件失败。
	s.SQL = "SELECT 1"
	s.Config = map[string]any{"path": e.workDir}
	if _, err := e.exec.Run(e.ctx, "ws", s, nil); err == nil {
		t.Fatal("路径为目录时应失败")
	}
}

// ---------- shell / http / branch / unknown ----------

func TestRunShell(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{ID: "sh", Type: model.NodeShell, Config: map[string]any{"script": "echo hello-$NAME"}}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"name": "godw"})
	if err != nil {
		t.Fatalf("shell 失败: %v", err)
	}
	if !strings.Contains(stats.Message, "hello-godw") {
		t.Fatalf("变量未注入: %s", stats.Message)
	}

	// 非零退出码必须失败。
	n.Config = map[string]any{"script": "exit 3"}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil {
		t.Fatal("非零退出码应失败")
	}

	// 空脚本在起进程前拒绝。
	n.Config = map[string]any{"script": ""}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil {
		t.Fatal("空脚本应失败")
	}
}

func TestRunHTTP(t *testing.T) {
	var gotMethod, gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-Test")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok:" + gotBody))
	}))
	defer srv.Close()

	e := newEnv(t)
	n := model.NodeSpec{ID: "cb", Type: model.NodeHTTP, Config: map[string]any{
		"url": srv.URL, "method": "post", "headers": map[string]any{"X-Test": "v"},
		"body": "hello ${name}", "expect_status": 201,
	}}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("http 失败: %v", err)
	}
	if stats.Message != "ok:hello world" {
		t.Fatalf("响应体异常: %q", stats.Message)
	}
	if gotMethod != "POST" || gotHeader != "v" || gotBody != "hello world" {
		t.Fatalf("请求构造异常: method=%s header=%s body=%s", gotMethod, gotHeader, gotBody)
	}

	// 期望状态码不符。
	n.Config["expect_status"] = 200
	if _, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"name": "world"}); err == nil {
		t.Fatal("状态码不符应失败")
	}

	// 缺 url / 非 http 协议。
	n.Config = map[string]any{}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil {
		t.Fatal("缺 url 应失败")
	}
	n.Config = map[string]any{"url": "ftp://x"}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil {
		t.Fatal("非 http 协议应失败")
	}
}

func TestRunBranchAndUnknown(t *testing.T) {
	e := newEnv(t)
	stats, err := e.exec.Run(e.ctx, "ws", model.NodeSpec{
		ID: "b", Type: model.NodeBranch, Config: map[string]any{"when": "amount > 0"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Message != "branch: amount > 0" {
		t.Fatalf("分支消息异常: %s", stats.Message)
	}

	if _, err := e.exec.Run(e.ctx, "ws", model.NodeSpec{ID: "x", Type: "no-such"}, nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("未知节点类型应报 CodePipelineInvalid，实际 %v", err)
	}
}

// ---------- 补充分支 ----------

// 未显式指定 write_mode 时默认 append。
func TestRunSQLDefaultMode(t *testing.T) {
	e := newEnv(t)
	n := model.NodeSpec{
		ID: "load", Type: model.NodeSQL, Model: "dwd",
		SQL: "SELECT order_id + 200, amount, dt, note FROM " + pgx.Identifier{e.schema, "src_orders"}.Sanitize() + " WHERE dt = ${dt}",
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, map[string]any{"dt": "2025-11-10"})
	if err != nil {
		t.Fatalf("默认模式应成功: %v", err)
	}
	if !strings.Contains(stats.Message, "append/append") {
		t.Fatalf("默认应走 append: %s", stats.Message)
	}
}

// full_refresh 会走换名路径并产生「存储翻倍」告警，这里用假连接器只验证计划选型。
// 注意：planSwap 对「schema.table」形式的目标表会拼出非法标识符，见交付说明中的问题记录。
func TestRunSQLFullRefreshWarns(t *testing.T) {
	e := newEnv(t)
	e.resolver.ds["fake"] = connector.Spec{ID: "fake-ds", Type: "nodefake"}
	e.resolver.models["fakeTarget"] = &model.ModelSpec{
		Name: "fakeTarget", Datasource: "fake", Schema: "dw", Table: "t",
		Columns: []model.ColumnSpec{{Name: "id"}}, PrimaryKey: []string{"id"},
	}
	n := model.NodeSpec{
		ID: "load", Type: model.NodeSQL, Model: "fakeTarget", WriteMode: "full_refresh", SQL: "SELECT 1 AS id",
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if err != nil {
		t.Fatalf("full_refresh 计划执行失败: %v", err)
	}
	if !strings.Contains(stats.Message, "swap") || !strings.Contains(stats.Message, "⚠") {
		t.Fatalf("应走 swap 并给出告警: %s", stats.Message)
	}
}

// 输出目录的父路径是普通文件时，MkdirAll 必须失败而不是继续写。
func TestRunSinkMkdirError(t *testing.T) {
	e := newEnv(t)
	blocker := filepath.Join(e.workDir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := model.NodeSpec{
		ID: "export", Type: model.NodeSink, Datasource: "pg", SQL: "SELECT 1",
		Config: map[string]any{"path": "blocked/out.csv"},
	}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil {
		t.Fatal("父路径是文件时应失败")
	}
}

// 无分区模型 + 预置错误结构的隔离表：INSERT 阶段失败。
func TestRunQualityQuarantineInsertError(t *testing.T) {
	e := newEnv(t)
	qtable := fmt.Sprintf("node_qins_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), `DROP TABLE IF EXISTS `+pgx.Identifier{qtable}.Sanitize())
	})
	if _, err := e.admin.Exec(e.ctx, "CREATE TABLE "+pgx.Identifier{qtable}.Sanitize()+" (wrong int)"); err != nil {
		t.Fatal(err)
	}

	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}
	cfg := map[string]any{"rules": rules, "quarantine": qtable}
	n := model.NodeSpec{ID: "q", Type: model.NodeQuality, Model: "quality_nopart", Config: cfg}
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("应门禁失败，实际 %v", err)
	}
	if !strings.Contains(stats.Message, "隔离失败") {
		t.Fatalf("应提示隔离失败: %s", stats.Message)
	}
}

// fakeConn 用于覆盖「连接器不支持事务」时 atomic 的降级执行路径。
type fakeConn struct{}

func (c *fakeConn) Type() string                                  { return "nodefake" }
func (c *fakeConn) Capabilities() connector.Capability            { return connector.Capability{} }
func (c *fakeConn) Dialect() dialect.Dialect                      { return dialect.Postgres{} }
func (c *fakeConn) Test(context.Context) error                    { return nil }
func (c *fakeConn) ListSchemas(context.Context) ([]string, error) { return nil, nil }
func (c *fakeConn) ListTables(context.Context, string) ([]connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) DescribeTable(context.Context, string, string) (*connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) Query(context.Context, string, ...any) (connector.Rows, error) {
	return connector.NewMemRows([]connector.ColumnMeta{{Name: "id"}}, [][]any{{int64(1)}}), nil
}
func (c *fakeConn) Exec(context.Context, string, ...any) (connector.Result, error) {
	return connector.Result{RowsAffected: 2}, nil
}
func (c *fakeConn) Stream(context.Context, string, int, func(connector.Rows) error) error {
	return nil
}
func (c *fakeConn) Close() error { return nil }

func init() {
	connector.Register("nodefake", func(context.Context, connector.Spec) (connector.Connector, error) {
		return &fakeConn{}, nil
	})
}

// delete_insert 要求事务，但假连接器没有事务能力：atomic 应逐条执行。
func TestRunSQLAtomicFallback(t *testing.T) {
	e := newEnv(t)
	e.resolver.ds["fake"] = connector.Spec{ID: "fake-ds", Type: "nodefake"}
	e.resolver.models["fakeTarget"] = &model.ModelSpec{
		Name: "fakeTarget", Datasource: "fake", Schema: "dw", Table: "t",
		Columns: []model.ColumnSpec{{Name: "id"}}, PrimaryKey: []string{"id"},
	}
	n := model.NodeSpec{
		ID: "load", Type: model.NodeSQL, Model: "fakeTarget", WriteMode: "delete_insert", SQL: "SELECT 1 AS id",
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if err != nil {
		t.Fatalf("无事务连接器应降级执行: %v", err)
	}
	if stats.RowsAffected != 2 || !strings.Contains(stats.Message, "delete+insert") {
		t.Fatalf("降级执行结果异常: %+v", stats)
	}
}

// errRows 模拟驱动在迭代结束后才暴露的网络错误。
type errRows struct{}

func (r *errRows) Columns() []connector.ColumnMeta { return []connector.ColumnMeta{{Name: "id"}} }
func (r *errRows) Next() bool                      { return false }
func (r *errRows) Scan(...any) error               { return nil }
func (r *errRows) Err() error                      { return errors.New("iterate failed") }
func (r *errRows) Close() error                    { return nil }

// errConn 只覆盖 Query，用于制造「迭代出错」的结果集。
type errConn struct{ fakeConn }

func (c *errConn) Type() string { return "nodefakeerr" }
func (c *errConn) Query(context.Context, string, ...any) (connector.Rows, error) {
	return &errRows{}, nil
}

func init() {
	connector.Register("nodefakeerr", func(context.Context, connector.Spec) (connector.Connector, error) {
		return &errConn{}, nil
	})
}

// source 取样时 rows.Err 必须向上返回，不能把网络中断当成「0 行」。
func TestRunSourceRowsErr(t *testing.T) {
	e := newEnv(t)
	e.resolver.ds["fakeerr"] = connector.Spec{ID: "fakeerr-ds", Type: "nodefakeerr"}
	n := model.NodeSpec{ID: "src", Type: model.NodeSource, Datasource: "fakeerr", SQL: "SELECT 1"}
	_, err := e.exec.Run(e.ctx, "ws", n, nil)
	if err == nil || !strings.Contains(err.Error(), "iterate failed") {
		t.Fatalf("应返回迭代错误，实际 %v", err)
	}
}

// sink 导出时同样不能吞掉 rows.Err，否则会生成一个看似成功的残缺 CSV。
func TestRunSinkRowsErr(t *testing.T) {
	e := newEnv(t)
	e.resolver.ds["fakeerr"] = connector.Spec{ID: "fakeerr-ds", Type: "nodefakeerr"}
	n := model.NodeSpec{
		ID: "export", Type: model.NodeSink, Datasource: "fakeerr", SQL: "SELECT 1",
		Config: map[string]any{"path": "bad.csv"},
	}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil || !strings.Contains(err.Error(), "iterate failed") {
		t.Fatalf("应返回迭代错误，实际 %v", err)
	}
}

// 隔离表名含非法字符时，CREATE TABLE 失败应被捕获并提示「隔离失败」。
func TestRunQualityQuarantineCreateError(t *testing.T) {
	e := newEnv(t)
	rules := []any{map[string]any{"name": "amount_not_null", "kind": "not_null", "column": "amount"}}
	n := model.NodeSpec{
		ID: "q", Type: model.NodeQuality, Model: "quality_nopart",
		Config: map[string]any{"rules": rules, "quarantine": "bad\x00name"},
	}
	stats, err := e.exec.Run(e.ctx, "ws", n, nil)
	if !errs.Is(err, errs.CodeQualityGate) {
		t.Fatalf("应门禁失败，实际 %v", err)
	}
	if !strings.Contains(stats.Message, "隔离失败") {
		t.Fatalf("应提示隔离失败: %s", stats.Message)
	}
}

// scanFailRows 模拟结果集行与目标列类型不兼容。
type scanFailRows struct{ done bool }

func (r *scanFailRows) Columns() []connector.ColumnMeta { return []connector.ColumnMeta{{Name: "id"}} }
func (r *scanFailRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *scanFailRows) Scan(...any) error { return errors.New("scan failed") }
func (r *scanFailRows) Err() error        { return nil }
func (r *scanFailRows) Close() error      { return nil }

type scanFailConn struct{ fakeConn }

func (c *scanFailConn) Type() string { return "nodefakescan" }
func (c *scanFailConn) Query(context.Context, string, ...any) (connector.Rows, error) {
	return &scanFailRows{}, nil
}

func init() {
	connector.Register("nodefakescan", func(context.Context, connector.Spec) (connector.Connector, error) {
		return &scanFailConn{}, nil
	})
}

// 导出时 Scan 失败必须中止，不能把半行数据写进 CSV。
func TestRunSinkScanError(t *testing.T) {
	e := newEnv(t)
	e.resolver.ds["sc"] = connector.Spec{ID: "scan-ds", Type: "nodefakescan"}
	n := model.NodeSpec{
		ID: "export", Type: model.NodeSink, Datasource: "sc", SQL: "SELECT 1",
		Config: map[string]any{"path": "scan.csv"},
	}
	if _, err := e.exec.Run(e.ctx, "ws", n, nil); err == nil || !strings.Contains(err.Error(), "scan failed") {
		t.Fatalf("应返回扫描错误，实际 %v", err)
	}
}
