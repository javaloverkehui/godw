package http

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

// ---------- 列级血缘 ----------

func (h *Handler) columnLineage(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	table := r.URL.Query().Get("table")
	if table == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 table"))
		return
	}
	edges, err := h.Svc.Repo.ColumnLineage(r.Context(), workspaceOf(r.Context()), table, r.URL.Query().Get("column"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, edges)
}

// columnGraph 返回以某列为焦点的列级血缘子图，供血缘页下钻。
func (h *Handler) columnGraph(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	g, err := h.Svc.ColumnGraph(r.Context(), workspaceOf(r.Context()), q.Get("table"),
		q.Get("column"), q.Get("direction"), queryInt(r, "depth", 6))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, g)
}

func (h *Handler) impact(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	table := r.URL.Query().Get("table")
	if table == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "缺少 table"))
		return
	}
	res, err := h.Svc.Impact(r.Context(), workspaceOf(r.Context()), table, r.URL.Query().Get("column"), queryInt(r, "depth", 5))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// previewTableRows 预览表数据，行级过滤与列级脱敏在此生效。
func (h *Handler) previewTableRows(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	ds, schema, table := q.Get("datasource"), q.Get("schema"), q.Get("table")
	if ds == "" || table == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "datasource 与 table 必填"))
		return
	}
	res, err := h.Svc.PreviewTable(r.Context(), workspaceOf(r.Context()), ds, schema, table,
		actorOf(r.Context()), roleOf(r.Context()), queryInt(r, "limit", 100))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}

// ---------- 列策略 ----------

func (h *Handler) listColumnPolicies(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListColumnPolicies(r.Context(), workspaceOf(r.Context()), r.URL.Query().Get("table"))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) saveColumnPolicy(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var p model.ColumnPolicy
	if err := decodeBody(r, &p); err != nil {
		fail(w, r, err)
		return
	}
	if p.TableName == "" || p.ColumnName == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "table 与 column 必填"))
		return
	}
	switch p.MaskType {
	case "", "none", "partial", "hash", "email", "phone", "full":
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "不支持的脱敏方式: "+p.MaskType))
		return
	}
	if err := h.Svc.Repo.SaveColumnPolicy(r.Context(), workspaceOf(r.Context()), p); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "policy.column.save", p.TableName+"."+p.ColumnName, map[string]any{"mask": p.MaskType})
	created(w, r, p)
}

// ---------- 行策略 ----------

func (h *Handler) listRowPolicies(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListRowPolicies(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) saveRowPolicy(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var p model.RowPolicy
	if err := decodeBody(r, &p); err != nil {
		fail(w, r, err)
		return
	}
	if p.TableName == "" || p.FilterExpr == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "table 与 filter 必填"))
		return
	}
	saved, err := h.Svc.Repo.SaveRowPolicy(r.Context(), workspaceOf(r.Context()), p)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "policy.row.save", p.TableName, map[string]any{"role": p.Role})
	created(w, r, saved)
}

func (h *Handler) deleteRowPolicy(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	if err := h.Svc.Repo.DeleteRowPolicy(r.Context(), workspaceOf(r.Context()), p["id"]); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "policy.row.delete", p["id"], nil)
	ok(w, r, map[string]bool{"deleted": true})
}

// ---------- 导出 ----------

func (h *Handler) listExports(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListExports(r.Context(), workspaceOf(r.Context()), queryInt(r, "limit", 30))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

type exportReq struct {
	Kind   string         `json:"kind"`
	Target string         `json:"target"`
	Params map[string]any `json:"params"`
}

func (h *Handler) createExport(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req exportReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if req.Kind == "" {
		req.Kind = "metric"
	}
	if req.Target == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "target 必填"))
		return
	}
	job, err := h.Svc.CreateExport(r.Context(), workspaceOf(r.Context()), req.Kind, req.Target, req.Params, actorOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	detail := map[string]any{"kind": req.Kind, "status": job.Status}
	if v, okc := job.Params["_classification"]; okc {
		detail["classification"] = v
	}
	h.audit(r, "export.create", req.Target, detail)
	created(w, r, job)
}

