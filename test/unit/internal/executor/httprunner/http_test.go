package httprunner_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/godw/godw/internal/executor/httprunner"
)

// 守住请求构造的完整链路：方法大写化、body 变量替换、headers 的类型无关写入。
// 任何一环失效都会让回调节点收到错误请求体或丢了鉴权头。
func TestRunSendsMethodBodyAndHeaders(t *testing.T) {
	var (
		gotMethod string
		gotBody   string
		gotToken  string
		gotCount  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod = r.Method
		gotBody = string(b)
		gotToken = r.Header.Get("X-Token")
		gotCount = r.Header.Get("X-Count")
		_, _ = io.WriteString(w, "accepted")
	}))
	defer srv.Close()

	cfg := map[string]any{
		"url":    srv.URL + "/hook",
		"method": "post",
		"headers": map[string]any{
			"X-Token": "static-token",
			"X-Count": 3,
		},
		"body": `{"name":"${name}","token":"${token}"}`,
	}
	vars := map[string]any{"name": "godw", "token": "t-1"}

	got, err := httprunner.Run(context.Background(), cfg, vars)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if got != "accepted" {
		t.Fatalf("返回体 = %q，期望 accepted", got)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q，期望 POST", gotMethod)
	}
	if gotBody != `{"name":"godw","token":"t-1"}` {
		t.Fatalf("body 变量未替换: %q", gotBody)
	}
	if gotToken != "static-token" || gotCount != "3" {
		t.Fatalf("headers 写入不符: token=%q count=%q", gotToken, gotCount)
	}
}

// 守住变量替换的边界：多个占位符、非字符串值、缺失占位符都要按原文保留。
// 直接通过回显服务观察实际发出的 body，覆盖 expand 的全部分支。
func TestRunExpandsPlaceholdersInBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		vars map[string]any
		want string
	}{
		{"无变量", "${a}", nil, "${a}"},
		{"单个变量", "v=${a}", map[string]any{"a": "1"}, "v=1"},
		{"非字符串值", "${n}-${b}", map[string]any{"n": 42, "b": true}, "42-true"},
		{"重复出现全部替换", "${a}${a}", map[string]any{"a": "x"}, "xx"},
		{"变量名不匹配", "${ab}", map[string]any{"a": "x"}, "${ab}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			cfg := map[string]any{
				"url":    srv.URL,
				"method": http.MethodPost,
				"body":   tc.body,
			}
			got, err := httprunner.Run(context.Background(), cfg, tc.vars)
			if err != nil {
				t.Fatalf("Run 报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("发出的 body = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 守住缺省 method：未配置时必须发 GET，否则回调会变成空 POST。
func TestRunDefaultsToGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := httprunner.Run(context.Background(), map[string]any{"url": srv.URL}, nil); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("缺省 method 应为 GET，实际 %q", gotMethod)
	}
}

// 守住响应截断：超过 2000 字符必须截断并加省略号，
// 否则大响应会原样写进任务日志把数据库打爆。
func TestRunTruncatesLongResponse(t *testing.T) {
	long := strings.Repeat("a", 2500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, long)
	}))
	defer srv.Close()

	got, err := httprunner.Run(context.Background(), map[string]any{"url": srv.URL}, nil)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if len(got) != 2003 {
		t.Fatalf("截断后长度 = %d，期望 2003", len(got))
	}
	if got[:2000] != strings.Repeat("a", 2000) || !strings.HasSuffix(got, "...") {
		t.Fatalf("截断内容不符: %q", got[len(got)-10:])
	}
}

// 守住状态码校验：默认期望 200，非 200 必须报错但仍返回响应体，
// 便于任务日志展示服务端实际返回的错误信息。
func TestRunStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "server error")
	}))
	defer srv.Close()

	got, err := httprunner.Run(context.Background(), map[string]any{"url": srv.URL}, nil)
	if err == nil {
		t.Fatal("500 响应应返回错误")
	}
	if !strings.Contains(err.Error(), "期望状态 200，实际 500") {
		t.Fatalf("错误信息不符: %v", err)
	}
	if got != "server error" {
		t.Fatalf("失败时也应返回响应体，实际 %q", got)
	}
}

