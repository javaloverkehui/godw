package http

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/pkg/errs"
)

// 执行面接口：队列视图、资源组、告警规则与事件、运行日志流。

// ---------- 队列 ----------

func (h *Handler) queueView(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	ws := workspaceOf(r.Context())
	items, err := h.Svc.Repo.PendingInstances(r.Context(), ws, 200)
	if err != nil {
		fail(w, r, err)
		return
	}
	groups, err := h.Svc.Repo.ListResourceGroups(r.Context(), ws)
	if err != nil {
		fail(w, r, err)
		return
	}
	// 附上每个资源组当前 running 数，前端能画出「占用 / 上限」。
	usage := map[string]map[string]int{}
	for _, g := range groups {
		name, _ := g["name"].(string)
		n, _ := h.Svc.Repo.RunningCount(r.Context(), ws, name)
		limit, _ := g["maxConcurrency"].(int)
		usage[name] = map[string]int{"running": n, "limit": limit}
	}
	ok(w, r, map[string]any{"pending": items, "groups": groups, "usage": usage})
}

func (h *Handler) dispatchOnce(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor", "runner") {
		return
	}
	n, err := h.Svc.Dispatch(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"promoted": n})
}

// ---------- 资源组 ----------

func (h *Handler) listResourceGroups(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	groups, err := h.Svc.Repo.ListResourceGroups(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, groups)
}

func (h *Handler) saveResourceGroup(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Name           string   `json:"name"`
		MaxConcurrency int      `json:"maxConcurrency"`
		MaxTimeoutSec  int      `json:"maxTimeoutSec"`
		MaxPool        int      `json:"maxPool"`
		Engines        []string `json:"engines"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if err := h.Svc.UpsertResourceGroup(r.Context(), workspaceOf(r.Context()), body.Name,
		body.MaxConcurrency, body.MaxTimeoutSec, body.MaxPool, body.Engines); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "resource_group.save", body.Name, map[string]any{"concurrency": body.MaxConcurrency})
	ok(w, r, map[string]any{"name": body.Name})
}

// ---------- 告警 ----------

func (h *Handler) listAlertRules(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	rules, err := h.Svc.Repo.ListAlertRules(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, rules)
}

func (h *Handler) saveAlertRule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var rule alert.Rule
	if err := decodeBody(r, &rule); err != nil {
		fail(w, r, err)
		return
	}
	if rule.Name == "" {
		fail(w, r, errs.New(errs.CodeInvalidArgument, "规则名不能为空"))
		return
	}
	saved, err := h.Svc.Repo.SaveAlertRule(r.Context(), workspaceOf(r.Context()), rule)
	if err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "alert_rule.save", rule.Name, map[string]any{"kind": rule.Kind})
	ok(w, r, saved)
}

func (h *Handler) deleteAlertRule(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	if err := h.Svc.Repo.DeleteAlertRule(r.Context(), workspaceOf(r.Context()), p["name"]); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, map[string]any{"deleted": p["name"]})
}

func (h *Handler) listAlerts(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	items, err := h.Svc.Repo.ListAlerts(r.Context(), workspaceOf(r.Context()),
		r.URL.Query().Get("status"), 200)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, items)
}

func (h *Handler) handleAlert(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !requireRole(w, r, "admin", "editor", "runner") {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	switch body.Status {
	case "acknowledged", "resolved", "open":
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "status 必须为 acknowledged/resolved/open"))
		return
	}
	if err := h.Svc.Repo.HandleAlert(r.Context(), workspaceOf(r.Context()), p["id"], body.Status, actorOf(r.Context())); err != nil {
		fail(w, r, err)
		return
	}
	h.audit(r, "alert.handle", p["id"], map[string]any{"status": body.Status})
	ok(w, r, map[string]any{"id": p["id"], "status": body.Status})
}

// testAlertRule 用一条构造事实立即验证规则，避免「配完等明天才发现写错」。
func (h *Handler) testAlertRule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if !requireRole(w, r, "admin", "editor") {
		return
	}
	var body struct {
		Rule alert.Rule `json:"rule"`
		Fact alert.Fact `json:"fact"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	body.Fact.Now = time.Now()
	if body.Fact.Pipeline == "" {
		body.Fact.Pipeline = "demo_pipeline"
	}
	ev := alert.Evaluate(body.Rule, body.Fact)
	ok(w, r, map[string]any{"triggered": ev != nil, "event": ev})
}

// ---------- 运行日志 ----------

// instanceLogs 支持 Last-Event-ID 断点续传：
// 客户端重连时带上已收到的 seq，服务端从下一条开始推，不丢不重。
func (h *Handler) instanceLogs(w http.ResponseWriter, r *http.Request, p map[string]string) {
	id := p["id"]
	flusher, okc := w.(http.Flusher)
	if !okc {
		fail(w, r, errs.New(errs.CodeInternal, "当前连接不支持流式输出"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	afterSeq := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			afterSeq = n
		}
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			afterSeq = n
		}
	}
	levelFilter := r.URL.Query().Get("level")

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	// 空闲超过 5 分钟自动断开，避免僵尸连接堆积。
	idle := time.NewTimer(5 * time.Minute)
	defer idle.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-idle.C:
			fmt.Fprint(w, "event: timeout\ndata: {}\n\n")
			flusher.Flush()
			return
		case <-ticker.C:
			lines, err := h.Svc.Logs(r.Context(), id, afterSeq, 200)
			if err != nil {
				return
			}
			for _, l := range lines {
				afterSeq = l.Seq
				if levelFilter != "" && l.Level != levelFilter {
					continue
				}
				payload := fmt.Sprintf(`{"seq":%d,"nodeId":%q,"level":%q,"message":%q,"at":%q}`,
					l.Seq, l.NodeID, l.Level, l.Message, l.At.Format(time.RFC3339Nano))
				fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", l.Seq, payload)
			}
			if len(lines) > 0 {
				idle.Reset(5 * time.Minute)
				flusher.Flush()
			}
			// 实例已结束且没有新日志 → 发 done 并关闭，前端不必再挂着。
			if inst, ierr := h.Svc.Repo.GetInstance(r.Context(), id); ierr == nil {
				if isTerminal(inst.Status) && len(lines) == 0 {
					fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", inst.Status)
					flusher.Flush()
					return
				}
			}
		}
	}
}

// instanceLogsSnapshot 返回一次性日志快照，供「复制全部日志」与排查导出。
func (h *Handler) instanceLogsSnapshot(w http.ResponseWriter, r *http.Request, p map[string]string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines, err := h.Svc.Logs(r.Context(), p["id"], after, 5000)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, lines)
}

func isTerminal(status string) bool {
	return status == "success" || status == "failed" || status == "canceled"
}
