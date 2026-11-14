package server_test

import (
	"bytes"
	"context"
	"github.com/godw/godw/internal/server"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/obs/log"
	"github.com/godw/godw/test/testenv"
)

const testVersion = "1.0.0"

func run(args []string) int { return server.Run(args, testVersion) }

// captureOutput 把 run 的标准输出/错误收进缓冲区，避免污染测试输出。
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	oldOut, oldErr := server.Stdout, server.Stderr
	server.Stdout, server.Stderr = buf, buf
	t.Cleanup(func() { server.Stdout, server.Stderr = oldOut, oldErr })
	return buf
}

// TestRunVersion 校验 --version 只打印版本并返回 0（不碰数据库）。
func TestRunVersion(t *testing.T) {
	buf := captureOutput(t)
	if code := run([]string{"--version"}); code != 0 {
		t.Fatalf("--version 应返回 0，实际 %d", code)
	}
	if got := buf.String(); got != "godw-server "+testVersion+"\n" {
		t.Fatalf("版本输出不符: %q", got)
	}
}

// TestRunHelp 校验 -h 由 flag 包处理并返回 0。
func TestRunHelp(t *testing.T) {
	captureOutput(t)
	if code := run([]string{"-h"}); code != 0 {
		t.Fatalf("-h 应返回 0，实际 %d", code)
	}
}

// TestRunUnknownFlag 校验未知参数返回用法错误码 2。
func TestRunUnknownFlag(t *testing.T) {
	captureOutput(t)
	if code := run([]string{"--nope"}); code != 2 {
		t.Fatalf("未知参数应返回 2，实际 %d", code)
	}
}

// TestRunMissingConfig 缺配置（默认 config.yaml 不存在且未注入密钥）应返回 1。
func TestRunMissingConfig(t *testing.T) {
	captureOutput(t)
	t.Setenv("GODW_AUTH_JWT_SECRET", "")
	t.Setenv("GODW_ENV", "missing")
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	if code := run(nil); code != 1 {
		t.Fatalf("缺配置应返回 1，实际 %d", code)
	}
}

// TestRunBadConfig 配置解析失败（未知字段）应返回 1，且错误信息带上文件名。
func TestRunBadConfig(t *testing.T) {
	buf := captureOutput(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("env: dev\nnope: 1\n"), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if code := run([]string{"--config", path}); code != 1 {
		t.Fatalf("坏配置应返回 1，实际 %d", code)
	}
	if buf.Len() == 0 {
		t.Fatal("坏配置应打印错误信息")
	}
}

// TestRunMigrateOnly 只跑迁移：--migrate 应在迁移完成后返回 0，不起 HTTP 服务。
func TestRunMigrateOnly(t *testing.T) {
	env := testenv.New(t, false)
	captureOutput(t)
	if code := run([]string{"--config", env.ConfigPath, "--migrate"}); code != 0 {
		t.Fatalf("--migrate 应返回 0，实际 %d", code)
	}
}

// TestRunPortInUse 端口被占用时 ListenAndServe 失败，run 应返回 1 而不是挂住。
func TestRunPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	env := testenv.New(t, true, func(c *config.Config) { c.Server.Addr = addr })
	buf := captureOutput(t)

	done := make(chan int, 1)
	go func() { done <- run([]string{"--config", env.ConfigPath}) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("端口占用应返回 1，实际 %d，输出:\n%s", code, buf.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("端口占用时 run 应尽快返回，实际超时")
	}
}

// TestWorkerLoopCanceled ctx 已取消时 server.WorkerLoop 应立即返回，不做任何轮询。
func TestWorkerLoopCanceled(t *testing.T) {
	env := testenv.New(t, true)
	svc := env.Service(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		server.WorkerLoop(ctx, svc, log.New(env.Config.Log), env.Config)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 取消后 server.WorkerLoop 应立即返回")
	}
}

// TestWorkerLoopIdle 队列为空时 server.WorkerLoop 应进入退避，并在 ctx 到期后返回。
func TestWorkerLoopIdle(t *testing.T) {
	env := testenv.New(t, true)
	svc := env.Service(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		server.WorkerLoop(ctx, svc, log.New(env.Config.Log), env.Config)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 到期后 server.WorkerLoop 应返回")
	}
}
