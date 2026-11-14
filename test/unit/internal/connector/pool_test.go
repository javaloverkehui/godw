package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

// fakeConn 隔离 Pool 的连接生命周期：不触碰任何真实数据源。
type fakeConn struct {
	typ     string
	closed  bool
	testErr error
}

func (f *fakeConn) Type() string                                  { return f.typ }
func (f *fakeConn) Capabilities() connector.Capability            { return connector.Capability{} }
func (f *fakeConn) Dialect() dialect.Dialect                      { return namedDialect{name: f.typ} }
func (f *fakeConn) Test(context.Context) error                    { return f.testErr }
func (f *fakeConn) ListSchemas(context.Context) ([]string, error) { return nil, nil }
func (f *fakeConn) ListTables(context.Context, string) ([]connector.TableMeta, error) {
	return nil, nil
}
func (f *fakeConn) DescribeTable(context.Context, string, string) (*connector.TableMeta, error) {
	return nil, nil
}
func (f *fakeConn) Query(context.Context, string, ...any) (connector.Rows, error) { return nil, nil }
func (f *fakeConn) Exec(context.Context, string, ...any) (connector.Result, error) {
	return connector.Result{}, nil
}
func (f *fakeConn) Stream(context.Context, string, int, func(connector.Rows) error) error {
	return nil
}
func (f *fakeConn) Close() error {
	f.closed = true
	return nil
}

type namedDialect struct {
	dialect.ANSI
	name string
}

func (d namedDialect) Name() string { return d.name }

// 注册一个可控的假连接器，用于验证构造函数分发与 Pool 的连接生命周期。
// 这属于测试脚手架，不会出现在生产二进制的类型列表里。
var (
	createdConns []*fakeConn
	openedCount  int
)

func init() {
	connector.Register("coverfake", func(_ context.Context, spec connector.Spec) (connector.Connector, error) {
		switch spec.Get("mode", "") {
		case "plain":
			return nil, errors.New("factory boom")
		case "app":
			return nil, errs.New(errs.CodeUnavailable, "fake 不可用")
		}
		fc := &fakeConn{typ: "coverfake"}
		if spec.Get("mode", "") == "testfail" {
			fc.testErr = errors.New("连通性测试失败")
		}
		openedCount++
		createdConns = append(createdConns, fc)
		return fc, nil
	})
}

func resetTrack() {
	openedCount = 0
	createdConns = nil
}

// TTL 配置错误会让连接要么永不回收、要么刚建就被回收，这里守住默认值边界。
func TestNewPoolTTL(t *testing.T) {
	// ttl<=0 必须回落到 10 分钟：9 分钟不回收、11 分钟回收。
	for _, ttl := range []time.Duration{0, -time.Second} {
		resetTrack()
		p := connector.NewPool(ttl)
		if _, err := p.Acquire(context.Background(), connector.Spec{ID: "ttl", Type: "coverfake"}); err != nil {
			t.Fatal(err)
		}
		base := time.Now()
		if n := p.Reap(base.Add(9 * time.Minute)); n != 0 {
			t.Fatalf("ttl=%v 时 9 分钟就回收了连接，默认值可能不是 10 分钟", ttl)
		}
		if n := p.Reap(base.Add(11 * time.Minute)); n != 1 {
			t.Fatalf("ttl=%v 时 11 分钟仍未回收，默认 TTL 未生效", ttl)
		}
	}

	// 显式 TTL 必须被尊重。
	resetTrack()
	p := connector.NewPool(20 * time.Millisecond)
	if _, err := p.Acquire(context.Background(), connector.Spec{ID: "ttl2", Type: "coverfake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	if n := p.Reap(time.Now()); n != 1 {
		t.Fatalf("自定义 TTL 到期未回收: %d", n)
	}
}

// 复用失效会导致每个任务新建连接，最终打满数据源连接数。
func TestPoolAcquireReuse(t *testing.T) {
	resetTrack()
	p := connector.NewPool(100 * time.Millisecond)
	spec := connector.Spec{ID: "ds-1", Type: "coverfake"}
	c1, err := p.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// 让两次获取产生可测量的时间差：命中缓存必须刷新 lastUsed，
	// 否则活跃连接会在第一次获取后满 TTL 时被误回收。
	time.Sleep(60 * time.Millisecond)
	c2, err := p.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("同一 datasource_id 必须复用同一个连接器实例")
	}
	if openedCount != 1 {
		t.Fatalf("只应打开一次连接，实际 %d 次", openedCount)
	}
	if p.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", p.Len())
	}
	// 距第一次获取已 130ms（>TTL），距刷新后的 lastUsed 仅 70ms。
	if n := p.Reap(time.Now().Add(70 * time.Millisecond)); n != 0 {
		t.Fatalf("刚使用的连接被回收，lastUsed 未刷新: %d", n)
	}
}

