package bench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/godw/godw/internal/bench"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// okRe 从摘要行里提取成功请求数。
var okRe = regexp.MustCompile(`成功=(\d+)`)

// captureStdout 把压测报告收进缓冲区，避免污染测试输出。
func captureStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := bench.Stdout
	bench.Stdout = buf
	t.Cleanup(func() { bench.Stdout = old })
	return buf
}

// TestPct 校验分位计算：空切片、边界 p=0/1、以及常规中位数。
func run(args []string) int { return bench.Run(args) }

func TestPct(t *testing.T) {
	if got := bench.Pct(nil, 0.5); got != 0 {
		t.Fatalf("空切片应返回 0，实际 %v", got)
	}
	lats := []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0, 1 * time.Millisecond},
		{0.5, 2 * time.Millisecond},
		{0.9, 3 * time.Millisecond},
		{1, 4 * time.Millisecond},
	}
	for _, c := range cases {
		if got := bench.Pct(lats, c.p); got != c.want {
			t.Errorf("bench.Pct(p=%v)=%v，期望 %v", c.p, got, c.want)
		}
	}
}

// TestPick 校验三种场景的接口选择，mixed 与 read 依赖 worker 序号轮转。
func TestPick(t *testing.T) {
	if path, method, body := bench.Pick("write", 0); method != http.MethodPost ||
		!strings.Contains(path, "/pipelines/order_daily/run") {
		t.Errorf("write 场景应打触发接口，实际 %s %s", method, path)
	} else if m, ok := body.(map[string]any); !ok || m["bizDate"] == nil {
		t.Errorf("write 场景应带 bizDate 请求体，实际 %v", body)
	}

	// mixed：worker%5==0 走总览，否则走指标查询。
	if path, method, _ := bench.Pick("mixed", 0); method != http.MethodGet || !strings.Contains(path, "/overview") {
		t.Errorf("mixed 场景 worker=0 应打总览，实际 %s %s", method, path)
	}
	if path, _, _ := bench.Pick("mixed", 1); !strings.Contains(path, "/metrics/gmv/query") {
		t.Errorf("mixed 场景 worker=1 应打指标查询，实际 %s", path)
	}

	// read（默认分支）：worker%3 轮流打三个接口。
	want := []string{"/api/v1/models", "/api/v1/instances?limit=20", "/api/v1/lineage/impact?table=ods_order&column=amount&depth=5"}
	for i, w := range want {
		path, method, body := bench.Pick("read", i)
		if method != http.MethodGet || path != w {
			t.Errorf("read 场景 worker=%d 期望 %s，实际 %s %s", i, w, method, path)
		}
		if body != nil {
			t.Errorf("read 场景不应带请求体，实际 %v", body)
		}
	}
	// 未知场景退回 read 分支，保证压测不会因为场景拼错而空转。
	if path, _, _ := bench.Pick("unknown", 0); path != "/api/v1/models" {
		t.Errorf("未知场景应退回 read，实际 %s", path)
	}
}

// TestDo 校验请求构造：方法、鉴权头、JSON 头与 body 是否按预期发出。
func TestDo(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotAuth, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod, gotAuth, gotCT = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotBody = nil
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &gotBody)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	status, err := bench.Do(context.Background(), srv.Client(), srv.URL, "tk", http.MethodPost, "/x", map[string]any{"bizDate": "2026-01-01"})
	if err != nil {
		t.Fatalf("请求不该失败: %v", err)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("状态码透传有误，实际 %d", status)
	}
	mu.Lock()
	method, auth, ct, body := gotMethod, gotAuth, gotCT, gotBody
	mu.Unlock()
	if method != http.MethodPost {
		t.Errorf("方法透传有误，实际 %s", method)
	}
	if auth != "Bearer tk" {
		t.Errorf("Bearer token 未设置，实际 %q", auth)
	}
	if ct != "application/json" {
		t.Errorf("POST 应带 JSON Content-Type，实际 %q", ct)
	}
	if body["bizDate"] != "2026-01-01" {
		t.Errorf("请求体未按 JSON 发送，实际 %v", body)
	}

	// body 为 nil 时不设 Content-Type，也不带鉴权头。
	if _, err := bench.Do(context.Background(), srv.Client(), srv.URL, "", http.MethodGet, "/y", nil); err != nil {
		t.Fatalf("GET 请求不该失败: %v", err)
	}
	mu.Lock()
	auth, ct = gotAuth, gotCT
	mu.Unlock()
	if auth != "" || ct != "" {
		t.Errorf("无 token / 无 body 时不应带请求头，实际 auth=%q ct=%q", auth, ct)
	}

	// 非法 URL 应在构造阶段就报错。
	if _, err := bench.Do(context.Background(), srv.Client(), "://bad", "", http.MethodGet, "/z", nil); err == nil {
		t.Error("非法 URL 应返回错误")
	}
}

// TestRunHelp 校验 --help 返回 0 且被 flag 包接管（不真正发请求）。
func TestRunHelp(t *testing.T) {
	captureStdout(t)
	if code := run([]string{"--help"}); code != 0 {
		t.Fatalf("--help 应返回 0，实际 %d", code)
	}
}

// TestRunBadFlag 校验未知参数返回用法错误码 2。
func TestRunBadFlag(t *testing.T) {
	captureStdout(t)
	if code := run([]string{"--nope"}); code != 2 {
		t.Fatalf("未知参数应返回 2，实际 %d", code)
	}
}

// TestRunSuccess 打一个永远 200 的本地服务：应至少完成一次成功请求并输出报告。
// 退出码允许 0/1——到达压测时长时仍在途的请求会以 context deadline exceeded 计为失败，
// 这是压测工具本身的语义，不是服务端错误，所以这里断言的是「成功数 > 0」与错误类型。
func TestRunSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	buf := captureStdout(t)
	code := run([]string{"-base", srv.URL, "-c", "2", "-d", "80ms", "-s", "mixed"})
	if code != 0 && code != 1 {
		t.Fatalf("退出码应为 0 或 1，实际 %d，输出:\n%s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "场景=mixed") || !strings.Contains(out, "QPS=") {
		t.Fatalf("摘要输出不符合预期:\n%s", out)
	}
	if !strings.Contains(out, "延迟 p50=") {
		t.Fatalf("应输出延迟分位:\n%s", out)
	}
	m := okRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("摘要里找不到成功数:\n%s", out)
	}
	okCount, err := strconv.Atoi(m[1])
	if err != nil || okCount == 0 {
		t.Fatalf("应至少有一次成功请求，实际 %q:\n%s", m[1], out)
	}
	// 除收尾时的超时外不应有别的错误。
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "->") && !strings.Contains(line, "context deadline exceeded") {
			t.Errorf("出现非收尾超时的错误: %s", line)
		}
	}
}

// TestRunFail 服务返回 500 时视为失败请求，退出码为 1 并打印错误分布。
func TestRunFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	buf := captureStdout(t)
	code := run([]string{"-base", srv.URL, "-c", "1", "-d", "60ms", "-s", "read"})
	if code != 1 {
		t.Fatalf("存在失败请求应返回 1，实际 %d，输出:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "错误分布") {
		t.Fatalf("应打印错误分布:\n%s", buf.String())
	}
}

// TestRunBadBase 地址非法时每次请求都失败，退出码为 1。
func TestRunBadBase(t *testing.T) {
	buf := captureStdout(t)
	code := run([]string{"-base", "://bad", "-c", "1", "-d", "40ms"})
	if code != 1 {
		t.Fatalf("非法地址应返回 1，实际 %d，输出:\n%s", code, buf.String())
	}
}
