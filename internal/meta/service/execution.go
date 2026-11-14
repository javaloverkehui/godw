package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/obs/alert"
	"github.com/godw/godw/internal/obs/logstore"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/scheduler/queue"
)

// 执行面服务：队列分发、日志、告警判定。全部与 Service 共享仓储与连接池。

// Dispatch 按资源组空位提升排队实例。
func (s *Service) Dispatch(ctx context.Context, workspaceID string) (int, error) {
	d := queue.NewDispatcher(repoQueueStore{r: s.Repo, ws: workspaceID}, s.groupList())
	return d.Dispatch(ctx, workspaceID)
}

// UpsertResourceGroup 保存资源组配置并同步到内存，免重启生效。
func (s *Service) UpsertResourceGroup(ctx context.Context, workspaceID, name string, conc, timeout, pool int, engines []string) error {
	if name == "" {
		name = "default"
	}
	if conc <= 0 {
		conc = 4
	}
	if timeout <= 0 {
		timeout = 3600
	}
	if pool <= 0 {
		pool = 10
	}
	g := queue.Group{Name: name, MaxConcurrency: conc, MaxTimeoutSec: timeout, MaxPool: pool, Engines: engines}
	if err := queue.ValidateGroup(g); err != nil {
		return err
	}
	if err := s.Repo.UpsertResourceGroup(ctx, workspaceID, name, conc, timeout, pool, engines); err != nil {
		return err
	}
	s.mu.Lock()
	if s.ResourceGroups == nil {
		s.ResourceGroups = map[string]queue.Group{}
	}
	s.ResourceGroups[name] = g
	s.mu.Unlock()
	return nil
}

func (s *Service) groupList() []queue.Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]queue.Group, 0, len(s.ResourceGroups))
	for _, g := range s.ResourceGroups {
		out = append(out, g)
	}
	return out
}

// Logs 返回实例日志（先热后冷）。
func (s *Service) Logs(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]logstore.Line, error) {
	if s.Logs_ == nil {
		sink := repo.LogSink{DB: s.Repo.DB}
		s.Logs_ = logstore.NewManager(sink, 2000)
	}
	return s.Logs_.Get(instanceID).Since(ctx, afterSeq, limit)
}

// AppendLog 供执行器写入日志。
func (s *Service) AppendLog(instanceID, nodeID, level, msg string) {
	if s.Logs_ == nil {
		return
	}
	s.Logs_.Get(instanceID).Append(nodeID, level, msg)
}

// ReleaseLogs 实例结束后刷盘并释放内存。
func (s *Service) ReleaseLogs(ctx context.Context, instanceID string) {
	if s.Logs_ == nil {
		return
	}
	s.Logs_.Release(ctx, instanceID)
}

// EvaluateAlerts 在实例结束后跑一遍告警规则。
// 失败只记录日志，绝不影响主流程——告警系统自己不能成为故障源。
func (s *Service) EvaluateAlerts(ctx context.Context, workspaceID string, inst *model.Instance) {
	rules, err := s.Repo.ListAlertRules(ctx, workspaceID)
	if err != nil {
		s.Log.Warn("加载告警规则失败", "err", err)
		return
	}
	if len(rules) == 0 {
		return
	}
	fact := alert.Fact{
		Pipeline: inst.PipelineName, InstanceID: inst.ID, Status: inst.Status,
		BizDate: inst.BizDate, Error: inst.Error, Now: time.Now(),
	}
	if inst.StartedAt != nil {
		fact.StartedAt = *inst.StartedAt
	}
	if inst.FinishedAt != nil {
		fact.FinishedAt = *inst.FinishedAt
	} else {
		fact.FinishedAt = time.Now()
	}
	// 汇总写入行数：成功但 0 行同样是事故。
	var rows int64
	for _, t := range inst.Tasks {
		rows += t.RowsAffected
	}
	fact.RowsTotal = rows
	if prev, err := s.Repo.PreviousInstanceRows(ctx, workspaceID, inst.PipelineID, inst.BizDate); err == nil {
		fact.PrevRows = prev
	}

	for _, r := range rules {
		ev := alert.Evaluate(r, fact)
		if ev == nil {
			continue
		}
		saved, created, err := s.Repo.EmitAlert(ctx, workspaceID, *ev)
		if err != nil {
			s.Log.Warn("写入告警失败", "rule", r.Name, "err", err)
			continue
		}
		if !created {
			// 同窗口已告警过，合并处理。
			continue
		}
		s.sendAlert(ctx, workspaceID, r, *ev, saved.ID)
	}
}