// approveExport 批准 high 级数据导出。只有 admin 能批，批准即开始执行。
func (h *Handler) approveExport(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	job, err := h.Svc.ApproveExport(r.Context(), workspaceOf(r.Context()), p["id"], actorOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, job)
}

func (h *Handler) rejectExport(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	job, err := h.Svc.RejectExport(r.Context(), workspaceOf(r.Context()), p["id"], body.Reason, actorOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, job)
}

func (h *Handler) getExport(w http.ResponseWriter, r *http.Request, p map[string]string) {
	job, err := h.Svc.Repo.GetExport(r.Context(), workspaceOf(r.Context()), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, job)
}

func (h *Handler) downloadExport(w http.ResponseWriter, r *http.Request, p map[string]string) {
	job, err := h.Svc.Repo.GetExport(r.Context(), workspaceOf(r.Context()), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	if job.Status != "success" || job.FilePath == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "导出尚未完成"))
		return
	}
	// 只允许下载导出目录内的文件，防止路径穿越读到系统文件。
	abs, err := filepath.Abs(job.FilePath)
	if err != nil || !strings.HasPrefix(abs, h.exportDirAbs()) {
		fail(w, r, errs.New(errs.CodeForbidden, "文件路径非法"))
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(abs)))
	http.ServeFile(w, r, abs)
}

func (h *Handler) exportDirAbs() string {
	dir := h.Cfg.Semantic.ExportDir
	if dir == "" {
		dir = "./data/exports"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// ---------- 订阅 ----------

func (h *Handler) listSubscriptions(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListSubscriptions(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) saveSubscription(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var s model.Subscription
	if err := decodeBody(r, &s); err != nil {
		fail(w, r, err)
		return
	}
	if s.Name == "" || s.Target == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "name 与 target 必填"))
		return
	}
	if s.Channel == "" {
		s.Channel = "webhook"
	}
	saved, err := h.Svc.Repo.SaveSubscription(r.Context(), workspaceOf(r.Context()), s)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "subscription.save", s.Name, nil)
	created(w, r, saved)
}

func (h *Handler) runSubscription(w http.ResponseWriter, r *http.Request, p map[string]string) {
	items, err := h.Svc.Repo.ListSubscriptions(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, s := range items {
		if s.ID != p["id"] {
			continue
		}
		res, err := h.Svc.RunSubscription(r.Context(), workspaceOf(r.Context()), s)
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, res)
		return
	}
	fail(w, r, errs.New(errs.CodeNotFound, "订阅不存在"))
}

func (h *Handler) deleteSubscription(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	if err := h.Svc.Repo.DeleteSubscription(r.Context(), workspaceOf(r.Context()), p["id"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]bool{"deleted": true})
}

// ---------- 分享 ----------

