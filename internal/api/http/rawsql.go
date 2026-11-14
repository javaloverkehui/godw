package http

import (
	"net/http"

	"github.com/godw/godw/internal/sqlx"
)

// rawSQLQuery 是自助查询的唯一入口。
//
// 权限：admin / editor 默认可用，viewer 不可用。docs/14 §4.3 明确写了这条，
// 后续如果要做「申请后临时开通」，再在这一层接审批流。
func (h *Handler) rawSQLQuery(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Datasource string `json:"datasource"`
		SQL        string `json:"sql"`
		Limit      int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	res, err := h.Svc.RawSQLQuery(r.Context(), workspaceOf(r.Context()), body.Datasource, body.SQL,
		actorOf(r.Context()), roleOf(r.Context()), body.Limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	// 审计只留指纹与前 200 字符：既能定位是哪条查询，又不把整段 SQL（可能含敏感常量）落库。
	preview := []rune(body.SQL)
	if len(preview) > 200 {
		preview = append(preview[:200], '…')
	}
	h.audit(r, "sql.query", body.Datasource, map[string]any{
		"fingerprint": sqlx.SQLFingerprint(body.SQL),
		"preview":     string(preview),
		"elapsedMs":   res.ElapsedMs,
		"rowCount":    res.RowCount,
		"truncated":   res.Truncated,
		"maskedCols":  len(res.Masks),
	})
	ok(w, r, res)
}
