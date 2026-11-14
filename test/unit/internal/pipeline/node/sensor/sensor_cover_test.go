package sensor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pipeline/node/sensor"
)

// 未知类型必须显式报错，避免拼错 kind 后永远静默不满足。
func TestCheckUnknownKind(t *testing.T) {
	c := &sensor.Checker{}
	if _, err := c.Check(context.Background(), sensor.Spec{Kind: sensor.Kind("ftp")}); err == nil ||
		!strings.Contains(err.Error(), "未知传感器类型") {
		t.Fatalf("未知类型应报错，实际 %v", err)
	}
}

// 文件传感器的四条边界：缺 path、文件不存在、空文件、默认 Stat 实现。
func TestCheckFileEdges(t *testing.T) {
	c := &sensor.Checker{}

	if _, err := c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: "  "}); err == nil {
		t.Fatal("缺 path 应报错")
	}

	// 走默认 os.Stat：不存在 → 不满足而非错误。
	missing := filepath.Join(t.TempDir(), "not-yet.csv")
	r, err := c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: missing})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "尚未出现") {
		t.Fatalf("不存在的文件应等待，实际 %+v err=%v", r, err)
	}

	// 空文件：存在但还没写完。
	empty := filepath.Join(t.TempDir(), "empty.csv")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: empty})
	if err != nil || r.Satisfied || r.Observed != "0 B" {
		t.Fatalf("空文件应不满足，实际 %+v err=%v", r, err)
	}

	// 有内容且修改时间足够久 → 就绪；同时覆盖 StableSeconds<=0 的默认 10s。
	ready := filepath.Join(t.TempDir(), "ready.csv")
	if err := os.WriteFile(ready, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(ready, old, old); err != nil {
		t.Fatal(err)
	}
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: ready})
	if err != nil || !r.Satisfied {
		t.Fatalf("稳定文件应满足，实际 %+v err=%v", r, err)
	}

	// 有内容但刚修改：默认稳定窗口 10s 内应判定仍在写入。
	c = &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 512, time.Now(), nil
	}}
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: "/x"})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "仍在写入") {
		t.Fatalf("刚修改的文件应等待，实际 %+v err=%v", r, err)
	}
}

// Stat 返回非「不存在」错误时不能吞掉，否则权限问题会被当成「还没到」。
func TestCheckFileStatError(t *testing.T) {
	c := &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 0, time.Time{}, errors.New("permission denied")
	}}
	if _, err := c.Check(context.Background(), sensor.Spec{Kind: sensor.KindFile, Path: "/x"}); err == nil {
		t.Fatal("Stat 错误应向上返回")
	}
}

// 表传感器的参数校验、空分区 SQL、查询错误与行数解析失败。
func TestCheckTableEdges(t *testing.T) {
	ctx := context.Background()

	if _, err := (&sensor.Checker{}).Check(ctx, sensor.Spec{Kind: sensor.KindTable, Datasource: "pg"}); err == nil {
		t.Fatal("缺 table 应报错")
	}
	if _, err := (&sensor.Checker{}).Check(ctx, sensor.Spec{Kind: sensor.KindTable, Table: "t"}); err == nil {
		t.Fatal("缺 datasource 应报错")
	}
	if _, err := (&sensor.Checker{}).Check(ctx, sensor.Spec{Kind: sensor.KindTable, Datasource: "pg", Table: "t"}); err == nil {
		t.Fatal("未注入 Query 应报错")
	}

	var gotSQL string
	c := &sensor.Checker{Query: func(_ context.Context, _, sqlText string) (string, error) {
		gotSQL = sqlText
		return "1", nil
	}}
	if _, err := c.Check(ctx, sensor.Spec{Kind: sensor.KindTable, Datasource: "pg", Table: "ods.t"}); err != nil {
		t.Fatal(err)
	}
	if gotSQL != "SELECT COUNT(*) FROM ods.t" {
		t.Fatalf("无分区时不应拼 WHERE: %s", gotSQL)
	}

	if _, err := c.Check(ctx, sensor.Spec{
		Kind: sensor.KindTable, Datasource: "pg", Table: "ods.t", Partition: "dt = '2025-11-10'",
	}); err != nil {
		t.Fatal(err)
	}
	if gotSQL != "SELECT COUNT(*) FROM ods.t WHERE dt = '2025-11-10'" {
		t.Fatalf("分区未拼进 SQL: %s", gotSQL)
	}

	c.Query = func(context.Context, string, string) (string, error) { return "", errors.New("db down") }
	if _, err := c.Check(ctx, sensor.Spec{Kind: sensor.KindTable, Datasource: "pg", Table: "t"}); err == nil {
		t.Fatal("查询错误应向上返回")
	}

	c.Query = func(context.Context, string, string) (string, error) { return "not-a-number", nil }
	_, err := c.Check(ctx, sensor.Spec{Kind: sensor.KindTable, Datasource: "pg", Table: "t"})
	if err == nil || !strings.Contains(err.Error(), "无法解析行数") {
		t.Fatalf("非数字行数应报错，实际 %v", err)
	}
}

