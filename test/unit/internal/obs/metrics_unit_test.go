package obs_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/godw/godw/internal/obs"
)

// scrape 调用 Handler 并返回文本，避免每个用例重复写 recorder 样板。
func scrape(t *testing.T, m *obs.Metrics) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("Handler 返回 %d", rec.Code)
	}
	return rec.Body.String(), rec.Header().Get("Content-Type")
}

// 计数器要支持同标签累加、空标签、多标签共存，且 HELP/TYPE 只出现一次。
func TestMetricsCounter(t *testing.T) {
	m := obs.NewMetrics()
	m.Inc("godw_requests_total", "请求总数", "")
	m.Inc("godw_requests_total", "请求总数", "")
	m.Inc("godw_requests_total", "请求总数", `method="GET",path="/x"`)

	body, ctype := scrape(t, m)
	if !strings.HasPrefix(ctype, "text/plain") {
		t.Fatalf("Content-Type 异常: %s", ctype)
	}
	for _, want := range []string{
		"# HELP godw_requests_total 请求总数",
		"# TYPE godw_requests_total counter",
		"godw_requests_total 2",
		`godw_requests_total{method="GET",path="/x"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, body)
		}
	}
	// 重复注册同名计数器时沿用第一次的 help，不能每次追加一段 HELP。
	if n := strings.Count(body, "# HELP godw_requests_total"); n != 1 {
		t.Fatalf("HELP 应只输出一次，实际 %d 次", n)
	}
}

// Gauge 语义是覆盖而不是累加。
func TestMetricsSetGauge(t *testing.T) {
	m := obs.NewMetrics()
	m.SetGauge("godw_queue_length", "队列长度", "", 5)
	m.SetGauge("godw_queue_length", "队列长度", "", 2)
	body, _ := scrape(t, m)
	if !strings.Contains(body, "godw_queue_length 2") {
		t.Fatalf("Gauge 应输出最后写入值:\n%s", body)
	}
	if strings.Contains(body, "godw_queue_length 7") {
		t.Fatalf("Gauge 不应累加:\n%s", body)
	}
}

// 直方图要覆盖全部桶、+Inf、sum 与 count 四个部分。
func TestMetricsHistogram(t *testing.T) {
	m := obs.NewMetrics()
	for _, v := range []float64{0.003, 0.02, 0.3, 20} {
		m.Observe("godw_request_duration_seconds", "请求耗时", v)
	}
	// 同名重复 Observe 必须复用同一个直方图，而不是重置。
	m.Observe("godw_request_duration_seconds", "请求耗时", 0.004)

	body, _ := scrape(t, m)
	for _, want := range []string{
		"# TYPE godw_request_duration_seconds histogram",
		`godw_request_duration_seconds_bucket{le="0.005"} 2`,
		`godw_request_duration_seconds_bucket{le="0.025"} 3`,
		`godw_request_duration_seconds_bucket{le="0.5"} 4`,
		`godw_request_duration_seconds_bucket{le="10"} 4`,
		`godw_request_duration_seconds_bucket{le="+Inf"} 5`,
		"godw_request_duration_seconds_count 5",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("直方图缺少 %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "godw_request_duration_seconds_sum ") {
		t.Fatalf("缺少 sum:\n%s", body)
	}
	if n := strings.Count(body, "# HELP godw_request_duration_seconds "); n != 1 {
		t.Fatalf("直方图 HELP 应只输出一次，实际 %d 次", n)
	}
}

// 指标名必须排序输出，否则每次抓取文本都不同，难以做 diff 与快照。
func TestMetricsSorted(t *testing.T) {
	m := obs.NewMetrics()
	m.Inc("zzz_total", "z", "")
	m.Inc("aaa_total", "a", "")
	m.Observe("yyy_seconds", "y", 1)
	m.Observe("bbb_seconds", "b", 1)

	body, _ := scrape(t, m)
	if strings.Index(body, "aaa_total") > strings.Index(body, "zzz_total") {
		t.Fatalf("计数器未按名称排序:\n%s", body)
	}
	if strings.Index(body, "bbb_seconds") > strings.Index(body, "yyy_seconds") {
		t.Fatalf("直方图未按名称排序:\n%s", body)
	}
	if !strings.Contains(body, "godw_uptime_seconds") {
		t.Fatalf("缺少运行时长指标:\n%s", body)
	}
}

// 中间件与调度器会并发写指标，-race 下必须无数据竞争且计数精确。
func TestMetricsConcurrent(t *testing.T) {
	m := obs.NewMetrics()
	const goroutines, rounds = 16, 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				m.Inc("godw_concurrent_total", "并发计数", "")
				m.Observe("godw_concurrent_seconds", "并发耗时", 0.01)
				m.SetGauge("godw_concurrent_gauge", "并发瞬时值", "", int64(j))
			}
		}()
	}
	wg.Wait()

	body, _ := scrape(t, m)
	if !strings.Contains(body, "godw_concurrent_total 1600") {
		t.Fatalf("并发计数丢失:\n%s", body)
	}
	if !strings.Contains(body, "godw_concurrent_seconds_count 1600") {
		t.Fatalf("并发观测丢失:\n%s", body)
	}
}

// Default 是进程级单例，必须可直接使用。
func TestDefaultMetricsSingleton(t *testing.T) {
	if obs.Default == nil {
		t.Fatal("Default 不能为 nil")
	}
	obs.Default.Inc("godw_default_total", "默认单例", "")
	body, _ := scrape(t, obs.Default)
	if !strings.Contains(body, "godw_default_total 1") {
		t.Fatalf("默认单例未生效:\n%s", body)
	}
}
