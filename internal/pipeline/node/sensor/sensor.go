// Package sensor 实现「等数据到」的触发条件。
//
// 定时调度解决的是「到点就跑」，但很多场景的真实条件是「上游文件落了」「源表分区出现了」。
// 用 cron 猜时间必然要么太早（空跑）要么太晚（延迟）。
// 传感器的职责就是把「时间驱动」补成「数据驱动」。
package sensor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/godw/godw/internal/connector"
)

// Kind 是传感器类型。
type Kind string

const (
	KindFile  Kind = "file"
	KindTable Kind = "table"
	KindHTTP  Kind = "http"
)

// Spec 是一个传感器的配置。
type Spec struct {
	Kind Kind `json:"kind"`
	// File：本地/S3 挂载路径。
	Path string `json:"path,omitempty"`
	// File：文件大小连续两次探测不变才认为写完（防止读到半个文件）。
	StableSeconds int `json:"stableSeconds,omitempty"`
	// Table：数据源名 + schema.table + 可选分区。
	Datasource string `json:"datasource,omitempty"`
	Table      string `json:"table,omitempty"`
	Partition  string `json:"partition,omitempty"`
	// HTTP：轮询地址与期望状态码。
	URL          string `json:"url,omitempty"`
	ExpectStatus int    `json:"expectStatus,omitempty"`
	// 通用：轮询间隔与超时。
	IntervalSec int `json:"intervalSec,omitempty"`
	TimeoutSec  int `json:"timeoutSec,omitempty"`
}

// Result 是一次探测结论。
type Result struct {
	Satisfied bool   `json:"satisfied"`
	Detail    string `json:"detail"`
	// Observed 是可展示的观测值（文件大小、行数、状态码）。
	Observed string `json:"observed,omitempty"`
}

// Checker 执行一次探测。file/table 需要外部注入依赖，便于测试。
type Checker struct {
	// Query 用于 table 传感器：执行一条返回单值的 SQL。
	Query func(ctx context.Context, datasource, sqlText string) (string, error)
	// HTTPGet 用于 http 传感器，默认用 net/http。
	HTTPGet func(ctx context.Context, url string) (int, error)
	// Stat 用于 file 传感器，默认 os.Stat。
	Stat func(path string) (size int64, modTime time.Time, err error)
}

// Check 执行一次探测。
func (c *Checker) Check(ctx context.Context, spec Spec) (Result, error) {
	switch spec.Kind {
	case KindFile:
		return c.checkFile(spec)
	case KindTable:
		return c.checkTable(ctx, spec)
	case KindHTTP:
		return c.checkHTTP(ctx, spec)
	}
	return Result{}, fmt.Errorf("未知传感器类型: %s", spec.Kind)
}

func (c *Checker) checkFile(spec Spec) (Result, error) {
	if strings.TrimSpace(spec.Path) == "" {
		return Result{}, fmt.Errorf("file 传感器缺少 path")
	}
	stat := c.Stat
	if stat == nil {
		stat = func(p string) (int64, time.Time, error) {
			fi, err := os.Stat(p)
			if err != nil {
				return 0, time.Time{}, err
			}
			return fi.Size(), fi.ModTime(), nil
		}
	}
	size, mod, err := stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, fs.ErrNotExist) {
			return Result{Satisfied: false, Detail: "文件尚未出现"}, nil
		}
		return Result{}, err
	}
	if size == 0 {
		return Result{Satisfied: false, Detail: "文件存在但为空", Observed: "0 B"}, nil
	}
	// 大小稳定判定：修改时间距今超过阈值才认为写完。
	stable := spec.StableSeconds
	if stable <= 0 {
		stable = 10
	}
	if time.Since(mod) < time.Duration(stable)*time.Second {
		return Result{
			Satisfied: false,
			Detail:    fmt.Sprintf("文件仍在写入（最后一次修改 %s 前）", time.Since(mod).Truncate(time.Second)),
			Observed:  fmt.Sprintf("%d B", size),
		}, nil
	}
	return Result{
		Satisfied: true,
		Detail:    fmt.Sprintf("文件已就绪（%d B，%s 前修改）", size, time.Since(mod).Truncate(time.Second)),
		Observed:  fmt.Sprintf("%d B", size),
	}, nil
}

