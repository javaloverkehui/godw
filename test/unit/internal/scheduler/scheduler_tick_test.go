package scheduler_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/scheduler"
	"github.com/godw/godw/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Main(m)) }

func newService(t *testing.T, db *repo.DB) *service.Service {
	t.Helper()
	cfg := testsupport.Config(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := service.New(db, cfg, log)
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	return svc
}

// runFor 启动调度循环，给定时间后取消并等待退出。
func runFor(t *testing.T, s *scheduler.Scheduler, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	time.Sleep(d)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("调度循环未在取消后退出")
	}
}

func insertWorkspace(t *testing.T, db *repo.DB, id string) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`INSERT INTO workspace(id,name) VALUES($1,$1)`, id); err != nil {
		t.Fatalf("插入工作区失败: %v", err)
	}
}

// 守住整轮 tick：僵尸回收、cron 命中派发、非法 cron 跳过、未命中跳过、
// 订阅的四种分支（空 schedule / 非法 schedule / 本分钟已跑 / 执行失败）。
// 这是调度器唯一对外入口，整条链路必须真库跑通。
func TestSchedulerFullTick(t *testing.T) {
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	insertWorkspace(t, db, "ws_unit")
	ctx := context.Background()

	svc := newService(t, db)

	// 正常管道：只在「当前分钟值」命中，保证补触发窗口内只有一个时间片，
	// 这样第二个 tick 就能走到 mark 的「本进程已处理」分支。
	schedExpr := fmt.Sprintf("%d * * * *", time.Now().Minute())
	ok, err := svc.Repo.SavePipeline(ctx, "ws_unit", model.PipelineSpec{
		Kind: "pipeline", Name: "p_ok", Schedule: schedExpr,
		Nodes: []model.NodeSpec{{ID: "n1", Type: model.NodeSQL}},
	}, "tester", true)
	if err != nil {
		t.Fatalf("保存管道失败: %v", err)
	}

	// 非法 cron：ListEnabledPipelines 能看到，但 Parse 失败必须跳过而不是中断整轮。
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO pipeline(id,workspace_id,name,schedule,enabled,active_version)
		 VALUES('p_bad','ws_unit','p_bad','not a cron',true,1)`); err != nil {
		t.Fatalf("插入非法 cron 管道失败: %v", err)
	}
	// 合法但当前不会命中的 cron（只在 1 月 1 日 0 点）。
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO pipeline(id,workspace_id,name,schedule,enabled,active_version)
		 VALUES('p_never','ws_unit','p_never','0 0 1 1 *',true,1)`); err != nil {
		t.Fatalf("插入未命中管道失败: %v", err)
	}

	// 僵尸实例：running 且租约过期，应被 Recover 退回 queued。
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO instance(id,workspace_id,pipeline_id,pipeline_ver,biz_date,trigger_type,status,idempotency_key,lease_until)
		 VALUES('ins_zombie','ws_unit',$1,1,'2025-01-01','manual','running','zombie-key',now()-interval '2 hours')`,
		ok.ID); err != nil {
		t.Fatalf("插入僵尸实例失败: %v", err)
	}

	// 订阅四种分支。
	subs := []struct {
		id, name, schedule, target string
		lastRun                    string
	}{
		{"sub_empty", "sub_empty", "", "m", "NULL"},
		{"sub_bad", "sub_bad", "bad cron", "m", "NULL"},
		{"sub_done", "sub_done", "* * * * *", "m", "now()"},
		{"sub_fail", "sub_fail", "* * * * *", "no_such_metric", "NULL"},
	}
	for _, s := range subs {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO subscription(id,workspace_id,name,kind,target,channel,schedule,enabled,last_run_at)
			 VALUES($1,'ws_unit',$2,'metric',$3,'webhook',$4,true,`+s.lastRun+`)`,
			s.id, s.name, s.target, s.schedule); err != nil {
			t.Fatalf("插入订阅 %s 失败: %v", s.id, err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &scheduler.Scheduler{Repo: svc.Repo, Svc: svc, Log: log, TickSec: 1, LookbackMin: 1}
	// 跑满多个 tick：后续 tick 会命中 mark 去重（同一时间片不重复派发）。
	runFor(t, s, 4*time.Second)

	// 僵尸应被退回 queued。
	var zombieStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM instance WHERE id='ins_zombie'`).Scan(&zombieStatus); err != nil {
		t.Fatalf("查询僵尸实例失败: %v", err)
	}
	if zombieStatus != "queued" {
		t.Fatalf("僵尸实例状态 = %q，期望 queued", zombieStatus)
	}

	// 正常管道应产生 schedule 实例，业务日期为触发时刻 T-1。
	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM instance WHERE pipeline_id=$1 AND trigger_type='schedule'`, ok.ID).Scan(&n); err != nil {
		t.Fatalf("统计实例失败: %v", err)
	}
	if n == 0 {
		t.Fatal("cron 命中的管道应被派发实例")
	}
	var bizDates int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM instance WHERE pipeline_id=$1 AND trigger_type='schedule' AND biz_date < current_date`, ok.ID).
		Scan(&bizDates); err != nil {
		t.Fatalf("查询业务日期失败: %v", err)
	}
	if bizDates != n {
		t.Fatalf("调度实例的业务日期应早于今天: %d/%d", bizDates, n)
	}

	// 非法/未命中管道不应产生实例。
	for _, id := range []string{"p_bad", "p_never"} {
		var c int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM instance WHERE pipeline_id=$1`, id).Scan(&c); err != nil {
			t.Fatalf("统计 %s 实例失败: %v", id, err)
		}
		if c != 0 {
			t.Fatalf("%s 不应派发实例，实际 %d", id, c)
		}
	}

	// 订阅：执行失败的应被标记 failed；被跳过的保持空状态。
	var failStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT last_status FROM subscription WHERE id='sub_fail'`).Scan(&failStatus); err != nil {
		t.Fatalf("查询订阅状态失败: %v", err)
	}
	if failStatus != "failed" {
		t.Fatalf("执行失败的订阅 last_status = %q，期望 failed", failStatus)
	}
	for _, id := range []string{"sub_empty", "sub_bad", "sub_done"} {
		var st string
		if err := db.Pool.QueryRow(ctx, `SELECT last_status FROM subscription WHERE id=$1`, id).Scan(&st); err != nil {
			t.Fatalf("查询订阅 %s 失败: %v", id, err)
		}
		if st != "" {
			t.Fatalf("被跳过的订阅 %s 不应执行，last_status=%q", id, st)
		}
	}
}

// 守住订阅表不可用时的降级：只记录告警，不能影响管道调度主流程。
func TestSchedulerSubscriptionListError(t *testing.T) {
	db := testsupport.DB(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `DROP TABLE subscription CASCADE`); err != nil {
		t.Fatalf("删表失败: %v", err)
	}
	svc := newService(t, db)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &scheduler.Scheduler{Repo: svc.Repo, Svc: svc, Log: log, TickSec: 1, LookbackMin: 1}
	// 不 panic、能正常退出即证明降级生效。
	runFor(t, s, 1300*time.Millisecond)
}

// 守住数据库不可用时的错误路径：Recover 与 ListEnabledPipelines 都必须记录并继续/退出，
// 而不是让调度循环崩掉。
func TestSchedulerDBUnavailable(t *testing.T) {
	db := testsupport.DB(t)
	svc := newService(t, db)
	db.Pool.Close() // 之后所有查询都会失败

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &scheduler.Scheduler{Repo: svc.Repo, Svc: svc, Log: log, TickSec: 1, LookbackMin: 1}
	runFor(t, s, 1300*time.Millisecond)
}

// 守住启动参数兜底：TickSec<=0 回退 10s、LookbackMin<=0 回退 5，
// 且未初始化 last 时不能 nil map 写入。这里用短超时只验证初始化与退出路径。
func TestSchedulerRunDefaults(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &scheduler.Scheduler{Log: log, TickSec: 0, LookbackMin: -1}
	runFor(t, s, 100*time.Millisecond)
}
