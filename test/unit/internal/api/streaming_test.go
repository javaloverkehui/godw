package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apihttp "github.com/godw/godw/internal/api/http"
)

// 事件流与日志流是长连接：走完整中间件时会被 statusWriter 吞掉 Flush，
// handler 只能落到「不支持流式输出」的兜底分支。
//
// 为了真正覆盖推流逻辑，这里绕过中间件，直接把请求打给 Handler.ServeHTTP，
// 并用 httptest.ResponseRecorder（它实现了 Flusher）接输出。
// 这样测的是「handler 本身对不对」，中间件的那层缺陷不在本文件范围内。
func TestStreamingHandlersDirect(t *testing.T) {
	e := newEnv(t)
	ds := e.pgDatasource("pgmain")
	schema := e.sourceSchema()
	fxApplyModel(t, e, fxModelYAML("dwd_order", schema, ds), true, true)
	fxApplyPipeline(t, e, fxPipelineYAML("dwd_order_daily", ds, "dwd_order", schema), true)
	id := fxFirstInstance(t, e, "dwd_order_daily", "2024-01-02")

	// 先取消成终态：事件流推完一帧就会发 done 并自行结束，不会挂住用例。
	e.ok("POST", "/api/v1/instances/"+id+"/cancel", nil)

	h := apihttp.NewHandler(e.svc, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), e.wsID)

	// 事件流：Content-Type 必须是 SSE，且 body 里要能看到 status 与 done 两种事件。
	rec := httptest.NewRecorder()
	serveEventually(t, h, rec, "/api/v1/instances/"+id+"/events")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("事件流 Content-Type 不对: %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: status") || !strings.Contains(body, `"status":"canceled"`) {
		t.Fatalf("事件流内容不对: %q", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("终态实例应发 done: %q", body)
	}

	// 日志流：初始化内存日志后追加两条，再按 level 过滤消费。
	e.ok("GET", "/api/v1/instances/"+id+"/logs", nil)
	e.svc.AppendLog(id, "src", "info", "开始读取源表")
	e.svc.AppendLog(id, "load", "error", "写入失败")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/instances/"+id+"/logs/stream?level=info&after=0", nil)
	req.Header.Set("Last-Event-ID", "0")
	serveEventually(t, h, rec, req.URL.RequestURI(), req.Header)
	logBody := rec.Body.String()
	if !strings.Contains(logBody, "event: log") || !strings.Contains(logBody, "开始读取源表") {
		t.Fatalf("日志流应推出 info 日志: %q", logBody)
	}
	// level=info 时 error 日志被过滤，但仍然会推进 seq。
	if strings.Contains(logBody, "写入失败") {
		t.Fatalf("level 过滤未生效: %q", logBody)
	}
	if !strings.Contains(logBody, "event: done") {
		t.Fatalf("终态实例应发 done: %q", logBody)
	}
}

// serveEventually 在后台跑 Handler，超时则取消请求并失败：
// 流式 handler 出 bug 时表现是「永远不返回」，不能让整个测试进程卡死。
func serveEventually(t *testing.T, h *apihttp.Handler, rec *httptest.ResponseRecorder, path string, hdr ...http.Header) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	if len(hdr) > 0 {
		for k, vs := range hdr[0] {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		cancel()
		t.Fatalf("流式 handler 超时未返回: %s", path)
	}
}
