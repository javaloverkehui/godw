package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
)

// Scheduler 周期检查 cron 命中并派发实例。
//
// 设计取舍：不引入 Kafka/Temporal，靠「每分钟扫描 + 幂等键」保证不重不漏。
// 幂等键由 (pipeline, biz_date) 生成，即使多副本同时扫描也只会成功创建一次。
type Scheduler struct {
	Repo    *repo.Repo
	Svc     *service.Service
	Log     *slog.Logger
	TickSec int
	// LookbackMin 是补触发窗口：进程重启错过整点也能在窗口内补上。
	LookbackMin int

	mu   sync.Mutex
	last map[string]string // pipelineID -> 已触发的 "YYYY-MM-DD HH:MM"
}

func (s *Scheduler) Run(ctx context.Context) {
	tick := time.Duration(s.TickSec) * time.Second
	if tick <= 0 {
		tick = 10 * time.Second
	}
	if s.LookbackMin <= 0 {
		s.LookbackMin = 5
	}
	if s.last == nil {
		s.last = map[string]string{}
	}

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Log.Info("调度循环退出")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	if n, err := s.Svc.Recover(ctx); err != nil {
		s.Log.Warn("回收僵尸实例失败", "err", err)
	} else if n > 0 {
		s.Log.Info("已回收僵尸实例", "count", n)
	}

	pipelines, err := s.Repo.ListEnabledPipelines(ctx)
	if err != nil {
		s.Log.Error("加载调度管道失败", "err", err)
		return
	}

	now := time.Now()
	s.runSubscriptions(ctx, now)
	for _, p := range pipelines {
		sched, err := Parse(p.Schedule)
		if err != nil {
			s.Log.Warn("cron 表达式非法，跳过", "pipeline", p.Name, "schedule", p.Schedule, "err", err)
			continue
		}
		// 从最早的可能时刻扫到当前，命中即触发一次（窗口用于重启补偿）。
		from := now.Add(-time.Duration(s.LookbackMin) * time.Minute).Truncate(time.Minute)
		for t := from; !t.After(now); t = t.Add(time.Minute) {
			if !sched.Match(t) {
				continue
			}
			slot := t.Format("2006-01-02 15:04")
			if s.mark(p.ID, slot) {
				continue
			}
			// 数仓默认跑 T-1 全量分区。
			bizDate := t.AddDate(0, 0, -1).Format("2006-01-02")
			inst, created, err := s.Svc.Trigger(ctx, p.WorkspaceID, p.Name, bizDate, "schedule", nil)
			if err != nil {
				s.Log.Error("派发实例失败", "pipeline", p.Name, "err", err)
				continue
			}
			if created {
				s.Log.Info("已派发实例", "pipeline", p.Name, "instance", inst.ID, "biz_date", bizDate)
			}
		}
	}
}

// mark 返回该时间片是否已处理过，避免同一进程重复派发。
func (s *Scheduler) mark(pipelineID, slot string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last[pipelineID] == slot {
		return true
	}
	// 只保留最新片，避免 map 无限增长。
	s.last[pipelineID] = slot
	return false
}

// runSubscriptions 按 cron 触发订阅推送。与管道调度共用一套 cron 解析。
// 失败只记录，不影响主调度循环。
func (s *Scheduler) runSubscriptions(ctx context.Context, now time.Time) {
	subs, err := s.Repo.ListEnabledSubscriptions(ctx)
	if err != nil {
		s.Log.Warn("加载订阅失败", "err", err)
		return
	}
	for _, sub := range subs {
		if strings.TrimSpace(sub.Schedule) == "" {
			continue
		}
		sc, err := Parse(sub.Schedule)
		if err != nil {
			continue
		}
		if !sc.Match(now.Truncate(time.Minute)) {
			continue
		}
		// 同一分钟内只跑一次：last_run_at 落在当前分钟则跳过
		if sub.LastRunAt != nil && sub.LastRunAt.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			continue
		}
		res, err := s.Svc.RunSubscription(ctx, sub.WorkspaceID, sub)
		if err != nil {
			s.Log.Warn("订阅执行失败", "subscription", sub.Name, "err", err)
			continue
		}
		s.Log.Info("订阅已推送", "subscription", sub.Name, "result", res)
	}
}
