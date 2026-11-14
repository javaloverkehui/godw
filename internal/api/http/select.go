package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
)

// 选择器接口。解析与触发放在一起，是因为「解析出来几条管道」正是触发前最该确认的信息：
// 一个 `+dwd_order` 可能命中十几条管道，让人先看到规模再决定，比直接排队更安全。
type selectRunBody struct {
	Select  string         `json:"select"`
	BizDate string         `json:"bizDate"`
	Params  map[string]any `json:"params"`
	DryRun  bool           `json:"dryRun"`
}

// selectPreview 只解析不触发，供界面实时显示命中范围。
func (h *Handler) selectPreview(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	expr := strings.TrimSpace(r.URL.Query().Get("select"))
	if expr == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 select 参数（例如 dwd_order+ 或 layer:dwd）"))
		return
	}
	res, err := h.Svc.ResolveSelect(r.Context(), workspaceOf(r.Context()), expr)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// selectRun 按选择器触发。dryRun=true 时只返回解析结果。
func (h *Handler) selectRun(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body selectRunBody
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	expr := strings.TrimSpace(body.Select)
	if expr == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 select"))
		return
	}
	if body.DryRun {
		res, err := h.Svc.ResolveSelect(r.Context(), workspaceOf(r.Context()), expr)
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"resolved": res, "runs": []any{}})
		return
	}
	runs, res, err := h.Svc.TriggerSelect(r.Context(), workspaceOf(r.Context()), expr, body.BizDate, body.Params)
	if err != nil {
		fail(w, r, err)
		return
	}
	created := 0
	for _, x := range runs {
		if x.Error == "" && x.Created {
			created++
		}
	}
	h.audit(r, "pipeline.trigger_select", expr, map[string]any{
		"models": len(res.Models), "pipelines": res.Pipelines, "created": created,
	})
	ok(w, r, map[string]any{"resolved": res, "runs": runs, "created": created})
}