// HTTP 传感器的默认实现、自定义期望码与请求失败分支。
func TestCheckHTTPEdges(t *testing.T) {
	if _, err := (&sensor.Checker{}).Check(context.Background(), sensor.Spec{Kind: sensor.KindHTTP}); err == nil {
		t.Fatal("缺 url 应报错")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &sensor.Checker{}
	r, err := c.Check(context.Background(), sensor.Spec{
		Kind: sensor.KindHTTP, URL: srv.URL + "/health", ExpectStatus: http.StatusNoContent,
	})
	if err != nil || !r.Satisfied {
		t.Fatalf("默认 HTTPGet 应识别 204，实际 %+v err=%v", r, err)
	}

	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindHTTP, URL: srv.URL + "/bad"})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "500") {
		t.Fatalf("500 不应满足，实际 %+v err=%v", r, err)
	}

	c.HTTPGet = func(context.Context, string) (int, error) { return 0, errors.New("dial fail") }
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindHTTP, URL: "http://x"})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "请求失败") {
		t.Fatalf("请求失败应转为不满足，实际 %+v err=%v", r, err)
	}

	// 地址可解析但连不上：覆盖默认实现里的 Do 错误分支。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	c.HTTPGet = nil
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindHTTP, URL: deadURL})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "请求失败") {
		t.Fatalf("连接失败应转为不满足，实际 %+v err=%v", r, err)
	}

	// 默认实现里 NewRequest 失败会被收敛成「不满足」，不能返回半成品结果。
	c.HTTPGet = nil
	r, err = c.Check(context.Background(), sensor.Spec{Kind: sensor.KindHTTP, URL: "http://exa mple.com"})
	if err != nil || r.Satisfied || !strings.Contains(r.Detail, "请求失败") {
		t.Fatalf("非法 URL 应转为不满足，实际 %+v err=%v", r, err)
	}
}

// Wait 在满足时立即返回，不空转一个 interval。
func TestWaitSatisfiedImmediately(t *testing.T) {
	c := &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 10, time.Now().Add(-time.Minute), nil
	}}
	start := time.Now()
	r, err := c.Wait(context.Background(), sensor.Spec{
		Kind: sensor.KindFile, Path: "/x", IntervalSec: 30, TimeoutSec: 60,
	})
	if err != nil || !r.Satisfied {
		t.Fatalf("应立即满足，实际 %+v err=%v", r, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("满足后不应继续轮询")
	}
}

// Check 内部错误必须终止等待，不能等满超时。
func TestWaitCheckError(t *testing.T) {
	c := &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 0, time.Time{}, errors.New("boom")
	}}
	if _, err := c.Wait(context.Background(), sensor.Spec{
		Kind: sensor.KindFile, Path: "/x", IntervalSec: 1, TimeoutSec: 60,
	}); err == nil {
		t.Fatal("探测错误应向上返回")
	}
}

// ctx 取消后 Wait 必须尽快返回 ctx.Err()，而不是等满 30 分钟默认超时。
func TestWaitContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 0, time.Time{}, os.ErrNotExist
	}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.Wait(ctx, sensor.Spec{Kind: sensor.KindFile, Path: "/x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，实际 %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("取消后未及时返回")
	}
}

