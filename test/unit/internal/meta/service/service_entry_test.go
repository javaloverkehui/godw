package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

// 数据源必须「先连通再落库」：连不上的配置留在库里只会制造更多误判。
func TestCreateAndTestDatasource(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.CreateDatasource(ctxBG(), ws.ID, "", "postgres", nil, nil); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺 name 应被拒绝，实际 %v", err)
	}
	if _, err := svc.CreateDatasource(ctxBG(), ws.ID, "bad", "postgres", map[string]any{
		"host": "127.0.0.1", "port": "1", "user": "godw", "database": "godw", "sslmode": "disable",
	}, map[string]string{"password": "x"}); err == nil {
		t.Fatal("连不上的数据源不应落库")
	}
	if list, err := svc.Repo.ListDatasources(ctxBG(), ws.ID); err != nil || len(list) != 0 {
		t.Fatalf("失败的数据源不应入库: %+v %v", list, err)
	}

	ds := newPgDatasource(t, svc, ws.ID)
	stored, _, err := svc.Repo.DatasourceByName(ctxBG(), ws.ID, ds.Name)
	if err != nil || stored.Status != "ok" {
		t.Fatalf("探测成功后状态应落库为 ok: %+v %v", stored, err)
	}
	if err := svc.TestDatasource(ctxBG(), ws.ID, ds.Name); err != nil {
		t.Fatalf("TestDatasource 失败: %v", err)
	}
	if err := svc.TestDatasource(ctxBG(), ws.ID, "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的数据源应 NotFound，实际 %v", err)
	}

	// 凭证必须解密回原文，否则连接器拿不到密码。
	spec, err := svc.DatasourceSpec(ctxBG(), ws.ID, ds.Name)
	if err != nil || spec.Secret["password"] != "godw" {
		t.Fatalf("凭证解密不对: %+v %v", spec.Secret, err)
	}
	if spec.Type != "postgres" || spec.Config["database"] != "godw" {
		t.Fatalf("配置读取不对: %+v", spec)
	}
	// ConnectorFor 暴露给「只编译不执行」的接口。
	c, err := svc.ConnectorFor(ctxBG(), ws.ID, ds.Name)
	if err != nil || c.Dialect().Name() != "postgres" {
		t.Fatalf("ConnectorFor 不对: %v %v", c, err)
	}
	if _, err := svc.ConnectorFor(ctxBG(), ws.ID, "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的数据源应 NotFound，实际 %v", err)
	}
}

// ApplyModel 是 YAML → 校验 → 编译 → 落库的完整入口；破坏性变更必须显式 force。
func TestApplyModelAndPreviewDDL(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	if _, err := svc.ApplyModel(ctxBG(), ws.ID, "alice", []byte("name: ["), false, false, false); !errs.Is(err, errs.CodeModelInvalid) {
		t.Fatalf("坏 YAML 应报模型不合法，实际 %v", err)
	}

	yamlText := `kind: Model
name: dwd_yaml
layer: dwd
datasource: ` + ds.Name + `
schema: dw
columns:
  - name: dt
    type: date
  - name: id
    type: bigint
  - name: amount
    type: decimal(18,2)
  - name: region
    type: string
primary_key: [id]
partition_by: [dt]
`
	m, err := svc.ApplyModel(ctxBG(), ws.ID, "alice", []byte(yamlText), true, false, false)
	if err != nil {
		t.Fatalf("ApplyModel 失败: %v", err)
	}
	if m.ActiveVersion != 1 || m.Name != "dwd_yaml" {
		t.Fatalf("发布结果不对: %+v", m)
	}
	if m.DDL == "" || !strings.Contains(m.DDL, "CREATE TABLE") {
		t.Fatalf("应顺手编译出 DDL: %q", m.DDL)
	}

	spec, err := svc.ModelSpec(ctxBG(), ws.ID, "dwd_yaml")
	if err != nil || spec.Name != "dwd_yaml" {
		t.Fatalf("ModelSpec 应返回已发布版本: %+v %v", spec, err)
	}
	if _, err := svc.ModelSpec(ctxBG(), ws.ID, "ghost"); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("未发布模型应 NotFound，实际 %v", err)
	}

	// 删列是破坏性变更：不带 force 必须拒绝，并给出变更清单。
	next := *spec
	next.Columns = next.Columns[:2] // 保留 dt/id，删掉 amount
	if _, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "alice", next, true, false, false); !errs.Is(err, errs.CodeModelImmutable) {
		t.Fatalf("破坏性变更应拒绝，实际 %v", err)
	}
	applied, err := svc.ApplyModelSpec(ctxBG(), ws.ID, "alice", next, true, false, true)
	if err != nil {
		t.Fatalf("force 后应成功: %v", err)
	}
	if applied.ActiveVersion != 2 {
		t.Fatalf("版本应递增到 2: %+v", applied)
	}

	// 预览 DDL 不落库：设计器需要它来展示将执行的语句。
	ddl, err := svc.PreviewModelDDL(ctxBG(), ws.ID, modelSpec(ds.Name, "dwd_prev_ddl", "dw", "dwd_prev_ddl"))
	if err != nil {
		t.Fatalf("PreviewModelDDL 失败: %v", err)
	}
	if !strings.Contains(ddl, `"dwd_prev_ddl"`) || !strings.Contains(ddl, "PRIMARY KEY") {
		t.Fatalf("DDL 不完整: %s", ddl)
	}
	if _, err := svc.PreviewModelDDL(ctxBG(), ws.ID, model.ModelSpec{Name: "x"}); err == nil {
		t.Fatal("非法模型应报错")
	}
	if _, err := svc.PreviewModelDDL(ctxBG(), ws.ID, modelSpec("ghost", "dwd_x", "dw", "dwd_x")); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("数据源不存在应 NotFound，实际 %v", err)
	}

}

