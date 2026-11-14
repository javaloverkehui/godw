package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/semantic"
)

// CreateExport 生成导出任务。high 级数据不会直接执行，而是落成 pending_approval，
// 由管理员在治理页批准后才开始跑；普通数据仍按 queued 立刻排队。
//
// 导出的内容与在线查询走同一套指标/看板定义，保证「看到的」和「导出的」口径一致。
func (s *Service) CreateExport(ctx context.Context, workspaceID, kind, target string, params map[string]any, actor string) (*model.ExportJob, error) {
	level, tables, lerr := s.ExportSensitivity(ctx, workspaceID, kind, target)
	if lerr != nil {
		// 分级解析失败不阻断导出，但必须留痕：否则又回到「看起来检查过」的老路。
		s.Log.Warn("导出分级解析失败", "kind", kind, "target", target, "err", lerr)
	}
	status := "queued"
	if level == model.ClassificationHigh {
		status = "pending_approval"
	}
	// 把分级与涉及的表一起存进 params：审批页和审计都能直接看到依据。
	meta := map[string]any{}
	for k, v := range params {
		meta[k] = v
	}
	meta["_classification"] = level
	meta["_tables"] = tables
	job, err := s.Repo.CreateExport(ctx, model.ExportJob{
		WorkspaceID: workspaceID, Kind: kind, Target: target, Params: meta,
		Status: status, CreatedBy: actor,
	})
	if err != nil {
		return nil, err
	}
	if job.Status == "queued" {
		s.runExportAsync(job)
	}
	return job, nil
}

func (s *Service) runExportAsync(job *model.ExportJob) {
	go func() {
		// 后台执行：不继承请求上下文（请求结束会 cancel），单独给 5 分钟上限。
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.runExport(bg, job)
	}()
}

// ApproveExport 批准一条待审批导出并立即执行。
func (s *Service) ApproveExport(ctx context.Context, workspaceID, id, actor string) (*model.ExportJob, error) {
	job, err := s.Repo.GetExport(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if job.Status != "pending_approval" {
		return nil, errs.New(errs.CodeConflict,
			"只有待审批的导出可以批准，当前状态: "+job.Status)
	}
	if err := s.Repo.SetExportStatus(ctx, id, "approved", ""); err != nil {
		return nil, err
	}
	job.Status = "approved"
	s.Repo.Audit(ctx, model.AuditLog{
		WorkspaceID: workspaceID, Actor: actor, Action: "export.approve",
		Resource: job.Target,
		Detail:   map[string]any{"kind": job.Kind, "exportId": job.ID, "classification": job.Params["_classification"]},
	})
	s.runExportAsync(job)
	return job, nil
}

// RejectExport 驳回一条待审批导出，理由写进 error 并列在导出记录里。
func (s *Service) RejectExport(ctx context.Context, workspaceID, id, reason, actor string) (*model.ExportJob, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "驳回必须说明理由")
	}
	job, err := s.Repo.GetExport(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if job.Status != "pending_approval" {
		return nil, errs.New(errs.CodeConflict,
			"只有待审批的导出可以驳回，当前状态: "+job.Status)
	}
	if err := s.Repo.SetExportStatus(ctx, id, "rejected", reason); err != nil {
		return nil, err
	}
	job.Status, job.Error = "rejected", reason
	s.Repo.Audit(ctx, model.AuditLog{
		WorkspaceID: workspaceID, Actor: actor, Action: "export.reject",
		Resource: job.Target,
		Detail:   map[string]any{"kind": job.Kind, "exportId": job.ID, "reason": reason},
	})
	return job, nil
}

