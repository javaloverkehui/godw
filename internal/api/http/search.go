package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// ---------- T0210 元数据搜索 ----------

// search 支持 /api/v1/search?q=订单&types=model,dataset&layer=dwd&tags=a,b
func (h *Handler) search(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	hits, err := h.Svc.Search(r.Context(), workspaceOf(r.Context()), service.SearchQuery{
		Q:          strings.TrimSpace(q.Get("q")),
		Types:      splitCSV(q.Get("types")),
		Layer:      strings.TrimSpace(q.Get("layer")),
		Tags:       splitCSV(q.Get("tags")),
		Datasource: strings.TrimSpace(q.Get("datasource")),
		Owner:      strings.TrimSpace(q.Get("owner")),
		Limit:      queryInt(r, "limit", 50),
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"total": len(hits), "items": hits})
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 标签 ----------

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	tags, err := h.Svc.ListTags(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, tags)
}

func (h *Handler) saveTag(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 name"))
		return
	}
	t, err := h.Svc.SaveTag(r.Context(), workspaceOf(r.Context()), body.Name, body.Color, body.Description)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, t)
}

func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if err := h.Svc.DeleteTag(r.Context(), workspaceOf(r.Context()), p["name"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"deleted": p["name"]})
}

func (h *Handler) tagEntity(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Type  string   `json:"type"`
		Name  string   `json:"name"`
		Tags  []string `json:"tags"`
		Owner string   `json:"owner,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Owner != "" {
		body.Tags = append(body.Tags, "owner:"+body.Owner)
	}
	if err := h.Svc.TagEntity(r.Context(), workspaceOf(r.Context()), body.Type, body.Name, body.Tags); err != nil {
		fail(w, r, err)
		return
	}
	tags, _ := h.Svc.EntityTags(r.Context(), workspaceOf(r.Context()), body.Type, body.Name)
	ok(w, r, map[string]any{"type": body.Type, "name": body.Name, "tags": tags})
}

func (h *Handler) untagEntity(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if err := h.Svc.UntagEntity(r.Context(), workspaceOf(r.Context()), p["type"], p["entity"], p["name"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"deleted": true})
}

func (h *Handler) entityTags(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	etype := r.URL.Query().Get("type")
	ename := r.URL.Query().Get("name")
	tags, err := h.Svc.EntityTags(r.Context(), workspaceOf(r.Context()), etype, ename)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"type": etype, "name": ename, "tags": tags})
}

func (h *Handler) entitiesByTag(w http.ResponseWriter, r *http.Request, p map[string]string) {
	hits, err := h.Svc.EntitiesByTag(r.Context(), workspaceOf(r.Context()), p["name"], queryInt(r, "limit", 200))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"tag": p["name"], "total": len(hits), "items": hits})
}
