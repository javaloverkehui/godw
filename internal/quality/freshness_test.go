package quality

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

// freshConn 只实现 freshness 用到的 Query，其余方法返回零值。
// 用它而不是起真库：这条规则的判定逻辑全在 Go 里，不需要 SQL 参与。
type freshConn struct {
	latest any
	err    error
	lastQ  string
}

func (c *freshConn) Type() string                                      { return "fake" }
func (c *freshConn) Capabilities() connector.Capability                { return connector.Capability{} }
func (c *freshConn) Dialect() dialect.Dialect                          { return dialect.Postgres{} }
func (c *freshConn) Test(ctx context.Context) error                    { return nil }
func (c *freshConn) ListSchemas(ctx context.Context) ([]string, error) { return nil, nil }
func (c *freshConn) ListTables(ctx context.Context, schema string) ([]connector.TableMeta, error) {
	return nil, nil
}
func (c *freshConn) DescribeTable(ctx context.Context, schema, table string) (*connector.TableMeta, error) {
	return nil, nil
}
func (c *freshConn) Query(ctx context.Context, sql string, args ...any) (connector.Rows, error) {
	c.lastQ = sql
	if c.err != nil {
		return nil, c.err
	}
	return connector.NewMemRows(
		[]connector.ColumnMeta{{Name: "latest", Type: "timestamp"}},
		[][]any{{c.latest}}), nil
}
func (c *freshConn) Exec(ctx context.Context, sql string, args ...any) (connector.Result, error) {
	return connector.Result{}, nil
}
func (c *freshConn) Stream(ctx context.Context, sql string, batch int, fn func(connector.Rows) error) error {
	return nil
}
func (c *freshConn) Close() error { return nil }

func freshRule(cfg map[string]any) model.QualityRule {
	return model.QualityRule{Name: "fresh", Kind: KindFreshness, Column: "dt", Severity: "error", Config: cfg}
}

func TestFreshnessBuild(t *testing.T) {
	r, err := BuildWith(freshRule(map[string]any{"max_age_hours": 6}), "dwd", "dwd_order", dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SQL, `MAX("dt")`) {
		t.Fatalf("应取时间列最大值: %s", r.SQL)
	}
	if r.FreshMaxMin != 360 {
		t.Fatalf("6 小时应折算为 360 分钟，实际 %v", r.FreshMaxMin)
	}
	if !strings.Contains(r.Expect, "6 小时") {
		t.Fatalf("期望文案应含 6 小时: %s", r.Expect)
	}
	if r.Kind != KindFreshness {
		t.Fatal("应标记 kind，运行期才会走时间比较分支")
	}
}

func TestFreshnessWindowDefaults(t *testing.T) {
	// 缺省 24 小时：日更链路「昨天的数据」不该被判过期。
	_, label := freshnessWindow(nil)
	if label != "1 天" {
		t.Fatalf("缺省应为 1 天，实际 %s", label)
	}
	if m, _ := freshnessWindow(map[string]any{"max_age_days": 2}); m != 2880 {
		t.Fatalf("2 天应为 2880 分钟，实际 %v", m)
	}
	if m, _ := freshnessWindow(map[string]any{"max_age_min": 30}); m != 30 {
		t.Fatalf("30 分钟应为 30，实际 %v", m)
	}
}

func TestFreshnessRun(t *testing.T) {
	r, err := BuildWith(freshRule(map[string]any{"max_age_hours": 2}), "dwd", "t", dialect.Postgres{})
	if err != nil {
		t.Fatal(err)
	}

	// 1 小时前：通过。
	c := &freshConn{latest: time.Now().Add(-time.Hour)}
	res, err := Run(context.Background(), c, []Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Passed {
		t.Fatalf("1 小时前应通过: %s", res[0].Observed)
	}
	if !strings.Contains(res[0].Observed, "分钟前") {
		t.Fatalf("observed 应写成人话: %s", res[0].Observed)
	}

	// 5 小时前：失败，且文案指出实际年龄。
	c = &freshConn{latest: time.Now().Add(-5 * time.Hour)}
	res, _ = Run(context.Background(), c, []Rule{r})
	if res[0].Passed {
		t.Fatalf("5 小时前应失败: %s", res[0].Observed)
	}

	// 空表：必须判失败。空表放过等于「没数据就算健康」。
	c = &freshConn{latest: nil}
	res, _ = Run(context.Background(), c, []Rule{r})
	if res[0].Passed || res[0].Observed != "无数据" {
		t.Fatalf("空表应判失败并写明无数据: %+v", res[0])
	}

	// 文本时间（ClickHouse 走 HTTP 返回字符串）：也要能判。
	c = &freshConn{latest: time.Now().Add(-10 * time.Minute).Format("2006-01-02 15:04:05")}
	res, _ = Run(context.Background(), c, []Rule{r})
	if !res[0].Passed {
		t.Fatalf("字符串时间应可解析并通过: %s", res[0].Observed)
	}
}

func TestFreshnessNotScoped(t *testing.T) {
	r, _ := BuildWith(freshRule(nil), "dwd", "dwd_order", dialect.Postgres{})
	scoped := Scope([]Rule{r}, "dwd", "dwd_order", dialect.Postgres{}, `"dt" = '2026-11-14'`)
	if strings.Contains(scoped[0].SQL, "godw_scope") {
		t.Fatalf("freshness 不应被分区收窄: %s", scoped[0].SQL)
	}
	normal, _ := BuildWith(qrule(KindNotNull, nil), "dwd", "dwd_order", dialect.Postgres{})
	scoped = Scope([]Rule{normal}, "dwd", "dwd_order", dialect.Postgres{}, `"dt" = '2026-11-14'`)
	if !strings.Contains(scoped[0].SQL, "godw_scope") {
		t.Fatal("其它规则仍应收窄")
	}
}
