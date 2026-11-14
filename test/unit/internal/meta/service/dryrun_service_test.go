package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// DryRun 的承诺是「不连库、不写库，把将要执行的东西完整摊开」。
// 因此它必须能渲染参数模板、标注会写哪张表，并且对环/缺模型给出可读错误。
func TestDryRunRendersParamsAndInference(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_dry", "dw", "dwd_dry"))

	spec := model.PipelineSpec{
		Name: "dry_pipe",
		Params: []model.ParamSpec{
			{Name: "lookback", Type: "int", Default: 7},
		},
		Nodes: []model.NodeSpec{
			{ID: "src", Type: model.NodeSource, Datasource: ds.Name,
				SQL: "SELECT * FROM dw.raw WHERE dt >= '${biz_date}' AND n = ${lookback}"},
			{ID: "w1", Type: model.NodeSQL, Model: "dwd_dry", DependsOn: []string{"src"},
				SQL: "SELECT id, amount FROM dw.raw WHERE dt = '${biz_date}'"},
			{ID: "q1", Type: model.NodeQuality, Model: "dwd_dry", DependsOn: []string{"w1"},
				Config: map[string]any{"rules": []any{map[string]any{"name": "r1", "kind": "not_null", "column": "id"}}}},
		},
	}
	res, err := svc.DryRun(ctxBG(), ws.ID, spec, "2026-08-01", map[string]any{"lookback": 3})
	if err != nil {
		t.Fatalf("DryRun 失败: %v", err)
	}
	if res.BizDate != "2026-08-01" || len(res.Nodes) != 3 {
		t.Fatalf("结果结构不对: %+v", res)
	}
	byID := map[string]service.DryRunNode{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	// extra 覆盖 params 默认值：用户本次传 3，渲染必须是 3 而不是 7。
	if !strings.Contains(byID["src"].SQL, "n = 3") || strings.Contains(byID["src"].SQL, "${") {
		t.Fatalf("参数模板未渲染或覆盖失效: %q", byID["src"].SQL)
	}
	if !hasStr(byID["src"].Refs, "lookback") || !hasStr(byID["src"].Refs, "biz_date") {
		t.Fatalf("引用列表不对: %v", byID["src"].Refs)
	}
	// 写模型的节点标 Managed，并进入 wouldWrite；增量条件应被推导出来。
	if !byID["w1"].Managed || !hasStr(res.WouldWrite, "dwd_dry") {
		t.Fatalf("写表节点应标记托管: %+v", byID["w1"])
	}
	if byID["w1"].Inference == nil || byID["w1"].Inference.Where == "" {
		t.Fatalf("应按分区推导增量条件: %+v", byID["w1"].Inference)
	}
	// 质量节点没有 SQL，不该被当成写表节点。
	if byID["q1"].Managed || byID["q1"].SQL != "" {
		t.Fatalf("质量节点不应有 SQL / 托管标记: %+v", byID["q1"])
	}
	// 阶段必须覆盖全部节点，否则前端泳道会漏节点。
	total := 0
	for _, s := range res.Stages {
		total += len(s)
	}
	if total != 3 {
		t.Fatalf("阶段划分丢失节点: %v", res.Stages)
	}
}

// 引用未发布模型时，单节点报错但不阻断其它节点渲染——试跑的价值就是一次看全。
func TestDryRunMissingModel(t *testing.T) {
	svc, ws := newSvc(t)
	spec := model.PipelineSpec{
		Name: "dry_missing",
		Nodes: []model.NodeSpec{
			{ID: "src", Type: model.NodeSource, Datasource: "pg", SQL: "SELECT 1"},
			{ID: "w1", Type: model.NodeSQL, Model: "ghost_model", DependsOn: []string{"src"}, SQL: "SELECT 1"},
		},
	}
	res, err := svc.DryRun(ctxBG(), ws.ID, spec, "2026-08-01", nil)
	if err != nil {
		t.Fatalf("DryRun 不应整体失败: %v", err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("应渲染两个节点: %+v", res.Nodes)
	}
	var w1 service.DryRunNode
	for _, n := range res.Nodes {
		if n.ID == "w1" {
			w1 = n
		}
	}
	if !strings.Contains(w1.Error, "模型未发布") || w1.Managed {
		t.Fatalf("未发布模型应只在节点上报错: %+v", w1)
	}
}

// 环和空定义属于结构问题，必须在试跑入口直接拒绝。
func TestDryRunRejectsBadGraph(t *testing.T) {
	svc, ws := newSvc(t)
	cyclic := model.PipelineSpec{
		Name: "dry_cycle",
		Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "SELECT 1", DependsOn: []string{"b"}},
			{ID: "b", Type: model.NodeSQL, SQL: "SELECT 2", DependsOn: []string{"a"}},
		},
	}
	if _, err := svc.DryRun(ctxBG(), ws.ID, cyclic, "2026-08-01", nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("环应被拒绝，实际 %v", err)
	}
	if _, err := svc.DryRun(ctxBG(), ws.ID, model.PipelineSpec{Name: "empty"}, "2026-08-01", nil); !errs.Is(err, errs.CodePipelineInvalid) {
		t.Fatalf("空节点应被拒绝，实际 %v", err)
	}

	// 只有分支节点（无 SQL、无模型）是合法的「空图」：返回节点但不写任何表。
	only := model.PipelineSpec{Name: "dry_noop", Nodes: []model.NodeSpec{
		{ID: "b", Type: model.NodeBranch},
	}}
	res, err := svc.DryRun(ctxBG(), ws.ID, only, "2026-08-01", nil)
	if err != nil {
		t.Fatalf("无 SQL 的图不应报错: %v", err)
	}
	if len(res.WouldWrite) != 0 || len(res.Nodes) != 1 {
		t.Fatalf("无写表节点时 wouldWrite 应为空: %+v", res)
	}
}