// 超时必须返回明确错误，调用方据此把实例置 FAILED(sensor_timeout)。
func TestWaitTimeout(t *testing.T) {
	c := &sensor.Checker{Stat: func(string) (int64, time.Time, error) {
		return 0, time.Time{}, os.ErrNotExist
	}}
	_, err := c.Wait(context.Background(), sensor.Spec{
		Kind: sensor.KindFile, Path: "/x", IntervalSec: 1, TimeoutSec: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "传感器超时") {
		t.Fatalf("应超时，实际 %v", err)
	}
}

// FromNodeConfig 覆盖所有字段类型与非法结构。
func TestFromNodeConfigEdges(t *testing.T) {
	if _, ok := sensor.FromNodeConfig(nil); ok {
		t.Fatal("无 sensor 应返回 false")
	}
	if _, ok := sensor.FromNodeConfig(map[string]any{"sensor": "file"}); ok {
		t.Fatal("sensor 不是对象应返回 false")
	}
	if _, ok := sensor.FromNodeConfig(map[string]any{"sensor": map[string]any{"kind": 7}}); ok {
		t.Fatal("kind 不是字符串应返回 false")
	}

	spec, ok := sensor.FromNodeConfig(map[string]any{"sensor": map[string]any{
		"kind":          "table",
		"datasource":    "pg",
		"table":         "ods.t",
		"partition":     "dt = '2025-11-10'",
		"url":           "http://x",
		"expectStatus":  int64(204),
		"intervalSec":   float64(5),
		"timeoutSec":    600,
		"stableSeconds": int32(3), // 未支持的类型按 0 处理
	}})
	if !ok {
		t.Fatal("合法配置应返回 true")
	}
	if spec.Kind != sensor.KindTable || spec.Datasource != "pg" || spec.Table != "ods.t" ||
		spec.Partition != "dt = '2025-11-10'" || spec.URL != "http://x" {
		t.Fatalf("字段解析错误: %+v", spec)
	}
	if spec.ExpectStatus != 204 || spec.IntervalSec != 5 || spec.TimeoutSec != 600 {
		t.Fatalf("数字解析错误: %+v", spec)
	}
	if spec.StableSeconds != 0 {
		t.Fatalf("不支持的 int32 应回落 0，实际 %d", spec.StableSeconds)
	}
}

// ConnectorQuery 的四种分支：取连接失败、查询失败、无结果、正常取值。
func TestConnectorQuery(t *testing.T) {
	ctx := context.Background()

	q := sensor.ConnectorQuery(func(context.Context, string) (connector.Connector, error) {
		return nil, errors.New("no datasource")
	})
	if _, err := q(ctx, "pg", "SELECT 1"); err == nil {
		t.Fatal("取连接失败应返回错误")
	}

	q = sensor.ConnectorQuery(func(context.Context, string) (connector.Connector, error) {
		return &fakeConn{queryErr: errors.New("bad sql")}, nil
	})
	if _, err := q(ctx, "pg", "SELECT 1"); err == nil {
		t.Fatal("查询失败应返回错误")
	}

	q = sensor.ConnectorQuery(func(context.Context, string) (connector.Connector, error) {
		return &fakeConn{rows: connector.NewMemRows(nil, nil)}, nil
	})
	if _, err := q(ctx, "pg", "SELECT 1"); err == nil || !strings.Contains(err.Error(), "无结果") {
		t.Fatalf("无结果应报错，实际 %v", err)
	}

	q = sensor.ConnectorQuery(func(context.Context, string) (connector.Connector, error) {
		return &fakeConn{rows: connector.NewMemRows(
			[]connector.ColumnMeta{{Name: "n", Type: "bigint"}},
			[][]any{{int64(42)}, {int64(99)}},
		)}, nil
	})
	got, err := q(ctx, "pg", "SELECT COUNT(*)")
	if err != nil || got != "42" {
		t.Fatalf("应取第一行，实际 %q err=%v", got, err)
	}

	q = sensor.ConnectorQuery(func(context.Context, string) (connector.Connector, error) {
		return &fakeConn{rows: &scanErrRows{}}, nil
	})
	if _, err := q(ctx, "pg", "SELECT 1"); err == nil {
		t.Fatal("Scan 失败应返回错误")
	}
}

// fakeConn 仅实现传感器用到的 Query/Dialect，其余方法保持空实现。
type fakeConn struct {
	rows     connector.Rows
	queryErr error
}

func (c *fakeConn) Type() string                                  { return "fake" }
func (c *fakeConn) Capabilities() connector.Capability            { return connector.Capability{} }
func (c *fakeConn) Dialect() dialect.Dialect                      { return dialect.Postgres{} }
func (c *fakeConn) Test(context.Context) error                    { return nil }
func (c *fakeConn) ListSchemas(context.Context) ([]string, error) { return nil, nil }
func (c *fakeConn) ListTables(context.Context, string) ([]connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) DescribeTable(context.Context, string, string) (*connector.TableMeta, error) {
	return nil, nil
}
func (c *fakeConn) Query(context.Context, string, ...any) (connector.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return c.rows, nil
}
func (c *fakeConn) Exec(context.Context, string, ...any) (connector.Result, error) {
	return connector.Result{}, nil
}
func (c *fakeConn) Stream(context.Context, string, int, func(connector.Rows) error) error {
	return nil
}
func (c *fakeConn) Close() error { return nil }

type scanErrRows struct{}

func (r *scanErrRows) Columns() []connector.ColumnMeta { return nil }
func (r *scanErrRows) Next() bool                      { return true }
func (r *scanErrRows) Scan(...any) error               { return errors.New("scan failed") }
func (r *scanErrRows) Err() error                      { return nil }
func (r *scanErrRows) Close() error                    { return nil }
