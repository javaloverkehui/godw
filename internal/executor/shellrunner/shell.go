package shellrunner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// shellPath 是脚本解释器：POSIX 平台固定为 /bin/sh，其它平台可覆盖为可用的 sh。
// ShellPath 是脚本解释器路径，默认 /bin/sh。
// 导出它是因为 Windows 等非 POSIX 平台没有 /bin/sh，需要由调用方或测试指向可用的 sh。
var ShellPath = "/bin/sh"

// Run 执行 shell 脚本。变量通过环境变量注入，避免拼进命令行造成注入。
func Run(ctx context.Context, workDir, script string, vars map[string]any) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("shellrunner: script 为空")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ShellPath, "-c", script)
	if workDir != "" {
		cmd.Dir = workDir
	}
	env := cmd.Environ()
	for k, v := range vars {
		env = append(env, strings.ToUpper(k)+"="+fmt.Sprint(v))
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > 8000 {
		text = text[:4000] + "\n...\n" + text[len(text)-4000:]
	}
	if err != nil {
		return text, fmt.Errorf("shellrunner: %w: %s", err, text)
	}
	return text, nil
}
