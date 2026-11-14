package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/semantic"
	"github.com/godw/godw/internal/testsupport"
)

// newRepoDB 清空共享 schema 后同时返回仓储与底层连接。
// 很多列（cpu_ms、created_at、pii_override）没有读取方法，只能靠原生 SQL 断言，
// 所以这里一并把 DB 交出来，避免用例里再查一次 Shared。
func newRepoDB(t *testing.T) (*repo.Repo, *repo.DB) {
	t.Helper()
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	return repo.New(db), db
}

// seedWorkspace 建一个工作区。几乎所有表都有 workspace 外键，用例第一步都要它。
func seedWorkspace(t *testing.T, r *repo.Repo, name string) *model.Workspace {
	t.Helper()
	ws, err := r.CreateWorkspace(context.Background(), name, "")
	if err != nil {
		t.Fatalf("创建工作区 %q 失败: %v", name, err)
	}
	return ws
}

// seedDatasource 建一个可连的数据源，凭证用占位字节，只为验证 secret 列能往返。
func seedDatasource(t *testing.T, r *repo.Repo, wsID, name string) *model.Datasource {
	t.Helper()
	ds, err := r.CreateDatasource(context.Background(), model.Datasource{
		WorkspaceID: wsID,
		Name:        name,
		Type:        "postgres",
		Config:      map[string]any{"host": "127.0.0.1", "port": float64(5432)},
	}, []byte("secret-"+name))
	if err != nil {
		t.Fatalf("创建数据源 %q 失败: %v", name, err)
	}
	return ds
}

// seedPipeline 创建并发布一条管道，返回的实体带 active_version。
func seedPipeline(t *testing.T, r *repo.Repo, wsID, name string) *model.Pipeline {
	t.Helper()
	return savePipeline(t, r, wsID, model.PipelineSpec{Name: name}, true)
}

func savePipeline(t *testing.T, r *repo.Repo, wsID string, spec model.PipelineSpec, publish bool) *model.Pipeline {
	t.Helper()
	p, err := r.SavePipeline(context.Background(), wsID, spec, "tester", publish)
	if err != nil {
		t.Fatalf("保存管道 %q 失败: %v", spec.Name, err)
	}
	return p
}

// seedInstance 建一个实例。调用方自己填 biz_date / 状态，避免每个用例再断言默认值。
func seedInstance(t *testing.T, r *repo.Repo, inst model.Instance) *model.Instance {
	t.Helper()
	out, _, err := r.CreateInstance(context.Background(), inst)
	if err != nil {
		t.Fatalf("创建实例 %q 失败: %v", inst.IdempotencyKey, err)
	}
	return out
}

// execSQL 直接执行原生 SQL，用于构造「仓储层没有写入口」的状态（例如过期租约）
// 以及断言没有读取方法的列。
func execSQL(t *testing.T, db *repo.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("执行 SQL 失败: %v\n%s", err, sql)
	}
}

func queryInt(t *testing.T, db *repo.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("查询整数失败: %v\n%s", err, sql)
	}
	return n
}

func queryStr(t *testing.T, db *repo.DB, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("查询字符串失败: %v\n%s", err, sql)
	}
	return s
}

func metricSample(name string) semantic.Metric {
	return semantic.Metric{
		Name:        name,
		DisplayName: name,
		ModelName:   "dwd_trade",
		Agg:         "sum",
		Expr:        "amount",
		TimeColumn:  "dt",
		Dimensions:  []string{"dt"},
		Unit:        "yuan",
	}
}

func dashboardSample(name, desc string) semantic.Dashboard {
	return semantic.Dashboard{
		Name:        name,
		Description: desc,
		Cards:       []semantic.Card{{Title: "GMV", Metric: "gmv", Days: 7}},
	}
}
