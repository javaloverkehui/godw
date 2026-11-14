package http

import (
	"net/http"

	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// ---------- 环境绑定（T0203） ----------

func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if err := h.Svc.EnsureEnvironments(r.Context(), workspaceOf(r.Context())); err != nil {
		fail(w, r, err)
		return
	}
	envs, err := h.Svc.ListEnvironments(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, envs)
}

func (h *Handler) bindEnvironment(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Env        string `json:"env"`
		Alias      string `json:"alias"`
		Datasource string `json:"datasource"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if err := h.Svc.BindEnv(r.Context(), workspaceOf(r.Context()), body.Env, body.Alias, body.Datasource); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "env.bind", body.Env, map[string]any{"alias": body.Alias, "datasource": body.Datasource})
	ok(w, r, map[string]any{"env": body.Env, "alias": body.Alias, "datasource": body.Datasource})
}

func (h *Handler) envBindings(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	m, err := h.Svc.EnvBindings(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("env"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, m)
}

// ---------- 元数据采集与变更（T0208） ----------

func (h *Handler) collectWithDiff(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Datasource string `json:"datasource"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Datasource == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 datasource"))
		return
	}
	res, err := h.Svc.CollectWithDiff(r.Context(), workspaceOf(r.Context()), body.Datasource)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "datasource.collect", body.Datasource, map[string]any{
		"tables": res.Tables, "changes": len(res.Changes),
	})
	ok(w, r, res)
}

func (h *Handler) listSchemaChanges(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.ListSchemaChanges(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 100))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

// ---------- 敏感字段识别（T0209） ----------

// detectPII 只识别不落库：让人先看一眼判定理由，再决定是否采纳。
func (h *Handler) detectPII(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Datasource  string `json:"datasource"`
		SampleLimit int    `json:"sampleLimit"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	findings, err := h.Svc.DetectPII(r.Context(), workspaceOf(r.Context()), body.Datasource, body.SampleLimit)
	if err != nil {
		fail(w, r, err)
		return
	}
	stats := map[string]int{"high": 0, "medium": 0, "low": 0}
	for _, f := range findings {
		stats[string(f.Level)]++
	}
	ok(w, r, map[string]any{"total": len(findings), "byLevel": stats, "items": findings})
}

func (h *Handler) applyPII(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Datasource string               `json:"datasource"`
		Findings   []service.PIIFinding `json:"findings"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	n, err := h.Svc.ApplyPIIFindings(r.Context(), workspaceOf(r.Context()), body.Datasource, body.Findings)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "pii.apply", body.Datasource, map[string]any{"updated": n})
	ok(w, r, map[string]any{"updated": n})
}
