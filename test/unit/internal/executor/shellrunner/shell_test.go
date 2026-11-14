package shellrunner_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/executor/shellrunner"
)

// TestMain 把解释器指向本机可用的 sh：Windows 上没有 /bin/sh，
// 直接执行会在所有用例里退化为「启动失败」，无法验证任何真实分支。
func TestMain(m *testing.M) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "shellrunner 测试需要 sh 解释器: %v\n", err)
		os.Exit(1)
	}
	shellrunner.ShellPath = sh
	os.Exit(m.Run())
}

// 守住空脚本校验：空串与纯空白都必须在起进程前拒绝，
// 否则会白白 fork 一个立即退出的 shell，还掩盖上层模板渲染失败。
func TestRunRejectsEmptyScript(t *testing.T) {
	cases := []struct{ name, script string }{
		{"空串", ""},
		{"纯空格", "   "},
		{"换行与制表", "\n\t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := shellrunner.Run(context.Background(), "", tc.script, nil)
			if err == nil {
				t.Fatal("空脚本应返回错误")
			}
			if !strings.Contains(err.Error(), "script 为空") {
				t.Fatalf("错误信息不符: %v", err)
			}
			if out != "" {
				t.Fatalf("空脚本不应有输出，实际 %q", out)
			}
		})
	}
}

// 守住输出回传：正常退出时 CombinedOutput 必须原样带出 stdout。
func TestRunReturnsStdout(t *testing.T) {
	out, err := shellrunner.Run(context.Background(), "", "echo hi", nil)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if out != "hi\n" {
		t.Fatalf("输出 = %q，期望 %q", out, "hi\n")
	}
}

// 守住变量注入：键名必须大写后进环境变量，值按 fmt.Sprint 转换。
// 直接把变量拼进命令行会带来注入风险，这条用例锁住实现的注入方式。
func TestRunInjectsVarsAsEnv(t *testing.T) {
	vars := map[string]any{"name": "godw", "n": 42, "flag": true}
	out, err := shellrunner.Run(context.Background(), "", `echo "$NAME/$N/$FLAG"`, vars)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if out != "godw/42/true\n" {
		t.Fatalf("输出 = %q，期望 %q", out, "godw/42/true\n")
	}
}

// 守住 workDir 生效：文件类数据源的落地目录依赖它，配错会把产物写到进程目录。
func TestRunUsesWorkDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := shellrunner.Run(context.Background(), dir, "echo written > marker.txt", nil); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "marker.txt"))
	if err != nil {
		t.Fatalf("产物未落在 workDir: %v", err)
	}
	if strings.TrimSpace(string(content)) != "written" {
		t.Fatalf("产物内容 = %q，期望 written", content)
	}
}

// 守住失败语义：非零退出码必须带出错误，且错误里要含脚本输出，
// 否则任务失败后运维只能看到 exit status 而无从定位。
func TestRunFailureIncludesOutput(t *testing.T) {
	out, err := shellrunner.Run(context.Background(), "", "echo boom; exit 3", nil)
	if err == nil {
		t.Fatal("非零退出应返回错误")
	}
	if !strings.Contains(err.Error(), "shellrunner:") {
		t.Fatalf("错误应带 shellrunner 前缀: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误应包含脚本输出: %v", err)
	}
	if out != "boom\n" {
		t.Fatalf("失败时也应返回输出，实际 %q", out)
	}
}

// 守住超长输出截断：保留头尾各 4000 字符并插入分隔标记，
// 避免一次误跑的 cat 把整张任务日志表撑爆。
func TestRunTruncatesLongOutput(t *testing.T) {
	script := `i=0; while [ $i -lt 3000 ]; do echo 0123456789; i=$((i+1)); done`
	out, err := shellrunner.Run(context.Background(), "", script, nil)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	const wantLen = 4000 + len("\n...\n") + 4000
	if len(out) != wantLen {
		t.Fatalf("截断后长度 = %d，期望 %d", len(out), wantLen)
	}
	if out[4000:4005] != "\n...\n" {
		t.Fatalf("截断标记不符: %q", out[3990:4010])
	}
	if strings.Trim(out[:4000], "0123456789\n") != "" {
		t.Fatal("头部 4000 字符应全部来自脚本输出")
	}
	if strings.Trim(out[4005:], "0123456789\n") != "" {
		t.Fatal("尾部 4000 字符应全部来自脚本输出")
	}
}

// 守住超时中断：脚本必须被 ctx 杀掉并返回错误，
// 否则挂死的节点会永久占用执行槽位。
// 这里用 shell 自身的死循环而非 sleep：sleep 是子进程，杀掉 sh 后它仍持有输出管道，
// 会让 CombinedOutput 等到子进程自然结束，测不出 ctx 是否生效。
func TestRunTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := shellrunner.Run(ctx, "", `while true; do :; done`, nil)
	if err == nil {
		t.Fatal("超时脚本应返回错误")
	}
	if !strings.Contains(err.Error(), "shellrunner:") {
		t.Fatalf("错误应带 shellrunner 前缀: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时未及时生效，耗时 %v", elapsed)
	}
}

// 守住已取消的 ctx：起进程前就应失败，不能把脚本真的跑起来。
func TestRunWithAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := shellrunner.Run(ctx, "", "echo should-not-run", nil)
	if err == nil {
		t.Fatal("已取消的 context 应返回错误")
	}
}
