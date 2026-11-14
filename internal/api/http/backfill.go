package http

import (
	"net/http"

	"github.com/godw/godw/internal/pkg/errs"
)

// ---------- 补数（T0906） ----------

// backfillPlan 只算不动：先让用户看清「要补几天、跳过几天」，再决定提交。
func (h *Handler) backfillPlan(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	pipeline := q.Get("pipeline")
	if pipeline == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 pipeline"))
		return
	}
	plan, err := h.Svc.PlanBackfill(r.Context(), workspaceOf(r.Context()), pipeline, q.Get("from"), q.Get("to"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, plan)
}

func (h *Handler) createBackfill(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Pipeline string `json:"pipeline"`
		From     string `json:"from"`
		To       string `json:"to"`
		Reason   string `json:"reason"`
		Force    bool   `json:"force"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	b, err := h.Svc.Backfill(r.Context(), workspaceOf(r.Context()), body.Pipeline, body.From, body.To,
		body.Reason, actorOf(r.Context()), body.Force)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "backfill.create", body.Pipeline, map[string]any{
		"from": body.From, "to": body.To, "total": b.Total, "created": b.Created, "force": body.Force,
	})
	created(w, r, b)
}

func (h *Handler) backfillProgress(w http.ResponseWriter, r *http.Request, p map[string]string) {
	b, err := h.Svc.BackfillProgress(r.Context(), workspaceOf(r.Context()), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, b)
}

func (h *Handler) listBackfills(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.ListBackfills(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 20))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

// ganttData 给运维甘特图：每行一个实例的起止时间。
func (h *Handler) ganttData(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.BackfillGantt(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 120))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

// cancelInstance 取消排队中的实例；运行中的实例明确拒绝。
func (h *Handler) cancelInstance(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	prev, err := h.Svc.Repo.CancelQueuedInstance(r.Context(), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "instance.cancel", p["id"], map[string]any{"prevStatus": prev})
	ok(w, r, map[string]any{"id": p["id"], "prevStatus": prev, "status": "canceled"})
}