func TestPoolAcquireOpenError(t *testing.T) {
	resetTrack()
	p := connector.NewPool(time.Minute)
	c, err := p.Acquire(context.Background(), connector.Spec{ID: "ds-x", Type: "no-such-type"})
	if c != nil {
		t.Fatal("打开失败不应返回半成品连接器")
	}
	if !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("错误应原样透出，得到 %v", err)
	}
	if p.Len() != 0 {
		t.Fatalf("失败结果不得写入缓存，Len() = %d", p.Len())
	}
}

// 配置变更后旧连接必须关闭并从缓存移除，否则会继续用过期凭据连接。
func TestPoolInvalidate(t *testing.T) {
	resetTrack()
	p := connector.NewPool(time.Minute)
	spec := connector.Spec{ID: "ds-1", Type: "coverfake"}
	c1, err := p.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	p.Invalidate("ds-1")
	if !createdConns[0].closed {
		t.Fatal("Invalidate 必须关闭底层连接")
	}
	if p.Len() != 0 {
		t.Fatalf("Invalidate 后缓存应为空，Len() = %d", p.Len())
	}
	// 不存在的 ID 重复失效不应 panic。
	p.Invalidate("ds-1")

	c2, err := p.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if openedCount != 2 {
		t.Fatalf("失效后应重建连接，opens = %d", openedCount)
	}
	if c1 == c2 {
		t.Fatal("失效后不应复用旧连接器")
	}
}

// Reap 关闭空闲超时的连接，未超时（含刚使用）的连接必须保留。
func TestPoolReap(t *testing.T) {
	resetTrack()
	p := connector.NewPool(10 * time.Minute)
	if _, err := p.Acquire(context.Background(), connector.Spec{ID: "a", Type: "coverfake"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := p.Acquire(context.Background(), connector.Spec{ID: "b", Type: "coverfake"}); err != nil {
		t.Fatal(err)
	}

	if n := p.Reap(now.Add(-20 * time.Minute)); n != 0 {
		t.Fatalf("时间倒退不应回收任何连接: %d", n)
	}
	if n := p.Reap(time.Now().Add(30 * time.Minute)); n != 2 {
		t.Fatalf("全部超时应回收 2 个，实际 %d", n)
	}
	if p.Len() != 0 {
		t.Fatalf("回收后 Len() = %d", p.Len())
	}
	for i, c := range createdConns {
		if !c.closed {
			t.Fatalf("第 %d 个连接未被关闭", i)
		}
	}
	if n := p.Reap(time.Now()); n != 0 {
		t.Fatalf("空池 Reap 应为 0，实际 %d", n)
	}
}

func TestPoolClose(t *testing.T) {
	resetTrack()
	p := connector.NewPool(time.Minute)
	for _, id := range []string{"a", "b"} {
		if _, err := p.Acquire(context.Background(), connector.Spec{ID: id, Type: "coverfake"}); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	if p.Len() != 0 {
		t.Fatalf("Close 后应清空缓存，Len() = %d", p.Len())
	}
	for i, c := range createdConns {
		if !c.closed {
			t.Fatalf("第 %d 个连接未被关闭", i)
		}
	}
	// 幂等：重复关闭不应 panic。
	p.Close()
}

// New 的错误分类决定了前端提示与重试策略：未知类型是参数错误，不是连接故障。
func TestNewDispatch(t *testing.T) {
	ctx := context.Background()

	if _, err := connector.New(ctx, connector.Spec{Type: "no-such-type"}); err == nil {
		t.Fatal("未知类型必须报错")
	} else if !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("未知类型错误码应为 CodeInvalidArgument，得到 %v", err)
	} else if !strings.Contains(err.Error(), "no-such-type") {
		t.Fatalf("错误信息应带原始类型名: %v", err)
	}

	c, err := connector.New(ctx, connector.Spec{Type: "COVERFAKE"})
	if err != nil {
		t.Fatalf("类型名应大小写不敏感: %v", err)
	}
	if c.Type() != "coverfake" {
		t.Fatalf("构造出的类型错误: %s", c.Type())
	}
	_ = c.Close()

	// 工厂返回普通错误时统一归为「数据源不可达」，并附上定位信息。
	_, err = connector.New(ctx, connector.Spec{Type: "coverfake", Name: "ds-name", Config: map[string]any{"mode": "plain"}})
	if !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("普通错误应包装成 CodeDatasourceUnreachable，得到 %v", err)
	}
	ae := errs.From(err)
	if ae.Meta["datasource"] != "ds-name" || ae.Meta["type"] != "coverfake" {
		t.Fatalf("包装错误应带上数据源定位信息: %+v", ae.Meta)
	}

	// 工厂已经给出业务错误码时必须原样透出，不能被笼统的「打开失败」覆盖。
	_, err = connector.New(ctx, connector.Spec{Type: "coverfake", Config: map[string]any{"mode": "app"}})
	if !errs.Is(err, errs.CodeUnavailable) {
		t.Fatalf("业务错误码不应被改写，得到 %v", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册必须 panic，否则测试里会静默覆盖真实工厂")
		}
	}()
	connector.Register("coverfake", func(context.Context, connector.Spec) (connector.Connector, error) {
		return nil, nil
	})
}

func TestOpen(t *testing.T) {
	ctx := context.Background()

	ok, err := connector.Open(ctx, connector.Spec{Type: "coverfake"})
	if err != nil {
		t.Fatalf("连通性正常时 Open 不应失败: %v", err)
	}
	if ok.Type() != "coverfake" {
		t.Fatalf("Open 返回了错误类型: %s", ok.Type())
	}
	_ = ok.Close()

	// 构造成功但连通性失败：Open 必须关闭连接并返回不可达。
	resetTrack()
	if _, err := connector.Open(ctx, connector.Spec{Type: "coverfake", Name: "unreachable", Config: map[string]any{"mode": "testfail"}}); err == nil {
		t.Fatal("连通性失败不应通过 Open")
	} else if !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("Open 失败应归为 CodeDatasourceUnreachable，得到 %v", err)
	}
	if len(createdConns) != 1 || !createdConns[0].closed {
		t.Fatal("Open 失败后必须关闭已构造的连接")
	}

	// 构造阶段失败应直接返回，不再做连通性测试。
	if _, err := connector.Open(ctx, connector.Spec{Type: "no-such-type"}); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("Open 应保留构造阶段的错误码，得到 %v", err)
	}
}

