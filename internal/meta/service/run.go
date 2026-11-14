package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/pipeline/node"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/scheduler/queue"
)

// Trigger 创建并排队一个实例。
// 幂等规则：schedule/backfill 按 (pipeline, bizDate) 唯一；manual 允许重复执行。
func (s *Service) Trigger(ctx context.Context, workspaceID, pipelineName, bizDate, triggerType string, params map[string]any) (*model.Instance, bool, error) {
	p, spec, ver, err := s.Repo.PipelineByName(ctx, workspaceID, pipelineName)
	if err != nil {
		return nil, false, err
	}
	if ver == 0 {
		return nil, false, errs.New(errs.CodePipelineInvalid, "管道未发布: "+pipelineName)
	}
	if bizDate == "" {
		bizDate = time.Now().Format("2006-01-02")
	}
	key := fmt.Sprintf("%s:%s", p.ID, bizDate)
	if triggerType == "manual" {
		key = fmt.Sprintf("%s:%s:%s", p.ID, bizDate, ids.Short(4))
	}

	inst, created, err := s.Repo.CreateInstance(ctx, model.Instance{
		WorkspaceID: workspaceID, PipelineID: p.ID, PipelineVer: ver,
		BizDate: bizDate, TriggerType: triggerType, IdempotencyKey: key, Params: params,
	})
	if err != nil {
		return nil, false, err
	}
	if !created {
		return inst, false, nil
	}
	if err := s.Repo.AddTasks(ctx, inst.ID, spec.Nodes); err != nil {
		return nil, false, err
	}
	// 资源组与优先级随实例固化：管道后续改配置不影响已排队实例的次序，
	// 否则「先排队的反而后跑」，用户无法预期。
	group := p.ResourceGroup
	if group == "" {
		group = "default"
	}
	priority := queue.ClampPriority(p.Priority)
	if err := s.Repo.SetInstanceResources(ctx, inst.ID, group, priority); err != nil {
		s.Log.Warn("写入实例资源组失败", "instance", inst.ID, "err", err)
	}
	inst.ResourceGroup, inst.Priority = group, priority

	// 资源组满则置为 waiting，由分发器在有槽位时提升为 queued。
	if running, rerr := s.Repo.RunningCount(ctx, workspaceID, group); rerr == nil {
		if running >= s.groupConcurrency(group) {
			if serr := s.Repo.SetInstanceStatus(ctx, inst.ID, "waiting"); serr == nil {
				inst.Status = "waiting"
			}
		}
	}
	return inst, true, nil
}

// groupConcurrency 返回资源组并发上限；未配置时用管道自身的 concurrency 兜底。
func (s *Service) groupConcurrency(group string) int {
	if g, ok := s.ResourceGroups[group]; ok && g.MaxConcurrency > 0 {
		return g.MaxConcurrency
	}
	return 4
}

// TriggerByPipelineID 用于重跑：复用同一管道与业务日期，不重新解析名称。
func (s *Service) TriggerByPipelineID(ctx context.Context, workspaceID, pipelineID, bizDate, triggerType string, params map[string]any) (*model.Instance, bool, error) {
	var name string
	if err := s.Repo.DB.Pool.QueryRow(ctx,
		`SELECT name FROM pipeline WHERE id=$1 AND workspace_id=$2`, pipelineID, workspaceID).Scan(&name); err != nil {
		return nil, false, errs.New(errs.CodeNotFound, "管道不存在")
	}
	return s.Trigger(ctx, workspaceID, name, bizDate, triggerType, params)
}

// RunOnce 领取并执行一个实例，返回是否真的干了活。
// 单进程内串行执行，跨进程靠 SKIP LOCKED 与租约保证不重复。
func (s *Service) RunOnce(ctx context.Context, worker string) (bool, error) {
	lease := time.Duration(s.Cfg.Scheduler.LeaseSec) * time.Second
	inst, err := s.Repo.ClaimInstance(ctx, worker, lease)
	if err != nil {
		return false, err
	}
	if inst == nil {
		return false, nil
	}
	s.Log.Info("开始执行实例", "instance", inst.ID, "pipeline", inst.PipelineID, "biz_date", inst.BizDate)
	s.AppendLog(inst.ID, "", "info", fmt.Sprintf("开始执行 pipeline=%s biz_date=%s", inst.PipelineID, inst.BizDate))

	// 心跳：租约过半即续期；一旦丢失立即取消执行，避免双写。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.heartbeatLoop(runCtx, inst.ID, worker, lease, cancel)

	execErr := s.execute(runCtx, inst)

	status := "success"
	errMsg := ""
	if execErr != nil {
		status = "failed"
		errMsg = execErr.Error()
	}
	if ferr := s.Repo.FinishInstance(ctx, inst.ID, status, errMsg); ferr != nil {
		s.Log.Error("更新实例状态失败", "instance", inst.ID, "err", ferr)
	}
	s.AppendLog(inst.ID, "", map[bool]string{true: "error", false: "info"}[execErr != nil],
		fmt.Sprintf("实例结束 status=%s %s", status, errMsg))

	// 告警判定在实例结束后进行：此时行数、耗时、状态都已落库。
	// 取最新实例快照再判定，避免用执行前的旧状态。
	if fresh, gerr := s.Repo.GetInstance(ctx, inst.ID); gerr == nil {
		s.EvaluateAlerts(context.WithoutCancel(ctx), inst.WorkspaceID, fresh)
	} else {
		s.EvaluateAlerts(context.WithoutCancel(ctx), inst.WorkspaceID, inst)
	}
	s.ReleaseLogs(context.WithoutCancel(ctx), inst.ID)

	if execErr != nil {
		return true, execErr
	}
	return true, nil
}

