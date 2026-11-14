package service_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 文档站要一页回答四个问题：字段/注释、血缘、质量、新鲜度。
// 这里用一条真实链路（模型 → 管道 → 实例 → 质量结果）验证聚合没有丢字段。
func TestCatalogDocsAggregatesModelInfo(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	spec := modelSpec(ds.Name, "dwd_order_doc", "dw", "dwd_order_doc")
	spec.Description = "订单明细"
	spec.Owner = "alice"
	spec.Tags = []string{"core", "order"}
	publishModel(t, svc, ws.ID, spec)
	// 再发一版：版本数应当变成 2，active 仍是 2。
	spec.Description = "订单明细 v2"
	publishModel(t, svc, ws.ID, spec)

	pipe := model.PipelineSpec{
		Kind: "Pipeline", Name: "order_doc_pipe",
		Nodes: []model.NodeSpec{
			{ID: "w1", Type: model.NodeSQL, Model: "dwd_order_doc", SQL: "SELECT * FROM dw.dwd_order_doc"},
			{ID: "q1", Type: model.NodeQuality, Model: "dwd_order_doc", DependsOn: []string{"w1"},
				Config: map[string]any{"rules": []any{
					map[string]any{"name": "order_fresh", "kind": "freshness", "max_age_min": 30},
				}}},
		},
	}
	publishPipeline(t, svc, ws.ID, pipe)

	// 造一次真实运行：写表节点成功，质量规则通过。
	inst, _, err := svc.Trigger(ctxBG(), ws.ID, "order_doc_pipe", "2026-06-01", "manual", nil)
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if err := svc.Repo.FinishTask(ctxBG(), inst.ID, "w1", "success", 10, 5, ""); err != nil {
		t.Fatalf("写任务状态失败: %v", err)
	}
	if err := svc.Repo.FinishInstance(ctxBG(), inst.ID, "success", ""); err != nil {
		t.Fatalf("写实例状态失败: %v", err)
	}
	if err := svc.Repo.SaveQualityResults(ctxBG(), inst.ID, "q1", []struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}{{"order_fresh", "5 分钟", "30 分钟", "error", true}}); err != nil {
		t.Fatalf("写质量结果失败: %v", err)
	}

	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogDocs 失败: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("应只有一个模型，实际 %d", len(page.Entries))
	}
	doc := page.Entries[0]
	if doc.Name != "dwd_order_doc" || doc.Table != "dwd_order_doc" || doc.Layer != "dwd" {
		t.Fatalf("基础字段不对: %+v", doc)
	}
	if doc.Version != 2 || doc.Versions != 2 {
		t.Fatalf("版本应升到 2 且累计 2 版: %+v", doc)
	}
	if doc.Description != "订单明细 v2" || doc.Owner != "alice" {
		t.Fatalf("描述 / Owner 应取发布版本: %+v", doc)
	}
	containsAll(t, "标签", doc.Tags, []string{"core", "order"})
	if len(doc.Columns) != 4 {
		t.Fatalf("字段数应为 4: %+v", doc.Columns)
	}
	// 质量规则必须从 pipeline 的质量节点解析出来，且带上最近一次判定结果。
	if len(doc.Quality) != 1 || doc.Quality[0].Name != "order_fresh" {
		t.Fatalf("质量规则解析不对: %+v", doc.Quality)
	}
	if doc.Quality[0].LastPassed == nil || !*doc.Quality[0].LastPassed || doc.Quality[0].Observed != "5 分钟" {
		t.Fatalf("质量结果应回填: %+v", doc.Quality[0])
	}
	// 有 freshness 规则且跑过 → 以规则为准，阈值为 30 分钟、状态 fresh。
	if doc.Freshness.Basis != "quality_rule" || doc.Freshness.Status != "fresh" || doc.Freshness.ThresholdMin != 30 {
		t.Fatalf("新鲜度应以质量规则为准: %+v", doc.Freshness)
	}
	if doc.LastRun == nil || doc.LastRun.Status != "success" || doc.LastRun.Pipeline != "order_doc_pipe" {
		t.Fatalf("最近运行不对: %+v", doc.LastRun)
	}
	if len(page.Layers) != 1 || page.Layers[0] != "dwd" {
		t.Fatalf("分层列表不对: %v", page.Layers)
	}
}

// 没有运行记录的模型不能装懂：status=unknown、basis=no_run。
func TestCatalogDocsNoRun(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_user_doc", "dw", "dwd_user_doc"))

	page, err := svc.CatalogDocs(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogDocs 失败: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("应只有一个模型")
	}
	f := page.Entries[0].Freshness
	if f.Status != "unknown" || f.Basis != "no_run" {
		t.Fatalf("未运行时应如实标注: %+v", f)
	}
	// 空集合必须序列化成 []，不能是 null：前端直接取 .tags.length 会因为 null 抛错。
	raw, err := json.Marshal(page.Entries[0])
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{`"upstream":[]`, `"downstream":[]`, `"quality":[]`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("空集合应序列化为空切片（缺少 %s）: %s", field, raw)
		}
	}
}

// 静态 HTML 的关键是「自带全部样式、断网可读、特殊字符不能回流成标签」。
func TestCatalogHTMLEscapesAndRenders(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	spec := modelSpec(ds.Name, "dwd_html_doc", "dw", "dwd_html_doc")
	spec.Description = `<script>alert("x")</script>`
	spec.Columns[1].Comment = "<b>id</b>"
	publishModel(t, svc, ws.ID, spec)

	html, err := svc.CatalogHTML(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("CatalogHTML 失败: %v", err)
	}
	if !strings.Contains(html, "<!DOCTYPE html>") || !strings.Contains(html, `id="m-dwd_html_doc"`) {
		t.Fatal("应输出完整 HTML 且带模型锚点")
	}
	// 用户填的描述与注释必须转义，否则导出的静态页就是一个存储型 XSS。
	if strings.Contains(html, `<script>alert("x")</script>`) || strings.Contains(html, "<b>id</b>") {
		t.Fatal("用户可控文本未转义")
	}
	if !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "&lt;b&gt;id&lt;/b&gt;") {
		t.Fatal("转义后的文本应出现在页面里")
	}
	// 暗色主题与打印样式：文档要能直接投屏/打印。
	if !strings.Contains(html, "prefers-color-scheme: dark") || !strings.Contains(html, "@media print") {
		t.Fatal("缺少暗色主题或打印样式")
	}
	if !strings.Contains(html, "DWD 明细层") || !strings.Contains(html, "没有血源记录") && !strings.Contains(html, "没有血缘记录") {
		t.Fatal("缺少分层标签或空血缘占位")
	}
}

// 空工作空间导出也要是一份可打开的页面，而不是报错或半截 HTML。
func TestCatalogHTMLEmptyWorkspace(t *testing.T) {
	svc, ws := newSvc(t)
	html, err := svc.CatalogHTML(ctxBG(), ws.ID)
	if err != nil {
		t.Fatalf("空库导出不应失败: %v", err)
	}
	if !strings.Contains(html, "</html>") {
		t.Fatal("HTML 未闭合")
	}
	if !strings.Contains(html, "0 个字段") || !strings.Contains(html, "0 条规则") {
		t.Fatalf("统计应为 0: %s", html[:200])
	}
}
