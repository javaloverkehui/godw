package main

import (
	"context"
	"fmt"
	"io"
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

// stdout / stderr 供测试替换；退出码语义与改造前一致。
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// godw-worker 是独立数据面进程：不提供 HTTP，只领取并执行实例。
// 横向扩容时直接起多个副本，靠 SKIP LOCKED 与租约保证不重复执行。
//
// run 解析参数后进入执行循环，返回进程退出码（0 正常退出、1 启动失败）。
func run(args []string) int {
	var configPath, name string
	var once bool
	fmt.Sscanf(os.Getenv("GODW_WORKER_NAME"), "%s", &name)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--once":
			once = true
		case "--version", "-v":
			fmt.Fprintln(stdout, "godw-worker", version)
			return 0
		}
	}
	if name == "" {
		name = fmt.Sprintf("worker-%d", os.Getpid())
	}

	res, err := config.Load(config.Loader{Path: configPath})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	lg := log.New(res.Config.Log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := repo.Open(ctx, res.Config.DB)
	if err != nil {
		lg.Error("连接元数据库失败", "err", err)
		return 1
	}
	defer db.Close()

	svc, err := service.New(db, res.Config, lg)
	if err != nil {
		lg.Error("初始化服务失败", "err", err)
		return 1
	}

	lg.Info("数据面已启动", "worker", name)
	for {
		select {
		case <-ctx.Done():
			lg.Info("数据面退出")
			return 0
		default:
		}
		ran, err := svc.RunOnce(ctx, name)
		if err != nil {
			lg.Error("执行失败", "err", err)
		}
		if ran {
			if once {
				return 0
			}
			continue
		}
		if once {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(2 * time.Second):
		}
	}
}
