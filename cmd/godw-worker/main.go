package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/obs/log"

	_ "github.com/godw/godw/internal/connector/clickhouse"
	_ "github.com/godw/godw/internal/connector/duckdb"
	_ "github.com/godw/godw/internal/connector/mysql"
	_ "github.com/godw/godw/internal/connector/postgres"
)

// 发布构建用 -ldflags "-X main.version=v1.0.0" 覆盖。
var version = "1.0.0"

// godw-worker 是独立数据面进程：不提供 HTTP，只领取并执行实例。
// 横向扩容时直接起多个副本，靠 SKIP LOCKED 与租约保证不重复执行。
func main() {
	var configPath, name string
	var once bool
	fmt.Sscanf(os.Getenv("GODW_WORKER_NAME"), "%s", &name)
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--config":
			if i+1 < len(os.Args) {
				configPath = os.Args[i+1]
				i++
			}
		case "--name":
			if i+1 < len(os.Args) {
				name = os.Args[i+1]
				i++
			}
		case "--once":
			once = true
		case "--version", "-v":
			fmt.Println("godw-worker", version)
			return
		}
	}
	if name == "" {
		name = fmt.Sprintf("worker-%d", os.Getpid())
	}

	res, err := config.Load(config.Loader{Path: configPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lg := log.New(res.Config.Log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := repo.Open(ctx, res.Config.DB)
	if err != nil {
		lg.Error("连接元数据库失败", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	svc, err := service.New(db, res.Config, lg)
	if err != nil {
		lg.Error("初始化服务失败", "err", err)
		os.Exit(1)
	}

	lg.Info("数据面已启动", "worker", name)
	for {
		select {
		case <-ctx.Done():
			lg.Info("数据面退出")
			return
		default:
		}
		ran, err := svc.RunOnce(ctx, name)
		if err != nil {
			lg.Error("执行失败", "err", err)
		}
		if ran {
			if once {
				return
			}
			continue
		}
		if once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
