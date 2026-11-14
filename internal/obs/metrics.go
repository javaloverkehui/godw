package obs

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 是一个极简的 Prometheus 文本格式导出器。
// 不引入 prometheus/client_golang：本系统的指标集很小，手写计数器足够，
// 也避免为一个 /metrics 端点增加依赖树。
type Metrics struct {
	mu       sync.RWMutex
	counters map[string]*counterVec
	histos   map[string]*histogram
	started  time.Time
}

type counterVec struct {
	mu     sync.Mutex
	values map[string]*atomic.Int64
	help   string
}

type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []int64
	sum     float64
	count   int64
	help    string
}

// Default 是进程级指标单例。中间件与调度器都写这里。
var Default = NewMetrics()

func NewMetrics() *Metrics {
	return &Metrics{
		counters: map[string]*counterVec{},
		histos:   map[string]*histogram{},
		started:  time.Now(),
	}
}

func (m *Metrics) counter(name, help string) *counterVec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[name]; ok {
		return c
	}
	c := &counterVec{values: map[string]*atomic.Int64{}, help: help}
	m.counters[name] = c
	return c
}

// Inc 累加一个带标签的计数器。labels 形如 `method="GET",path="/api/v1/models"`。
func (m *Metrics) Inc(name, help, labels string) {
	c := m.counter(name, help)
	c.mu.Lock()
	v, ok := c.values[labels]
	if !ok {
		v = &atomic.Int64{}
		c.values[labels] = v
	}
	c.mu.Unlock()
	v.Add(1)
}

// Observe 记录一次耗时（秒）。
func (m *Metrics) Observe(name, help string, seconds float64) {
	m.mu.Lock()
	h, ok := m.histos[name]
	if !ok {
		h = &histogram{
			buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			help:    help,
		}
		h.counts = make([]int64, len(h.buckets))
		m.histos[name] = h
	}
	m.mu.Unlock()

	h.mu.Lock()
	for i, b := range h.buckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.count++
	h.mu.Unlock()
}

// SetGauge 用计数器语义暴露一个瞬时值（如队列长度），写入时覆盖。
func (m *Metrics) SetGauge(name, help, labels string, value int64) {
	c := m.counter(name, help)
	c.mu.Lock()
	v, ok := c.values[labels]
	if !ok {
		v = &atomic.Int64{}
		c.values[labels] = v
	}
	c.mu.Unlock()
	v.Store(value)
}

// Handler 输出 Prometheus 文本格式。
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder

		b.WriteString("# HELP godw_uptime_seconds 进程已运行秒数\n")
		b.WriteString("# TYPE godw_uptime_seconds gauge\n")
		fmt.Fprintf(&b, "godw_uptime_seconds %.0f\n", time.Since(m.started).Seconds())

		m.mu.RLock()
		names := make([]string, 0, len(m.counters))
		for n := range m.counters {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := m.counters[n]
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", n, c.help, n)
			c.mu.Lock()
			keys := make([]string, 0, len(c.values))
			for k := range c.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if k == "" {
					fmt.Fprintf(&b, "%s %d\n", n, c.values[k].Load())
				} else {
					fmt.Fprintf(&b, "%s{%s} %d\n", n, k, c.values[k].Load())
				}
			}
			c.mu.Unlock()
		}

		hnames := make([]string, 0, len(m.histos))
		for n := range m.histos {
			hnames = append(hnames, n)
		}
		sort.Strings(hnames)
		for _, n := range hnames {
			h := m.histos[n]
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", n, h.help, n)
			h.mu.Lock()
			for i, bound := range h.buckets {
				fmt.Fprintf(&b, "%s_bucket{le=\"%g\"} %d\n", n, bound, h.counts[i])
			}
			fmt.Fprintf(&b, "%s_bucket{le=\"+Inf\"} %d\n", n, h.count)
			fmt.Fprintf(&b, "%s_sum %g\n", n, h.sum)
			fmt.Fprintf(&b, "%s_count %d\n", n, h.count)
			h.mu.Unlock()
		}
		m.mu.RUnlock()

		_, _ = w.Write([]byte(b.String()))
	})
}