func (s *Service) runExport(ctx context.Context, job *model.ExportJob) {
	_ = s.Repo.FinishExport(ctx, job.ID, "running", "", "", 0)

	dir := s.Cfg.Semantic.ExportDir
	if dir == "" {
		dir = "./data/exports"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = s.Repo.FinishExport(ctx, job.ID, "failed", "", "创建导出目录失败: "+err.Error(), 0)
		return
	}
	path := filepath.Join(dir, job.Kind+"_"+job.Target+"_"+time.Now().Format("20060102_150405")+"_"+job.ID[len(job.ID)-4:]+".csv")

	f, err := os.Create(path)
	if err != nil {
		_ = s.Repo.FinishExport(ctx, job.ID, "failed", "", "创建文件失败: "+err.Error(), 0)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	var rows int64
	switch job.Kind {
	case "metric":
		rows, err = s.exportMetric(ctx, job, w)
	case "dashboard":
		rows, err = s.exportDashboard(ctx, job, w)
	default:
		err = fmt.Errorf("不支持的导出类型: %s", job.Kind)
	}
	w.Flush()
	if err != nil {
		_ = os.Remove(path)
		_ = s.Repo.FinishExport(ctx, job.ID, "failed", "", err.Error(), 0)
		return
	}
	if err := w.Error(); err != nil {
		_ = s.Repo.FinishExport(ctx, job.ID, "failed", "", "写文件失败: "+err.Error(), 0)
		return
	}
	_ = s.Repo.FinishExport(ctx, job.ID, "success", path, "", rows)
}

func (s *Service) exportMetric(ctx context.Context, job *model.ExportJob, w *csv.Writer) (int64, error) {
	q := semantic.Query{Granularity: "day", Limit: 5000}
	if v, ok := job.Params["from"].(string); ok {
		q.From = v
	}
	if v, ok := job.Params["to"].(string); ok {
		q.To = v
	}
	if v, ok := job.Params["granularity"].(string); ok && v != "" {
		q.Granularity = v
	}
	if v, ok := job.Params["dimension"].(string); ok {
		q.Dimension = v
	}
	series, err := s.QueryMetric(ctx, job.WorkspaceID, job.Target, q)
	if err != nil {
		return 0, err
	}
	header := []string{"date", "value"}
	if q.Dimension != "" {
		header = []string{"date", "dimension", "value"}
	}
	if err := w.Write(header); err != nil {
		return 0, err
	}
	var n int64
	for _, p := range series.Points {
		rec := []string{p.Ts, strconv.FormatFloat(p.Value, 'f', -1, 64)}
		if q.Dimension != "" {
			rec = []string{p.Ts, p.Dim, strconv.FormatFloat(p.Value, 'f', -1, 64)}
		}
		if err := w.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Service) exportDashboard(ctx context.Context, job *model.ExportJob, w *csv.Writer) (int64, error) {
	data, err := s.QueryDashboard(ctx, job.WorkspaceID, job.Target)
	if err != nil {
		return 0, err
	}
	// 看板导出用宽表：每行一个日期，每列一张卡片。
	first := ""
	for _, c := range data.Cards {
		if len(c.Points) > 0 {
			first = c.Metric
			break
		}
	}
	if first == "" {
		return 0, nil
	}
	header := []string{"date"}
	for _, c := range data.Cards {
		header = append(header, c.Title)
	}
	if err := w.Write(header); err != nil {
		return 0, err
	}
	// 以第一张卡片的日期序列为主轴
	dates := []string{}
	seen := map[string]bool{}
	for _, p := range data.Cards[0].Points {
		if !seen[p.Ts] {
			seen[p.Ts] = true
			dates = append(dates, p.Ts)
		}
	}
	idx := make([]map[string]float64, len(data.Cards))
	for i, c := range data.Cards {
		idx[i] = map[string]float64{}
		for _, p := range c.Points {
			idx[i][p.Ts] = p.Value
		}
	}
	var n int64
	for _, d := range dates {
		rec := []string{d}
		for i := range data.Cards {
			rec = append(rec, strconv.FormatFloat(idx[i][d], 'f', -1, 64))
		}
		if err := w.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RunSubscription 执行一次订阅：取数后按渠道推送。
// 当前支持 webhook（POST JSON）与 file（落 CSV）。邮件与 IM 需要外部凭证，留待扩展。
func (s *Service) RunSubscription(ctx context.Context, workspaceID string, sub model.Subscription) (map[string]any, error) {
	var payload any
	switch sub.Kind {
	case "dashboard":
		data, err := s.QueryDashboard(ctx, workspaceID, sub.Target)
		if err != nil {
			_ = s.Repo.MarkSubscriptionRun(ctx, sub.ID, "failed")
			return nil, err
		}
		payload = data
	default:
		q := semantic.Query{Granularity: "day", Limit: 500}
		if v, ok := sub.Config["days"].(float64); ok && v > 0 {
			q.From = time.Now().AddDate(0, 0, -int(v)).Format("2006-01-02")
		}
		series, err := s.QueryMetric(ctx, workspaceID, sub.Target, q)
		if err != nil {
			_ = s.Repo.MarkSubscriptionRun(ctx, sub.ID, "failed")
			return nil, err
		}
		payload = series
	}

	switch sub.Channel {
	case "file":
		dir := s.Cfg.Semantic.ExportDir
		if dir == "" {
			dir = "./data/exports"
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "sub_"+sub.Name+"_"+time.Now().Format("20060102_150405")+".json")
		raw, err := jsonMarshal(payload)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return nil, err
		}
		_ = s.Repo.MarkSubscriptionRun(ctx, sub.ID, "success")
		return map[string]any{"channel": "file", "path": path, "bytes": len(raw)}, nil
	case "webhook":
		if sub.Endpoint == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "webhook 订阅缺少 endpoint")
		}
		raw, err := jsonMarshal(map[string]any{
			"subscription": sub.Name, "kind": sub.Kind, "target": sub.Target,
			"generatedAt": time.Now().Format(time.RFC3339), "payload": payload,
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, strings.NewReader(string(raw)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GoDW-Subscription", sub.Name)
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			_ = s.Repo.MarkSubscriptionRun(ctx, sub.ID, "failed")
			return nil, errs.Wrap(errs.CodeUnavailable, err, "webhook 推送失败")
		}
		defer resp.Body.Close()
		status := "success"
		if resp.StatusCode >= 300 {
			status = "failed"
		}
		_ = s.Repo.MarkSubscriptionRun(ctx, sub.ID, status)
		return map[string]any{"channel": "webhook", "status": resp.StatusCode, "ok": status == "success"}, nil
	}
	return nil, errs.New(errs.CodeInvalidArgument, "不支持的渠道: "+sub.Channel)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