// 采样执行只跑 SELECT 且强制包 LIMIT：截断标记与行数上限都要能被观察到。
func TestSampleRunLimitAndTruncation(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint, amount decimal(18,2))`, p.table("dwd_sample_t"))
	p.exec(t, `INSERT INTO %s VALUES (1,1),(2,2),(3,3)`, p.table("dwd_sample_t"))

	spec := model.PipelineSpec{
		Name: "sample_pipe",
		Nodes: []model.NodeSpec{
			{ID: "src", Type: model.NodeSQL, Datasource: ds.Name,
				SQL: "SELECT id, amount FROM " + p.table("dwd_sample_t") + " ORDER BY id"},
		},
	}
	res, err := svc.SampleRun(ctxBG(), ws.ID, spec, "", "2026-08-01", 2)
	if err != nil {
		t.Fatalf("SampleRun 失败: %v", err)
	}
	if res.Count != 2 || len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("应截断到 2 行并标记 truncated: %+v", res)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "id" {
		t.Fatalf("列名不对: %v", res.Columns)
	}
	if !strings.Contains(res.SQL, "LIMIT 3") {
		t.Fatalf("应多取一行以判断截断: %s", res.SQL)
	}

	// limit 越界回到默认 100，且不足上限时不标截断。
	res, err = svc.SampleRun(ctxBG(), ws.ID, spec, "src", "2026-08-01", 0)
	if err != nil || res.Truncated || res.Count != 3 {
		t.Fatalf("默认 limit 下应返回全部 3 行: %+v %v", res, err)
	}
}

// 采样失败必须是可读的业务错误：坏 SQL、找不到节点、缺数据源。
func TestSampleRunErrors(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	onlyQuality := model.PipelineSpec{Name: "sq", Nodes: []model.NodeSpec{
		{ID: "q", Type: model.NodeQuality, Model: "m"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, onlyQuality, "", "2026-08-01", 10); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("无可采样节点应报 NotFound，实际 %v", err)
	}

	noDS := model.PipelineSpec{Name: "no_ds", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSQL, SQL: "SELECT 1"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, noDS, "", "2026-08-01", 10); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺数据源应报参数错误，实际 %v", err)
	}

	badSQL := model.PipelineSpec{Name: "bad_sql", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSQL, Datasource: ds.Name, SQL: "SELECT * FROM table_that_does_not_exist"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, badSQL, "src", "2026-08-01", 10); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("坏 SQL 应报执行失败，实际 %v", err)
	}

	badVar := model.PipelineSpec{Name: "bad_var", Nodes: []model.NodeSpec{
		{ID: "src", Type: model.NodeSQL, Datasource: ds.Name, SQL: "SELECT ${undeclared}"},
	}}
	if _, err := svc.SampleRun(ctxBG(), ws.ID, badVar, "src", "2026-08-01", 10); err == nil {
		t.Fatal("未声明参数应渲染失败")
	}
}