func (s *Service) heartbeatLoop(ctx context.Context, instanceID, worker string, lease time.Duration, cancel context.CancelFunc) {
	interval := lease / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Repo.Heartbeat(ctx, instanceID, worker, lease); err != nil {
				s.Log.Error("租约续期失败，取消执行", "instance", instanceID, "err", err)
				cancel()
				return
			}
		}
	}
}

// execute 按阶段推进：同阶段节点并发，阶段之间串行。
func (s *Service) execute(ctx context.Context, inst *model.Instance) error {
	// 无论成败都在结束时清一次缓存：失败也可能写了部分数据，
	// 让下一次查询回到真实库，比让用户看到陈旧结果更安全。
	defer func() {
		if n, err := s.Repo.CachePurgeByWorkspace(context.Background(), inst.WorkspaceID); err == nil && n > 0 {
			s.Log.Debug("实例结束，已清理查询缓存", "instance", inst.ID, "count", n)
		}
	}()

	// 重跑必须用实例创建时的版本快照，否则模型改动会让历史结果漂移。
	spec, err := s.Repo.PipelineVersionSpec(ctx, inst.PipelineID, inst.PipelineVer)
	if err != nil {
		return err
	}
	vars := RenderVars(inst.BizDate, inst.Params)

	tasks, err := s.Repo.Tasks(ctx, inst.ID)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, t := range tasks {
		if t.Status == "success" || t.Status == "skipped" {
			done[t.NodeID] = true
		}
	}

	blocked := map[string]string{} // nodeID -> 原因
	for _, stage := range compile.Stages(spec.Nodes) {
		if err := ctx.Err(); err != nil {
			return errs.Wrap(errs.CodeExecutionFailed, err, "执行被取消")
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var stageErr error
		for _, n := range stage {
			if _, isBlocked := blocked[n.ID]; isBlocked {
				continue
			}
			// 上游失败/阻断 → 当前节点阻断
			skipReason := ""
			for _, dep := range n.DependsOn {
				if reason, ok := blocked[dep]; ok {
					skipReason = "上游 " + dep + " 失败: " + reason
					break
				}
			}
			if skipReason != "" {
				blocked[n.ID] = skipReason
				_ = s.Repo.FinishTask(ctx, inst.ID, n.ID, "blocked", 0, 0, skipReason)
				continue
			}
			if done[n.ID] {
				continue
			}

			wg.Add(1)
			go func(n model.NodeSpec) {
				defer wg.Done()
				if err := s.runNode(ctx, inst, n, vars); err != nil {
					mu.Lock()
					blocked[n.ID] = err.Error()
					if stageErr == nil {
						stageErr = err
					}
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()

		// 阶段内失败不立刻中止：同阶段其他节点已跑完，后续阶段按依赖自动阻断，
		// 最终整体标记失败，便于一次看到所有问题。
		if stageErr != nil {
			continue
		}
	}

	if len(blocked) > 0 {
		msgs := make([]string, 0, len(blocked))
		for id, reason := range blocked {
			msgs = append(msgs, id+": "+reason)
		}
		return errs.New(errs.CodeExecutionFailed, "存在失败节点: "+strings.Join(msgs, "; "))
	}
	return nil
}

func (s *Service) runNode(ctx context.Context, inst *model.Instance, n model.NodeSpec, vars map[string]any) error {
	retry := n.Retry
	if retry <= 0 {
		retry = 0
	}
	timeout := time.Duration(n.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	var lastErr error
	for attempt := 0; attempt <= retry; attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		_ = s.Repo.StartTask(ctx, inst.ID, n.ID, attempt+1)

		nodeCtx, cancel := context.WithTimeout(ctx, timeout)
		stats, err := s.Exec.Run(nodeCtx, inst.WorkspaceID, n, vars)
		cancel()

		if err == nil {
			_ = s.Repo.FinishTask(ctx, inst.ID, n.ID, "success", stats.RowsAffected, stats.DurationMS, "")
			if stats.Message != "" {
				s.AppendLog(inst.ID, n.ID, "info", fmt.Sprintf("%s 完成：%s（%d 行 / %dms）",
					n.ID, stats.Message, stats.RowsAffected, stats.DurationMS))
			}
			s.saveQuality(ctx, inst, n, stats)
			return nil
		}
		lastErr = err
		s.Log.Warn("节点执行失败", "node", n.ID, "attempt", attempt+1, "err", err)
		s.AppendLog(inst.ID, n.ID, "error", fmt.Sprintf("%s 第 %d 次尝试失败：%v", n.ID, attempt+1, err))
		if attempt < retry {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}

	msg := ""
	if lastErr != nil {
		msg = lastErr.Error()
	}
	_ = s.Repo.FinishTask(ctx, inst.ID, n.ID, "failed", 0, 0, msg)
	return lastErr
}

func (s *Service) saveQuality(ctx context.Context, inst *model.Instance, n model.NodeSpec, stats node.Stats) {
	if len(stats.Quality) == 0 {
		return
	}
	records := make([]struct {
		RuleName, Observed, Expected, Severity string
		Passed                                 bool
	}, 0, len(stats.Quality))
	for _, q := range stats.Quality {
		records = append(records, struct {
			RuleName, Observed, Expected, Severity string
			Passed                                 bool
		}{q.RuleName, q.Observed, q.Expected, q.Severity, q.Passed})
	}
	if err := s.Repo.SaveQualityResults(ctx, inst.ID, n.ID, records); err != nil {
		s.Log.Warn("保存质量结果失败", "err", err)
	}
}

// Recover 回收僵尸实例，由调度循环周期调用。
func (s *Service) Recover(ctx context.Context) (int, error) {
	return s.Repo.RequeueStale(ctx, 30*time.Second)
}
