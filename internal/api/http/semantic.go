package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
	"gopkg.in/yaml.v3"
)

// 语义层接口：数据集与图表。

func (h *Handler) listDatasetsSemantic(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListDatasetsScoped(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) getDataset(w http.ResponseWriter, r *http.Request, p map[string]string) {
	d, err := h.Svc.Repo.GetDataset(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, d)
}

func (h *Handler) applyDataset(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		YAML string            `json:"yaml"`
		Spec *semantic.Dataset `json:"spec"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	var d semantic.Dataset
	if body.Spec != nil {
		d = *body.Spec
	} else {
		if err := yaml.Unmarshal([]byte(body.YAML), &d); err != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, err, "数据集 YAML 解析失败"))
			return
		}
	}
	saved, err := h.Svc.ApplyDataset(r.Context(), workspaceOf(r.Context()), d)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "dataset.apply", saved.Name, map[string]any{"version": saved.Version})
	ok(w, r, saved)
}

func (h *Handler) deleteDataset(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	usage, err := h.Svc.DatasetUsage(r.Context(), workspaceOf(r.Context()), p["name"])
	if err != nil {
		fail(w, r, err)
		return
	}
	if n, _ := usage["charts"].(int); n > 0 {
		fail(w, r, errs.New(errs.CodeConflict,
			"仍有图表引用该数据集，请先解除引用").WithMeta("charts", n))
		return
	}
	if err := h.Svc.Repo.DeleteDataset(r.Context(), workspaceOf(r.Context()), p["name"]); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "dataset.delete", p["name"], nil)
	ok(w, r, map[string]any{"deleted": p["name"]})
}

// datasetQuery 执行数据集查询，支持「预览 SQL」（不执行）。
func (h *Handler) datasetQuery(w http.ResponseWriter, r *http.Request, p map[string]string) {
	var body semantic.QueryRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Dataset == "" {
		body.Dataset = p["name"]
	}
	res, err := h.Svc.DatasetQuery(r.Context(), workspaceOf(r.Context()), body, actorOf(r.Context()), roleOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// datasetSQL 只编译不执行，供前端「查看 SQL」。
func (h *Handler) datasetSQL(w http.ResponseWriter, r *http.Request, p map[string]string) {
	var body semantic.QueryRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Dataset == "" {
		body.Dataset = p["name"]
	}
	d, err := h.Svc.Repo.GetDataset(r.Context(), workspaceOf(r.Context()), body.Dataset)
	if err != nil {
		fail(w, r, err)
		return
	}
	models, err := h.Svc.PublishedModels(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	base, okm := models[d.BaseModel]
	if !okm {
		fail(w, r, errs.New(errs.CodeNotFound, "基础模型未发布"))
		return
	}
	c, err := h.Svc.ConnectorFor(r.Context(), workspaceOf(r.Context()), base.Datasource)
	if err != nil {
		fail(w, r, err)
		return
	}
	built, err := semantic.BuildQuery(*d, body, models, c.Dialect())
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, built)
}

func (h *Handler) listCharts(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListCharts(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) applyChart(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		YAML string          `json:"yaml"`
		Spec *semantic.Chart `json:"spec"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	var c semantic.Chart
	if body.Spec != nil {
		c = *body.Spec
	} else {
		if err := yaml.Unmarshal([]byte(body.YAML), &c); err != nil {
			fail(w, r, errs.Wrap(errs.CodeInvalidArgument, err, "图表 YAML 解析失败"))
			return
		}
	}
	saved, impact, err := h.Svc.ApplyChart(r.Context(), workspaceOf(r.Context()), c)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "chart.apply", saved.Name, map[string]any{"version": saved.Version, "impact": impact})
	ok(w, r, map[string]any{"chart": saved, "impact": impact})
}

func (h *Handler) deleteChart(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	if err := h.Svc.Repo.DeleteChart(r.Context(), workspaceOf(r.Context()), p["name"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"deleted": p["name"]})
}

func (h *Handler) queryChart(w http.ResponseWriter, r *http.Request, p map[string]string) {
	res, err := h.Svc.ChartQuery(r.Context(), workspaceOf(r.Context()), p["name"], actorOf(r.Context()), roleOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) chartTypes(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ok(w, r, map[string]any{"types": semantic.ChartTypes(), "schema": semantic.ChartSchema()})
}

var _ = strings.TrimSpace
