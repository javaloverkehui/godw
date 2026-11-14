package service_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	// 驱动注册：服务测试直接构造连接器，不经过 main，必须在这里显式注册。
	_ "github.com/godw/godw/internal/connector/postgres"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

// TestMain 建立整包共享的 PG schema 并跑迁移。service 层的价值几乎全在
// 「这条 SQL / 这段流程在真库上对不对」，因此不用 mock 仓储。
func TestMain(m *testing.M) { os.Exit(testsupport.Main(m)) }

// fs 是文件系统测试用的临时目录，Config(t) 已经指向 t.TempDir()，这里不再重复。
func ctxBG() context.Context { return context.Background() }

// newSvc 返回一个干净的服务实例与默认工作空间。每个用例独立 Reset，
// 避免前一个用例的实例 / 策略污染后一个用例的断言。
func newSvc(t *testing.T) (*service.Service, *model.Workspace) {
	t.Helper()
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	svc, err := service.New(db, testsupport.Config(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	ws, err := svc.Bootstrap(ctxBG())
	if err != nil {
		t.Fatalf("Bootstrap 失败: %v", err)
	}
	// 连接器池不关会把 PG 的 max_connections 耗尽（每个 use 至少 1 条常驻连接），
	// 整包几十个用例连下来足以把测试库打满。
	t.Cleanup(svc.Pool.Close)
	return svc, ws
}

// newPgDatasource 建一个指向本机真实 PG 的数据源（profile / 预览 / 原始 SQL 需要真表）。
func newPgDatasource(t *testing.T, svc *service.Service, wsID string) *model.Datasource {
	t.Helper()
	ds, err := svc.CreateDatasource(ctxBG(), wsID, "pg", "postgres", map[string]any{
		"host": "127.0.0.1", "port": "5432", "user": "godw",
		"database": "godw", "sslmode": "disable",
	}, map[string]string{"password": "godw"})
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	return ds
}

// phys 是直接连库的测试夹具：仓储连接带 search_path（专用测试 schema），
// 而「业务数据表」必须放在独立 schema 里，否则会被 testsupport.Reset 截断。
type phys struct {
	conn   *pgx.Conn
	schema string
}

// newPhys 建一个用例独占的物理 schema，用例结束自动 DROP。
func newPhys(t *testing.T) *phys {
	t.Helper()
	conn, err := pgx.Connect(ctxBG(), testsupport.DSN())
	if err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	schema := "p_" + randHex(6)
	if _, err := conn.Exec(ctxBG(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = conn.Close(ctxBG())
		t.Fatalf("建物理 schema 失败: %v", err)
	}
	p := &phys{conn: conn, schema: schema}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = conn.Close(context.Background())
	})
	return p
}

// exec 在物理 schema 内执行 DDL/DML，表名带 schema 前缀。
func (p *phys) exec(t *testing.T, format string, args ...any) {
	t.Helper()
	sqlText := fmt.Sprintf(format, args...)
	if _, err := p.conn.Exec(ctxBG(), sqlText); err != nil {
		t.Fatalf("执行 %q 失败: %v", sqlText, err)
	}
}

func (p *phys) execErr(t *testing.T, format string, args ...any) error {
	t.Helper()
	_, err := p.conn.Exec(ctxBG(), fmt.Sprintf(format, args...))
	return err
}

func (p *phys) table(name string) string {
	return pgx.Identifier{p.schema, name}.Sanitize()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b)
}

// modelSpec 返回一份能通过 modelc.Validate 的 dwd 模型定义。
// dt 是默认规则强制的分区列，dwd 还要求主键——这两条是最容易被测试忽略的硬约束。
func modelSpec(datasource, name, schema, table string) model.ModelSpec {
	return model.ModelSpec{
		Kind: "Model", Name: name, Layer: "dwd", Datasource: datasource,
		Schema: schema, Table: table,
		Columns: []model.ColumnSpec{
			{Name: "dt", Type: "date", Nullable: false},
			{Name: "id", Type: "bigint", Nullable: false},
			{Name: "amount", Type: "decimal(18,2)", Nullable: true},
			{Name: "region", Type: "string", Nullable: true},
		},
		PrimaryKey:  []string{"id"},
		PartitionBy: []string{"dt"},
	}
}

// publishModel 落库并发布一个模型，返回发布后的模型（ActiveVersion=1）。
func publishModel(t *testing.T, svc *service.Service, wsID string, spec model.ModelSpec) *model.Model {
	t.Helper()
	m, err := svc.ApplyModelSpec(ctxBG(), wsID, "tester", spec, true, false, true)
	if err != nil {
		t.Fatalf("发布模型 %s 失败: %v", spec.Name, err)
	}
	return m
}

// publishPipeline 直接走仓储发布管道：这些用例关注执行面行为，
// 不需要 ApplyPipeline 里的「引用模型必须已发布」前置校验。
func publishPipeline(t *testing.T, svc *service.Service, wsID string, spec model.PipelineSpec) *model.Pipeline {
	t.Helper()
	p, err := svc.Repo.SavePipeline(ctxBG(), wsID, spec, "tester", true)
	if err != nil {
		t.Fatalf("发布管道 %s 失败: %v", spec.Name, err)
	}
	return p
}

// simplePipeline 是一条最小的已发布管道：单个 source 节点，不写表、不需要真库。
func simplePipeline(name string) model.PipelineSpec {
	return model.PipelineSpec{
		Kind: "Pipeline", Name: name,
		Nodes: []model.NodeSpec{
			{ID: "src", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 1 AS id"},
		},
	}
}

// metricFor / datasetFor / dashboardFor 是语义层用例的最小合法定义，
// 只包含校验必需项，避免把「能不能建出来」和「业务字段对不对」混在一起。
func metricFor(name, modelName string) semantic.Metric {
	return semantic.Metric{
		Name: name, DisplayName: name, ModelName: modelName,
		Agg: "sum", Expr: "amount", TimeColumn: "dt", Dimensions: []string{"region"},
	}
}

func datasetFor(name, modelName, timeField string) semantic.Dataset {
	return semantic.Dataset{
		Name: name, BaseModel: modelName,
		Dimensions: []semantic.Dimension{
			{Name: "dt", Field: timeField, Type: "time", Filterable: true, TimeGrains: []string{"day", "month"}},
			{Name: "region", Field: "region", Type: "categorical", Filterable: true},
		},
		Measures: []semantic.Measure{{Name: "amount", Field: "amount", Agg: "sum"}},
	}
}

func dashboardFor(name, metric string) semantic.Dashboard {
	return semantic.Dashboard{
		Name:  name,
		Cards: []semantic.Card{{Title: "GMV", Metric: metric, Days: 7}},
	}
}

// hasStr 是断言小工具：切片里是否包含某值。
func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// containsAll 断言 list 同时包含 want 里的全部元素，失败时输出完整 list 便于定位。
func containsAll(t *testing.T, label string, list, want []string) {
	t.Helper()
	for _, w := range want {
		if !hasStr(list, w) {
			t.Fatalf("%s 缺少 %q，实际 %v", label, w, list)
		}
	}
}

// joinLower 把切片拼成小写逗号串，方便断言「有且只有」这类关系。
func joinLower(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.ToLower(s)
	}
	return strings.Join(out, ",")
}
