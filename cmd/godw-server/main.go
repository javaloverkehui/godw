package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	apihttp "github.com/godw/godw/internal/api/http"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/obs/log"
	"github.com/godw/godw/internal/scheduler"

	// 连接器在 init 中注册，core 只认类型名。
	_ "github.com/godw/godw/internal/connector/clickhouse"
	_ "github.com/godw/godw/internal/connector/duckdb"
	_ "github.com/godw/godw/internal/connector/mysql"
	_ "github.com/godw/godw/internal/connector/postgres"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// stdout / stderr 供测试替换；退出码语义与改造前一致：0 正常、1 启动失败、2 用法错误。
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// 发布构建用 -ldflags "-X main.version=v1.0.0" 覆盖。
var version = "1.0.0"

// run 解析参数并启动控制面，返回进程退出码。
// 参数解析改用独立 FlagSet：全局 flag.CommandLine 在同一进程里只能注册一次，
// 测试反复调用 run 会 panic。
func run(args []string) int {
	fs := flag.NewFlagSet("godw-server", flag.ContinueOnError)
	var configPath string
	var migrateOnly, showVersion bool
	fs.StringVar(&configPath, "config", "", "配置文件路径，默认 ./config.yaml")
	fs.BoolVar(&migrateOnly, "migrate", false, "只执行迁移后退出")
	fs.BoolVar(&showVersion, "version", false, "打印版本后退出")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}
	if showVersion {
		fmt.Fprintln(stdout, "godw-server", version)
		return 0
	}
	if err := serve(configPath, migrateOnly); err != nil {
		fmt.Fprintln(stderr, "启动失败:", err)
		return 1
	}
	return 0
}

func serve(configPath string, migrateOnly bool) error {
	res, err := config.Load(config.Loader{Path: configPath})
	if err != nil {
		return err
	}
	cfg := res.Config

	lg := log.New(cfg.Log)
	slog.SetDefault(lg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := repo.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("迁移失败: %w", err)
	}
	if len(applied) > 0 {
		lg.Info("已应用迁移", "versions", applied)
	}
	if migrateOnly {
		lg.Info("迁移完成，退出")
		return nil
	}

	svc, err := service.New(db, cfg, lg)
	if err != nil {
		return err
	}
	ws, err := svc.Bootstrap(ctx)
	if err != nil {
		return err
	}

	// 调度与执行内嵌在同一进程，减少部署面；高负载时可关掉调度，
	// 改用独立 godw-worker 横向扩展数据面。
	if cfg.Scheduler.Enabled {
		sch := &scheduler.Scheduler{Repo: svc.Repo, Svc: svc, Log: lg, TickSec: cfg.Scheduler.TickSec}
		go sch.Run(ctx)
	}
	go workerLoop(ctx, svc, lg, cfg)

	h := apihttp.NewHandler(svc, cfg, lg, ws.ID)
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           h.Middleware(),
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		lg.Info("控制面已启动", "addr", cfg.Server.Addr, "env", cfg.Env, "workspace", ws.Name)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		lg.Info("收到退出信号，开始优雅关闭")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	return srv.Shutdown(shutdownCtx)
}

// workerLoop 轮询领取实例。空闲时退避到 3 秒，避免空转打满数据库。
func workerLoop(ctx context.Context, svc *service.Service, lg *slog.Logger, cfg config.Config) {
	worker := fmt.Sprintf("srv-%d", os.Getpid())
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ran, err := svc.RunOnce(ctx, worker)
		if err != nil {
			lg.Error("执行实例失败", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