// 守住 expect_status 配置生效：回调第三方接口时 201/204 都是正常结果。
func TestRunCustomExpectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	}))
	defer srv.Close()

	cfg := map[string]any{"url": srv.URL, "expect_status": http.StatusCreated}
	got, err := httprunner.Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("201 应视为成功: %v", err)
	}
	if got != "created" {
		t.Fatalf("返回体 = %q，期望 created", got)
	}
}

// 守住 expect_status 非 int 时退回默认值：配置里写成字符串不能被当成 0，
// 否则所有请求都会被判成状态码不匹配。
func TestRunIgnoresNonIntExpectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := map[string]any{"url": srv.URL, "expect_status": "201"}
	if _, err := httprunner.Run(context.Background(), cfg, nil); err != nil {
		t.Fatalf("非 int 的 expect_status 应被忽略并沿用 200: %v", err)
	}
}

// 守住 headers 类型不符时不 panic：历史配置里 headers 可能是字符串，
// 解析失败应忽略而不是让节点崩溃。
func TestRunIgnoresMalformedHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cfg := map[string]any{"url": srv.URL, "headers": "X-Token: t"}
	if got, err := httprunner.Run(context.Background(), cfg, nil); err != nil || got != "ok" {
		t.Fatalf("headers 非 map 时应忽略，实际 got=%q err=%v", got, err)
	}
}

// 守住参数校验：缺少 url 或 url 非字符串时必须立即报错，不能发起空请求。
func TestRunMissingURL(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
	}{
		{"空配置", map[string]any{}},
		{"url 为空串", map[string]any{"url": ""}},
		{"url 非字符串", map[string]any{"url": 123}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := httprunner.Run(context.Background(), tc.config, nil); err == nil {
				t.Fatal("应返回缺少 url 的错误")
			} else if !strings.Contains(err.Error(), "缺少 url") {
				t.Fatalf("错误信息不符: %v", err)
			}
		})
	}
}

// 守住协议白名单：file:// 与 ftp:// 必须被拒绝，否则可被用来读取本地文件。
func TestRunRejectsNonHTTPScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/a", "//example.com/a", "javascript:alert(1)"} {
		t.Run(u, func(t *testing.T) {
			if _, err := httprunner.Run(context.Background(), map[string]any{"url": u}, nil); err == nil {
				t.Fatal("非 http/https 应被拒绝")
			} else if !strings.Contains(err.Error(), "只支持 http/https") {
				t.Fatalf("错误信息不符: %v", err)
			}
		})
	}
}

// 守住非法 method 与非法 URL 的错误返回：不能构造出半成品请求。
func TestRunInvalidRequest(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"method 含空格", map[string]any{"url": "http://example.com/x", "method": "BAD METHOD"}},
		{"url 含非法转义", map[string]any{"url": "http://example.com/%zz"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := httprunner.Run(context.Background(), tc.cfg, nil); err == nil {
				t.Fatal("非法请求应返回错误")
			}
		})
	}
}

// 守住网络错误透传：连接被拒时错误必须返回给调度器以便重试，
// 不能被当成「空响应成功」。
func TestRunTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, err := httprunner.Run(context.Background(), map[string]any{"url": url}, nil); err == nil {
		t.Fatal("连接已关闭的服务应返回错误")
	}
}

// 守住上下文取消：上游取消后请求必须立即失败，不能继续占用执行槽位。
func TestRunHonorsCanceledContext(t *testing.T) {
	// 用只监听不响应的地址保证请求会阻塞在网络层，再由 ctx 取消终止。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听端口: %v", err)
	}
	addr := l.Addr().String()
	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := httprunner.Run(ctx, map[string]any{"url": "http://" + addr + "/x"}, nil); err == nil {
		t.Fatal("已取消的 context 应返回错误")
	}
}