func (s *Service) sendAlert(ctx context.Context, workspaceID string, rule alert.Rule, ev alert.Event, eventID string) {
	channels := rule.Channels
	if len(channels) == 0 {
		channels = []string{"webhook"}
	}
	d := alert.NewDispatcher(s.webhookChannels()...)
	res := d.Send(ctx, ev, channels)
	anyOK := false
	for name, r := range res {
		if r == "ok" {
			anyOK = true
		} else if r != "渠道未配置" {
			s.Log.Warn("告警推送失败", "channel", name, "err", r, "event", eventID)
		}
	}
	if anyOK {
		if err := s.Repo.MarkAlertNotified(ctx, eventID); err != nil {
			s.Log.Warn("标记告警已推送失败", "err", err)
		}
	}
}

// webhookChannels 从配置构造渠道。当前支持 webhook，邮件/IM 走同一个 webhook 网关。
func (s *Service) webhookChannels() []alert.Channel {
	out := []alert.Channel{}
	if s.Cfg.Alert.WebhookURL != "" {
		out = append(out, alert.WebhookChannel{
			ChannelName: "webhook",
			URL:         s.Cfg.Alert.WebhookURL,
			Template:    s.Cfg.Alert.Template,
		})
	}
	// 没有配置任何渠道时给一个内存渠道，避免「告警静默失败」——
	// 至少在日志里能看到事件已经产生。
	out = append(out, logChannel{log: s.Log})
	return out
}

// logChannel 把告警写进服务日志，作为兜底渠道。
type logChannel struct{ log *slog.Logger }

func (logChannel) Name() string { return "log" }
func (c logChannel) Send(ctx context.Context, ev alert.Event) error {
	c.log.Warn("告警触发", "title", ev.Title, "detail", ev.Detail,
		"pipeline", ev.Pipeline, "severity", ev.Severity)
	return nil
}

// repoQueueStore 把仓储适配成 queue.Store。
type repoQueueStore struct {
	r  *repo.Repo
	ws string
}

func (s repoQueueStore) RunningCount(ctx context.Context, workspaceID, group string) (int, error) {
	return s.r.RunningCount(ctx, workspaceID, group)
}

func (s repoQueueStore) Promote(ctx context.Context, workspaceID, group string, limit int) (int, error) {
	return s.r.Promote(ctx, workspaceID, group, limit)
}

func (s repoQueueStore) Pending(ctx context.Context, workspaceID string, limit int) ([]queue.Item, error) {
	items, err := s.r.PendingInstances(ctx, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]queue.Item, 0, len(items))
	for _, it := range items {
		out = append(out, queue.Item{
			InstanceID: it.InstanceID, Pipeline: it.PipelineName,
			ResourceGroup: it.ResourceGroup, Priority: it.Priority,
		})
	}
	return out, nil
}

// WaiveQuality 强制放行质量门禁：记录理由与操作人后，把失败实例重新排回队列，
// 并跳过 quality 节点的判定（通过在该节点 config 上打标实现）。
// 关键约束：必须有理由，且留下审计——放行不是「忽略」，是「带责任地继续」。
func (s *Service) WaiveQuality(ctx context.Context, workspaceID, instanceID, nodeID, reason, actor string) error {
	inst, err := s.Repo.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if inst.WorkspaceID != workspaceID {
		return errs.New(errs.CodeNotFound, "实例不存在")
	}
	if inst.Status != "failed" {
		return errs.New(errs.CodeConflict, "只有失败实例可以放行，当前状态 "+inst.Status)
	}
	if err := s.Repo.RecordQualityWaiver(ctx, workspaceID, instanceID, nodeID, reason, actor); err != nil {
		return err
	}
	// 把失败的质量节点置为 skipped，下游即可继续；已有成功节点保持不变。
	tasks, err := s.Repo.Tasks(ctx, instanceID)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Status == "failed" && (nodeID == "" || t.NodeID == nodeID) {
			if err := s.Repo.FinishTask(ctx, instanceID, t.NodeID, "skipped", t.RowsAffected, t.DurationMS,
				"质量门禁已放行: "+reason); err != nil {
				return err
			}
		}
	}
	return s.Repo.SetInstanceStatus(ctx, instanceID, "queued")
}

// QualityWaivers 返回某实例的放行记录。
func (s *Service) QualityWaivers(ctx context.Context, workspaceID, instanceID string) ([]map[string]any, error) {
	return s.Repo.ListQualityWaivers(ctx, workspaceID, instanceID)
}
