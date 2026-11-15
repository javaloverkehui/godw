package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/meta/model"
)

// 质量规则接口。
//
// 规则属于模型（不是管道）：同一条规则被所有产出该模型的管道复用。
// 读接口带上 ?model=xxx；写接口整套替换，body 里的 rules 就是最终状态。

func (h *Handler) listQualityRules(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.QualityRules(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("model"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) saveQualityRules(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Model string              `json:"model"`
		Rules []model.QualityRule `json:"rules"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	modelName := strings.TrimSpace(body.Model)
	if modelName == "" {
		modelName = strings.TrimSpace(r.URL.Query().Get("model"))
	}
	items, err := h.Svc.SaveQualityRules(r.Context(), workspaceOf(r.Context()),
		actorOf(r.Context()), modelName, body.Rules)
	if err != nil {
		fail(w, r, err)
		return
	}
	// 规则变了不会改变数据，但会改变「什么算合格」，属于要能追溯的变更。
	h.audit(r, "quality.rules.save", modelName, map[string]any{"count": len(items)})
	ok(w, r, items)
}
