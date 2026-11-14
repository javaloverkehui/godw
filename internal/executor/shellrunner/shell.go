package shellrunner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Run 执行 shell 脚本。变量通过环境变量注入，避免拼进命令行造成注入。
func Run(ctx context.Context, workDir, script string, vars map[string]any) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("shellrunner: script 为空")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
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
