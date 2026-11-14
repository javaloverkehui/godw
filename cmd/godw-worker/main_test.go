package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godw/godw/cmd/internal/testenv"
)

// captureOutput 接管 stdout/stderr，避免测试输出被版本号/错误信息刷屏。
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	oldOut, oldErr := stdout, stderr
	stdout, stderr = buf, buf
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return buf
}

// TestRunVersion 校验 --version / -v 打印版本后返回 0。
func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"--version", "-v"} {
		buf := captureOutput(t)
		if code := run([]string{arg}); code != 0 {
			t.Fatalf("%s 应返回 0，实际 %d", arg, code)
		}
		if got := buf.String(); got != "godw-worker "+version+"\n" {
			t.Fatalf("%s 输出不符: %q", arg, got)
		}
	}
}

// TestRunBadConfig 配置文件不存在时返回 1 并把错误写到 stderr。
func TestRunBadConfig(t *testing.T) {
	buf := captureOutput(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := run([]string{"--config", missing}); code != 1 {
		t.Fatalf("坏配置应返回 1，实际 %d", code)
	}
	if buf.Len() == 0 {
		t.Fatal("坏配置应打印错误")
	}
}

// TestRunBadYAML 配置内容非法（未知字段）同样返回 1。
func TestRunBadYAML(t *testing.T) {
	buf := captureOutput(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("env: test\nunknown_key: 1\n"), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if code := run([]string{"--config", path}); code != 1 {
		t.Fatalf("非法配置应返回 1，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "未知配置项") {
		t.Fatalf("应打印配置错误:\n%s", buf.String())
	}
}

// TestRunOnceEmpty 队列为空时 --once 跑一轮即退出，返回 0。
// 同时覆盖 --name 与未识别参数的忽略逻辑。
func TestRunOnceEmpty(t *testing.T) {
	env := testenv.New(t, true)
	buf := captureOutput(t)
	if code := run([]string{"--config", env.ConfigPath, "--name", "test-worker", "--once", "--unknown-flag"}); code != 0 {
		t.Fatalf("--once 应返回 0，实际 %d，输出:\n%s", code, buf.String())
	}
}

// TestRunOnceDefaultName 不传 --name 时用进程号生成名称，行为不变。
func TestRunOnceDefaultName(t *testing.T) {
	env := testenv.New(t, true)
	captureOutput(t)
	t.Setenv("GODW_WORKER_NAME", "")
	if code := run([]string{"--config", env.ConfigPath, "--once"}); code != 0 {
		t.Fatalf("--once 应返回 0，实际 %d", code)
	}
}

// TestRunNameFromEnv GODW_WORKER_NAME 提供默认 worker 名。
func TestRunNameFromEnv(t *testing.T) {
	env := testenv.New(t, true)
	captureOutput(t)
	t.Setenv("GODW_WORKER_NAME", "env-worker")
	if code := run([]string{"--config", env.ConfigPath, "--once"}); code != 0 {
		t.Fatalf("--once 应返回 0，实际 %d", code)
	}
}

// TestRunOpenFail 元数据库连不上时返回 1，而不是静默退出。
func TestRunOpenFail(t *testing.T) {
	env := testenv.New(t, true)
	// 端口改成不可达值：DSN 解析成功但 Ping 失败。
	raw, err := os.ReadFile(env.ConfigPath)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	patched := strings.Replace(string(raw), "port: 5432", "port: 1", 1)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	captureOutput(t)
	if code := run([]string{"--config", path, "--once"}); code != 1 {
		t.Fatalf("连不上库应返回 1，实际 %d", code)
	}
}
