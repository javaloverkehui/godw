package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// ResolveSelect 把「模型表达式」映射到「实际会跑的管道」，并显式暴露放大效应
// （选中一个模型 → 整条管道都会跑）与被漏掉的模型。
func TestResolveSelectMapsToPipelines(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "dwd_a", "dwd"))
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "dws_b", "dws"))
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "dwd_uncovered", "dwd"))

	if err := svc.Repo.ReplaceLineage(ctxBG(), ws.ID, "dws_b", []string{"dwd_a"}); err != nil {
		t.Fatalf("写血缘失败: %v", err)
	}
	pipe := model.PipelineSpec{Kind: "Pipeline", Name: "p_ab", Nodes: []model.NodeSpec{
		{ID: "w1", Type: model.NodeSQL, Model: "dwd_a", SQL: "SELECT 1"},
		{ID: "w2", Type: model.NodeSQL, Model: "dws_b", DependsOn: []string{"w1"}, SQL: "SELECT 2"},
	}}
	publishPipeline(t, svc, ws.ID, pipe)

	res, err := svc.ResolveSelect(ctxBG(), ws.ID, "dwd_a+")
	if err != nil {
		t.Fatalf("ResolveSelect 失败: %v", err)
	}
	containsAll(t, "模型", res.Models, []string{"dwd_a", "dws_b"})
	if len(res.Pipelines) != 1 || res.Pipelines[0] != "p_ab" {
		t.Fatalf("应映射到 p_ab: %+v", res.Pipelines)
	}
	if len(res.Uncovered) != 0 {
		t.Fatalf("被管道覆盖的模型不应出现在 uncovered: %v", res.Uncovered)
	}
	if !strings.Contains(res.String(), "2 个模型 / 1 条管道") {
		t.Fatalf("摘要文案不对: %s", res.String())
	}

	// 没有管道写出的模型：解析成功但必须显式列出「跑了也不会覆盖」。
	res, err = svc.ResolveSelect(ctxBG(), ws.ID, "dwd_uncovered")
	if err != nil {
		t.Fatalf("ResolveSelect 失败: %v", err)
	}
	if len(res.Pipelines) != 0 || !hasStr(res.Uncovered, "dwd_uncovered") {
		t.Fatalf("无管道覆盖时应进 uncovered: %+v", res)
	}

	// 未发布管道不能算「命中」：否则解析成功但触发一定失败。
	publishModel(t, svc, ws.ID, modelAt(ds.Name, "dwd_c", "dwd"))
	if _, err := svc.Repo.SavePipeline(ctxBG(), ws.ID, model.PipelineSpec{
		Kind: "Pipeline", Name: "p_draft",
		Nodes: []model.NodeSpec{{ID: "w", Type: model.NodeSQL, Model: "dwd_c", SQL: "SELECT 1"}},
	}, "tester", false); err != nil {
		t.Fatalf("保存草稿管道失败: %v", err)
	}
	res, err = svc.ResolveSelect(ctxBG(), ws.ID, "dwd_c")
	if err != nil {
		t.Fatalf("ResolveSelect 失败: %v", err)
	}
	if len(res.Pipelines) != 0 {
		t.Fatalf("未发布管道不应命中: %+v", res.Pipelines)
	}

	// pipeline: 前缀走同一套映射。
	res, err = svc.ResolveSelect(ctxBG(), ws.ID, "pipeline:p_ab")
	if err != nil {
		t.Fatalf("ResolveSelect 失败: %v", err)
	}
	containsAll(t, "模型", res.Models, []string{"dwd_a", "dws_b"})
}

// 解析失败必须在触发之前返回，不能触发一半再报错。
func TestTriggerSelectPropagatesResolveError(t *testing.T) {
	svc, ws := newSvc(t)
	if _, _, err := svc.TriggerSelect(ctxBG(), ws.ID, "ghost_model", "2026-10-07", nil); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("不存在的模型应 NotFound，实际 %v", err)
	}
	if _, _, err := svc.TriggerSelect(ctxBG(), ws.ID, "", "2026-10-07", nil); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("空表达式应报参数错误，实际 %v", err)
	}
}
