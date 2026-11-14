package httprunner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Run 执行 HTTP 回调节点。config 支持 method/url/headers/body/expect_status。
// 只允许 http/https，且禁止跟随到非预期主机以外的重定向由调用方通过 URL 白名单控制。
func Run(ctx context.Context, config map[string]any, vars map[string]any) (string, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return "", fmt.Errorf("httprunner: 缺少 url")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("httprunner: 只支持 http/https")
	}
	method, _ := config["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	body, _ := config["body"].(string)
	body = expand(body, vars)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, bytes.NewReader([]byte(body)))
	if err != nil {
		return "", err
	}
	if headers, ok := config["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	expect := 200
	if v, ok := config["expect_status"].(int); ok {
		expect = v
	}
	if resp.StatusCode != expect {
		return string(respBody), fmt.Errorf("httprunner: 期望状态 %d，实际 %d", expect, resp.StatusCode)
	}
	text := string(respBody)
	if len(text) > 2000 {
		text = text[:2000] + "..."
	}
	return text, nil
}

func expand(s string, vars map[string]any) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", fmt.Sprint(v))
	}
	return s
}
