// Command bench 是轻量压测工具：并发打 API，输出延迟分位与吞吐。
//
// 为什么不用 k6/hey：需要携带登录 token、按业务接口组合压测，
// 且要能在 CI 里直接跑，单独引一个外部二进制反而更麻烦。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latencies []time.Duration
	errs      map[string]int
	mu        sync.Mutex
}

// stdout 供测试替换，避免用例把压测报告打到测试输出里。
var stdout io.Writer = os.Stdout

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 解析参数并执行压测，返回退出码：0 无错误、1 有失败请求、2 用法错误。
func run(args []string) int {
	fs := flag.NewFlagSet("godw-bench", flag.ContinueOnError)
	base := fs.String("base", "http://localhost:8080", "服务地址")
	token := fs.String("token", "", "Bearer token，留空则用 dev 模式")
	conc := fs.Int("c", 20, "并发数")
	dur := fs.Duration("d", 20*time.Second, "持续时长")
	scenario := fs.String("s", "read", "场景：read（查询类）/ write（提交流程）/ mixed")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conc * 2,
			MaxIdleConnsPerHost: *conc * 2,
			MaxConnsPerHost:     *conc * 2,
		},
	}

	var (
		ok, fail int64
		res      = &result{errs: map[string]int{}}
		wg       sync.WaitGroup
	)
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	start := time.Now()
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for ctx.Err() == nil {
				path, method, body := pick(*scenario, worker)
				t0 := time.Now()
				status, err := do(ctx, client, *base, *token, method, path, body)
				el := time.Since(t0)
				res.mu.Lock()
				res.latencies = append(res.latencies, el)
				res.mu.Unlock()
				if err != nil || status >= 500 {
					atomic.AddInt64(&fail, 1)
					res.mu.Lock()
					key := fmt.Sprintf("%s %s -> %d %v", method, path, status, err)
					res.errs[key]++
					res.mu.Unlock()
				} else {
					atomic.AddInt64(&ok, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	res.mu.Lock()
	lats := append([]time.Duration(nil), res.latencies...)
	errs := make(map[string]int, len(res.errs))
	for k, v := range res.errs {
		errs[k] = v
	}
	res.mu.Unlock()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })

	fmt.Fprintf(stdout, "场景=%s 并发=%d 时长=%s\n", *scenario, *conc, total.Round(time.Millisecond))
	fmt.Fprintf(stdout, "请求=%d 成功=%d 失败=%d QPS=%.1f\n",
		ok+fail, ok, fail, float64(ok+fail)/total.Seconds())
	if len(lats) > 0 {
		fmt.Fprintf(stdout, "延迟 p50=%s p90=%s p99=%s max=%s\n",
			pct(lats, 0.5), pct(lats, 0.9), pct(lats, 0.99), lats[len(lats)-1].Round(time.Millisecond))
	}
	if len(errs) > 0 {
		fmt.Fprintln(stdout, "错误分布：")
		for k, v := range errs {
			fmt.Fprintf(stdout, "  %4d  %s\n", v, k)
		}
		return 1
	}
	return 0
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i].Round(time.Millisecond)
}

func pick(scenario string, worker int) (path, method string, body any) {
	switch scenario {
	case "write":
		// 触发一次手动运行：写路径最重的一段（创建实例 + 幂等键）
		return "/api/v1/pipelines/order_daily/run", "POST",
			map[string]any{"bizDate": "2026-11-09"}
	case "mixed":
		if worker%5 == 0 {
			return "/api/v1/overview", "GET", nil
		}
		return "/api/v1/metrics/gmv/query?from=2026-10-11&to=2026-11-10&granularity=day", "GET", nil
	default:
		// 读场景轮流打三个典型接口：列表、详情、指标
		switch worker % 3 {
		case 0:
			return "/api/v1/models", "GET", nil
		case 1:
			return "/api/v1/instances?limit=20", "GET", nil
		default:
			return "/api/v1/lineage/impact?table=ods_order&column=amount&depth=5", "GET", nil
		}
	}
}

func do(ctx context.Context, c *http.Client, base, token, method, path string, body any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