func TestSpecGet(t *testing.T) {
	spec := connector.Spec{Config: map[string]any{
		"host":  "db.example",
		"empty": "",
		"num":   42,
	}}
	cases := []struct {
		key, def, want string
	}{
		{"host", "127.0.0.1", "db.example"},
		{"empty", "fallback", "fallback"}, // 空串按未配置处理
		{"missing", "fallback", "fallback"},
		{"num", "fallback", "fallback"}, // 非字符串不能强转
	}
	for _, tc := range cases {
		if got := spec.Get(tc.key, tc.def); got != tc.want {
			t.Errorf("Get(%q,%q) = %q, want %q", tc.key, tc.def, got, tc.want)
		}
	}
}

func TestSpecGetInt(t *testing.T) {
	spec := connector.Spec{Config: map[string]any{
		"i":    7,
		"i64":  int64(8),
		"f":    float64(9.9),
		"s":    "10",
		"int8": int8(1),
	}}
	cases := []struct {
		key  string
		def  int
		want int
	}{
		{"i", 0, 7},
		{"i64", 0, 8},
		{"f", 0, 9},
		{"s", 42, 42},    // 字符串不做隐式转换
		{"int8", 42, 42}, // 只接受三种明确类型
		{"missing", 42, 42},
	}
	for _, tc := range cases {
		if got := spec.GetInt(tc.key, tc.def); got != tc.want {
			t.Errorf("GetInt(%q,%d) = %d, want %d", tc.key, tc.def, got, tc.want)
		}
	}
}

// BuildDSN 会出现在日志与 API 响应里，绝不能带出账号密码。
func TestBuildDSN(t *testing.T) {
	cases := []struct {
		name string
		spec connector.Spec
		want string
	}{
		{"完整", connector.Spec{Type: "postgres", Config: map[string]any{
			"host": "db.local", "port": 5433, "database": "dw",
		}}, "postgres://db.local:5433/dw"},
		{"无端口", connector.Spec{Type: "mysql", Config: map[string]any{
			"host": "db.local", "database": "dw",
		}}, "mysql://db.local/dw"},
		{"零端口不输出", connector.Spec{Type: "mysql", Config: map[string]any{
			"host": "db.local", "port": 0,
		}}, "mysql://db.local"},
		{"无主机只有库", connector.Spec{Type: "clickhouse", Config: map[string]any{
			"database": "default",
		}}, "clickhouse:///default"},
		{"空配置", connector.Spec{Type: "duckdb"}, "duckdb://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := connector.BuildDSN(tc.spec)
			if got != tc.want {
				t.Fatalf("BuildDSN = %q, want %q", got, tc.want)
			}
			for _, secret := range []string{"password", "godw", "@"} {
				if strings.Contains(got, secret) {
					t.Fatalf("DSN 泄露了敏感内容 %q: %s", secret, got)
				}
			}
		})
	}
}

func TestTypesSorted(t *testing.T) {
	types := connector.Types()
	if len(types) == 0 {
		t.Fatal("至少应有已注册类型")
	}
	for i := 1; i < len(types); i++ {
		if types[i-1] > types[i] {
			t.Fatalf("Types() 必须排序，%q 在 %q 之后", types[i-1], types[i])
		}
	}
	found := false
	for _, got := range types {
		if got == "coverfake" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少已注册类型 coverfake: %v", types)
	}
}

func TestRegistryListing(t *testing.T) {
	reg := connector.Registry()
	if len(reg) != len(connector.Types()) {
		t.Fatalf("Registry 数量 %d 与 Types 数量 %d 不一致", len(reg), len(connector.Types()))
	}
	for _, typ := range connector.Types() {
		v, ok := reg[typ].(map[string]any)
		if !ok || v["type"] != typ {
			t.Fatalf("Registry[%q] 结构错误: %#v", typ, reg[typ])
		}
	}
}