func (h *Handler) listShares(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListShares(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) createShare(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var body struct {
		Kind    string `json:"kind"`
		Target  string `json:"target"`
		TTLDays int    `json:"ttlDays"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Kind == "" || body.Target == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "kind 与 target 必填"))
		return
	}
	ttl := time.Duration(body.TTLDays) * 24 * time.Hour
	link, err := h.Svc.Repo.CreateShare(r.Context(), workspaceOf(r.Context()), body.Kind, body.Target, actorOf(r.Context()), ttl)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "share.create", body.Target, map[string]any{"token": link.Token})
	created(w, r, link)
}

func (h *Handler) deleteShare(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if err := h.Svc.Repo.DeleteShare(r.Context(), workspaceOf(r.Context()), p["token"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]bool{"deleted": true})
}

// publicShare 无需登录：凭 token 读取看板/指标。过期或不存在都返回 404。
func (h *Handler) publicShare(w http.ResponseWriter, r *http.Request, p map[string]string) {
	link, err := h.Svc.Repo.GetShare(r.Context(), p["token"])
	if err != nil {
		fail(w, r, err)
		return
	}
	switch link.Kind {
	case "dashboard":
		data, err := h.Svc.QueryDashboard(r.Context(), link.WorkspaceID, link.Target)
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"kind": "dashboard", "target": link.Target, "data": data})
	case "metric":
		q := semanticQueryFrom(r)
		series, err := h.Svc.QueryMetricFor(r.Context(), link.WorkspaceID, link.Target, q, "", "viewer")
		if err != nil {
			fail(w, r, err)
			return
		}
		ok(w, r, map[string]any{"kind": "metric", "target": link.Target, "data": series})
	default:
		fail(w, r, errs.New(errs.CodeNotFound, "未知的分享类型"))
	}
}

// ---------- 工作区 ----------

func (h *Handler) listWorkspaces(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListWorkspacesFor(r.Context(), actorOf(r.Context()), roleOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) createWorkspace(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Name == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "name 必填"))
		return
	}
	ws, err := h.Svc.Repo.CreateWorkspace(r.Context(), body.Name, body.Description)
	if err != nil {
		fail(w, r, err)
		return
	}
	// 创建者自动成为该工作区管理员，否则建完自己进不去
	_ = h.Svc.Repo.AddMember(r.Context(), ws.ID, actorOf(r.Context()), "admin")
	h.audit(r, "workspace.create", ws.Name, nil)
	created(w, r, ws)
}

// switchWorkspace 签发一个指向新租户的 token：不改密码，只换作用域。
func (h *Handler) switchWorkspace(w http.ResponseWriter, r *http.Request, p map[string]string) {
	ws, err := h.Svc.Repo.WorkspaceByID(r.Context(), p["id"])
	if err != nil {
		fail(w, r, err)
		return
	}
	role := h.Svc.Repo.RoleOf(r.Context(), ws.ID, actorOf(r.Context()))
	if role == "" {
		role = roleOf(r.Context())
		if role != "admin" {
			fail(w, r, errs.New(errs.CodeForbidden, "不是该工作区成员"))
			return
		}
	}
	token, err := h.Svc.Signer.Sign(actorOf(r.Context()), ws.ID, role)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "workspace.switch", ws.Name, nil)
	ok(w, r, map[string]any{"token": token, "workspace": ws, "role": role})
}

func (h *Handler) tenantUsage(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	u, err := h.Svc.Repo.TenantUsage(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, u)
}

// ---------- 成员 ----------

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListMembers(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var m model.Member
	if err := decodeBody(r, &m); err != nil {
		fail(w, r, err)
		return
	}
	if m.UserID == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "userId 必填"))
		return
	}
	switch m.Role {
	case "admin", "editor", "viewer":
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "role 只支持 admin/editor/viewer"))
		return
	}
	if err := h.Svc.Repo.AddMember(r.Context(), workspaceOf(r.Context()), m.UserID, m.Role); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "member.add", m.UserID, map[string]any{"role": m.Role})
	created(w, r, m)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin") {
		return
	}
	if err := h.Svc.Repo.RemoveMember(r.Context(), workspaceOf(r.Context()), p["userId"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]bool{"deleted": true})
}

// ---------- 缓存 ----------

func (h *Handler) cacheStats(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	st, err := h.Svc.Repo.CacheStats(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	st["ttlSec"] = h.Cfg.Semantic.CacheTTLSec
	ok(w, r, st)
}

func (h *Handler) cachePurge(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	n, err := h.Svc.Repo.CachePurge(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"purged": n})
}

func semanticQueryFrom(r *http.Request) semantic.Query {
	return semantic.Query{
		From:        r.URL.Query().Get("from"),
		To:          r.URL.Query().Get("to"),
		Granularity: r.URL.Query().Get("granularity"),
		Dimension:   r.URL.Query().Get("dimension"),
		Limit:       queryInt(r, "limit", 500),
	}
}

// simulatePolicies 是「策略命中测试」接口：给定表与身份，返回实际生效的脱敏列与行过滤。
// 只读，viewer 也能调——它回答的正是「我能不能看到这些数据」，不该只有管理员能问。
func (h *Handler) simulatePolicies(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	q := r.URL.Query()
	user := q.Get("user")
	if user == "" {
		user = actorOf(r.Context())
	}
	role := q.Get("role")
	if role == "" {
		role = roleOf(r.Context())
	}
	res, err := h.Svc.SimulatePolicies(r.Context(), workspaceOf(r.Context()), q.Get("table"), user, role)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, res)
}
