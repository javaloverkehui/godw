package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
	pipelinecompile "github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/pkg/errs"
	"gopkg.in/yaml.v3"
)

// 建模规范相关接口：自检、脚手架、结构对比、数据字典、分层规范。

func (h *Handler) checkModel(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		YAML string           `json:"yaml"`
		Spec *model.ModelSpec `json:"spec"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	spec, err := h.modelSpecFrom(body.YAML, body.Spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	res, err := h.Svc.CheckModel(r.Context(), workspaceOf(r.Context()), spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) scaffoldModel(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body modelc.ScaffoldOptions
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	res, err := h.Svc.ScaffoldModel(r.Context(), workspaceOf(r.Context()), body)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) modelDiff(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		YAML string           `json:"yaml"`
		Spec *model.ModelSpec `json:"spec"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	spec, err := h.modelSpecFrom(body.YAML, body.Spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	res, err := h.Svc.ModelDiff(r.Context(), workspaceOf(r.Context()), spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

func (h *Handler) rollbackPreview(w http.ResponseWriter, r *http.Request, p map[string]string) {
	ver, err := atoiDefault(r.URL.Query().Get("version"), 0)
	if err != nil || ver <= 0 {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "version 必须为正整数"))
		return
	}
	diff, err := h.Svc.RollbackPreview(r.Context(), workspaceOf(r.Context()), p["name"], int(ver))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, diff)
}

func (h *Handler) dictionary(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	entries, err := h.Svc.Dictionary(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, entries)
}

func (h *Handler) dictionaryExport(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	md, err := h.Svc.DictionaryMarkdown(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dictionary.md"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(md))
}

func (h *Handler) layerRules(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ok(w, r, h.Svc.LayerRules())
}

// modelSpecFrom 支持同时接受 YAML 与结构化 JSON，前端设计器用后者，CLI 用前者。
func (h *Handler) modelSpecFrom(raw string, spec *model.ModelSpec) (model.ModelSpec, error) {
	if spec != nil {
		return *spec, nil
	}
	if raw == "" {
		return model.ModelSpec{}, errs.New(errs.CodeInvalidArgument, "必须提供 yaml 或 spec")
	}
	var out model.ModelSpec
	if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
		return out, errs.Wrap(errs.CodeModelInvalid, err, "模型 YAML 解析失败")
	}
	return out, nil
}

func atoiDefault(s string, def int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errs.New(errs.CodeInvalidArgument, "非法数字: "+s)
		}
		v = v*10 + int64(c-'0')
	}
	return v, nil
}

// dryRunPipeline 渲染整条管道但不执行，用于上线前确认「将执行什么」。
func (h *Handler) dryRunPipeline(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		YAML    string         `json:"yaml"`
		BizDate string         `json:"bizDate"`
		Params  map[string]any `json:"params"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	spec, err := pipelinecompile.Parse([]byte(body.YAML))
	if err != nil {
		fail(w, r, err)
		return
	}
	if body.BizDate == "" {
		body.BizDate = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	}
	res, err := h.Svc.DryRun(r.Context(), workspaceOf(r.Context()), *spec, body.BizDate, body.Params)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// sampleRunPipeline 只跑 SELECT 并 LIMIT，绝不写目标表。
func (h *Handler) sampleRunPipeline(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		YAML    string `json:"yaml"`
		NodeID  string `json:"nodeId"`
		BizDate string `json:"bizDate"`
		Limit   int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	spec, err := pipelinecompile.Parse([]byte(body.YAML))
	if err != nil {
		fail(w, r, err)
		return
	}
	if body.BizDate == "" {
		body.BizDate = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	}
	res, err := h.Svc.SampleRun(r.Context(), workspaceOf(r.Context()), *spec, body.NodeID, body.BizDate, body.Limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// ---------- 血缘图（T0610） ----------

func (h *Handler) lineageGraph(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	g, err := h.Svc.LineageGraphFor(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("pipeline"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, g)
}

func (h *Handler) lineageFocus(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	table := r.URL.Query().Get("table")
	if table == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 table"))
		return
	}
	g, err := h.Svc.LineageFocus(r.Context(), workspaceOf(r.Context()), table,
		r.URL.Query().Get("direction"), queryInt(r, "depth", 3))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, g)
}

func (h *Handler) lineagePaths(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" || to == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "from 与 to 必填"))
		return
	}
	paths, err := h.Svc.LineagePaths(r.Context(), workspaceOf(r.Context()), from, to, queryInt(r, "limit", 10))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"paths": paths, "count": len(paths)})
}

func (h *Handler) lineageDOT(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	dot, err := h.Svc.LineageDOT(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("table"))
	if err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/vnd.graphviz; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dot))
}

func (h *Handler) lineageCache(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ok(w, r, h.Svc.LineageCacheStats())
}

// ---------- 质量门禁放行（T0603） ----------

func (h *Handler) waiveQuality(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Reason string `json:"reason"`
		NodeID string `json:"nodeId"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "强制放行必须填写理由"))
		return
	}
	if err := h.Svc.WaiveQuality(r.Context(), workspaceOf(r.Context()), p["id"], body.NodeID,
		body.Reason, actorOf(r.Context())); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "quality.waive", p["id"], map[string]any{"reason": body.Reason, "node": body.NodeID})
	ok(w, r, map[string]any{"instance": p["id"], "waived": true})
}

// qualityWaivers 返回实例的放行记录。
func (h *Handler) qualityWaivers(w http.ResponseWriter, r *http.Request, p map[string]string) {
	items, err := h.Svc.QualityWaivers(r.Context(), workspaceOf(r.Context()), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}