// ApplyPipeline：提交即校验依赖模型已发布，发布后写入表级与列级血缘。
func TestApplyPipelineValidatesAndRecordsLineage(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_src_pl", "dw", "dwd_src_pl"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dst_pl", "dw", "dwd_dst_pl"))

	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "alice", []byte("nodes: ["), false); err == nil {
		t.Fatal("坏 YAML 应报错")
	}
	missing := `kind: Pipeline
name: pl_missing
nodes:
  - id: w
    type: sql
    model: ghost_model
    sql: SELECT 1
`
	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "alice", []byte(missing), true); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("引用未发布模型应拒绝，实际 %v", err)
	}

	good := `kind: Pipeline
name: pl_good
nodes:
  - id: src
    type: sql
    model: dwd_src_pl
    datasource: ` + ds.Name + `
    sql: SELECT * FROM dw.dwd_src_pl
  - id: w
    type: sql
    model: dwd_dst_pl
    depends_on: [src]
    sql: SELECT s.id AS id, s.amount AS amount FROM dw.dwd_src_pl s
`
	p, err := svc.ApplyPipeline(ctxBG(), ws.ID, "alice", []byte(good), true)
	if err != nil {
		t.Fatalf("ApplyPipeline 失败: %v", err)
	}
	if p.ActiveVersion != 1 {
		t.Fatalf("发布版本应为 1: %+v", p)
	}

	edges, err := svc.Repo.ListLineage(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("ListLineage 失败: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.Downstream == "dwd_dst_pl" && e.Upstream == "dwd_src_pl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("表级血缘应记录上游模型: %+v", edges)
	}
	colEdges, err := svc.Repo.ColumnLineage(ctxBG(), ws.ID, "dwd_dst_pl", "")
	if err != nil || len(colEdges) == 0 {
		t.Fatalf("列级血缘应记录: %+v %v", colEdges, err)
	}

	// 未发布（draft）时不应写血缘：草稿不产生运行事实。
	draft := strings.Replace(good, "pl_good", "pl_draft", 1)
	if _, err := svc.ApplyPipeline(ctxBG(), ws.ID, "alice", []byte(draft), false); err != nil {
		t.Fatalf("保存草稿失败: %v", err)
	}
}

// semanticQueryForTest 是缓存键用例的最小查询，字段齐全才能证明「序列化覆盖了全部入参」。
func semanticQueryForTest() semantic.Query {
	return semantic.Query{From: "2026-08-01", To: "2026-08-10", Granularity: "day",
		Dimension: "region", Limit: 100, ExtraWhere: []string{"region = 'x'"}}
}

// 占位变量：biz_date / biz_ts 必填，dt 缺省补齐，实例参数覆盖默认值。
func TestRenderVars(t *testing.T) {
	vars := service.RenderVars("2026-08-01", map[string]any{"lookback": 3})
	if vars["biz_date"] != "2026-08-01" || vars["biz_ts"] != "2026-08-01 00:00:00" {
		t.Fatalf("业务日期变量不对: %+v", vars)
	}
	if vars["dt"] != "2026-08-01" {
		t.Fatalf("dt 应补默认值: %+v", vars)
	}
	if vars["lookback"] != 3 {
		t.Fatalf("实例参数应透传: %+v", vars)
	}
	vars = service.RenderVars("2026-08-01", map[string]any{"dt": "2026-07-31"})
	if vars["dt"] != "2026-07-31" {
		t.Fatalf("显式 dt 不应被覆盖: %+v", vars)
	}
}
