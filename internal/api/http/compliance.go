package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 合规接口：保留策略、删除权、导出审批。

func (h *Handler) listRetention(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListRetentionPolicies(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) saveRetention(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var p model.RetentionPolicy
	if err := decodeBody(r, &p); err != nil {
		fail(w, r, err)
		return
	}
	if strings.TrimSpace(p.ModelName) == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "modelName 必填"))
		return
	}
	if err := h.Svc.Repo.SaveRetentionPolicy(r.Context(), workspaceOf(r.Context()), p); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "retention.save", p.ModelName, map[string]any{"days": p.RetainDays, "archiveTo": p.ArchiveTo})
	ok(w, r, p)
}

// retentionPlan 只生成计划不执行，用于「先看看会删多少」。
func (h *Handler) retentionPlan(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	name := r.URL.Query().Get("model")
	if name == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "model 必填"))
		return
	}
	plan, err := h.Svc.PlanRetention(r.Context(), workspaceOf(r.Context()), name)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, plan)
}

func (h *Handler) retentionRun(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	n, err := h.Svc.RunRetention(r.Context(), workspaceOf(r.Context()), body.Model, actorOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"model": body.Model, "deleted": n})
}

func (h *Handler) listErasureJobs(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListErasureJobs(r.Context(), workspaceOf(r.Context()), 100)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) createErasureJob(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Model  string `json:"model"`
		Column string `json:"column"`
		Value  string `json:"value"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	job, err := h.Svc.RequestErasure(r.Context(), workspaceOf(r.Context()), body.Model, body.Column, body.Value, body.Reason, actorOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "erasure.request", body.Model, map[string]any{"reason": body.Reason, "column": body.Column})
	ok(w, r, job)
}