func (c *Checker) checkTable(ctx context.Context, spec Spec) (Result, error) {
	if spec.Table == "" || spec.Datasource == "" {
		return Result{}, fmt.Errorf("table 传感器缺少 datasource 或 table")
	}
	if c.Query == nil {
		return Result{}, fmt.Errorf("table 传感器未注入查询能力")
	}
	where := ""
	if spec.Partition != "" {
		where = " WHERE " + spec.Partition
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", spec.Table, where)
	raw, err := c.Query(ctx, spec.Datasource, sqlText)
	if err != nil {
		return Result{}, err
	}
	var n int64
	if _, err := fmt.Sscan(strings.TrimSpace(raw), &n); err != nil {
		return Result{}, fmt.Errorf("无法解析行数 %q: %w", raw, err)
	}
	return Result{
		Satisfied: n > 0,
		Detail:    fmt.Sprintf("目标表行数 %d", n),
		Observed:  fmt.Sprint(n),
	}, nil
}

func (c *Checker) checkHTTP(ctx context.Context, spec Spec) (Result, error) {
	if spec.URL == "" {
		return Result{}, fmt.Errorf("http 传感器缺少 url")
	}
	get := c.HTTPGet
	if get == nil {
		get = defaultHTTPGet
	}
	code, err := get(ctx, spec.URL)
	if err != nil {
		return Result{Satisfied: false, Detail: "请求失败: " + err.Error()}, nil
	}
	expect := spec.ExpectStatus
	if expect == 0 {
		expect = 200
	}
	return Result{
		Satisfied: code == expect,
		Detail:    fmt.Sprintf("HTTP %d（期望 %d）", code, expect),
		Observed:  fmt.Sprint(code),
	}, nil
}

// Wait 轮询直到满足或超时。超时返回明确错误，调用方据此把实例置 FAILED(sensor_timeout)。
func (c *Checker) Wait(ctx context.Context, spec Spec) (Result, error) {
	interval := time.Duration(spec.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timeout := time.Duration(spec.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	var last Result
	for {
		r, err := c.Check(ctx, spec)
		if err != nil {
			return last, err
		}
		if r.Satisfied {
			return r, nil
		}
		last = r
		if time.Now().After(deadline) {
			return last, fmt.Errorf("传感器超时（%s）：%s", timeout, last.Detail)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// FromNodeConfig 从节点 config 解析传感器定义。
// 约定 config.sensor.{kind,path,table,...}，缺 kind 视为未启用。
func FromNodeConfig(cfg map[string]any) (Spec, bool) {
	raw, ok := cfg["sensor"]
	if !ok {
		return Spec{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return Spec{}, false
	}
	s := Spec{}
	if v, ok := m["kind"].(string); ok {
		s.Kind = Kind(v)
	}
	if s.Kind == "" {
		return Spec{}, false
	}
	s.Path, _ = m["path"].(string)
	s.Datasource, _ = m["datasource"].(string)
	s.Table, _ = m["table"].(string)
	s.Partition, _ = m["partition"].(string)
	s.URL, _ = m["url"].(string)
	s.ExpectStatus = toInt(m["expectStatus"])
	s.IntervalSec = toInt(m["intervalSec"])
	s.TimeoutSec = toInt(m["timeoutSec"])
	s.StableSeconds = toInt(m["stableSeconds"])
	return s, true
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

// ConnectorQuery 把连接器适配成传感器需要的查询函数。
func ConnectorQuery(get func(ctx context.Context, name string) (connector.Connector, error)) func(context.Context, string, string) (string, error) {
	return func(ctx context.Context, datasource, sqlText string) (string, error) {
		c, err := get(ctx, datasource)
		if err != nil {
			return "", err
		}
		rows, err := c.Query(ctx, sqlText)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		if !rows.Next() {
			return "", fmt.Errorf("查询无结果")
		}
		var v any
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
		return fmt.Sprint(v), nil
	}
}
