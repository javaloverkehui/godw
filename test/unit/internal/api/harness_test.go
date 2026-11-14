package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	apihttp "github.com/godw/godw/internal/api/http"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

// HTTP 层的用例全部打在真实依赖上：真实 PostgreSQL（每包独占 schema + 正式迁移）、
// 真实 service 与 repo、完整中间件链。handler 里 90% 的代码是「解析 → 调用 → 编码」，
// 用 mock 只能验证调用次数，验证不了契约是否自洽。
func TestMain(m *testing.M) { os.Exit(testsupport.Main(m)) }

type testEnv struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *service.Service
	cfg   config.Config
	token string
	wsID  string
	admin string
}

// newEnv 准备一个干净的元数据库与一套可用的 HTTP 服务。
// 鉴权走真实 token（而非 dev 匿名），保证 withAuth / requireRole 的代码路径被覆盖。
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith 允许在构造 handler 之前改配置，用于验证 CORS、环境开关等分支。
func newEnvWith(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)

	cfg := testsupport.Config(t)
	cfg.Env = "test"
	if mutate != nil {
		mutate(&cfg)
	}
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc, err := service.New(db, cfg, lg)
	if err != nil {
		t.Fatalf("构造 service 失败: %v", err)
	}
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("初始化工作空间失败: %v", err)
	}
	token, err := svc.Signer.Sign("usr_test_admin", ws.ID, "admin")
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	h := apihttp.NewHandler(svc, cfg, lg, ws.ID)
	srv := httptest.NewServer(h.Middleware())
	t.Cleanup(srv.Close)
	// 每个用例都会新建一个 Service，而 Service 内部的连接器池默认持有连接。
	// 不主动关闭的话，几十个用例累积起来会打满 PostgreSQL 的 max_connections，
	// 表现为「随机某个用例建数据源失败」——比真 bug 更难排查。
	t.Cleanup(svc.Pool.Close)

	return &testEnv{t: t, srv: srv, svc: svc, cfg: cfg, token: token, wsID: ws.ID, admin: "usr_test_admin"}
}

// as 切换当前请求使用的角色（生成对应角色的 token），用于验证权限门禁。
func (e *testEnv) as(role string) *testEnv {
	e.t.Helper()
	token, err := e.svc.Signer.Sign("usr_"+role, e.wsID, role)
	if err != nil {
		e.t.Fatalf("签发 %s 令牌失败: %v", role, err)
	}
	cp := *e
	cp.token = token
	return &cp
}

// do 发一次请求，返回状态码与解码后的 Response.Data（非 JSON 时为 nil）。
func (e *testEnv) do(method, path string, body any) (int, any) {
	e.t.Helper()
	status, data, _ := e.doRaw(method, path, body)
	return status, data
}

// doRaw 额外返回原始响应体，用于断言 HTML / DOT / CSV 这类非 JSON 输出。
func (e *testEnv) doRaw(method, path string, body any) (int, any, string) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			rdr = strings.NewReader(v)
		case []byte:
			rdr = bytes.NewReader(v)
		default:
			buf, err := json.Marshal(v)
			if err != nil {
				e.t.Fatalf("序列化请求体失败: %v", err)
			}
			rdr = bytes.NewReader(buf)
		}
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatalf("读取响应失败: %v", err)
	}
	var resp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return res.StatusCode, nil, string(raw)
	}
	if len(resp.Data) == 0 {
		return res.StatusCode, nil, string(raw)
	}
	var data any
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return res.StatusCode, nil, string(raw)
	}
	return res.StatusCode, data, string(raw)
}

// ok 断言成功（2xx）并返回 data。
func (e *testEnv) ok(method, path string, body any) any {
	e.t.Helper()
	status, data, raw := e.doRaw(method, path, body)
	if status < 200 || status >= 300 {
		e.t.Fatalf("%s %s 期望成功，得到 %d: %s", method, path, status, raw)
	}
	return data
}

// fail 断言失败（>=400）并返回错误消息，用于校验参数校验与权限门禁。
func (e *testEnv) fail(method, path string, body any, wantStatus int) string {
	e.t.Helper()
	status, _, raw := e.doRaw(method, path, body)
	if wantStatus != 0 && status != wantStatus {
		e.t.Fatalf("%s %s 期望 %d，得到 %d: %s", method, path, wantStatus, status, raw)
	}
	if status < 400 {
		e.t.Fatalf("%s %s 期望失败，却得到 %d: %s", method, path, status, raw)
	}
	var resp struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(raw), &resp)
	return resp.Message
}

// obj 把 data 断言成 map，便于链式取字段。
func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望对象，得到 %T: %v", v, v)
	}
	return m
}

// list 把 data 断言成数组。
func list(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("期望数组，得到 %T: %v", v, v)
	}
	return a
}

// field 读取对象里的字符串字段。
func field(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("字段 %s 不存在: %v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("字段 %s 不是字符串: %T", key, v)
	}
	return s
}

// ---------- 数据源夹具 ----------

// sourceSchema 建一个独立 schema 供执行引擎建表，避免污染 public。
func (e *testEnv) sourceSchema() string {
	e.t.Helper()
	schema := "src_" + strings.ToLower(randomSuffix())
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testsupport.DSN())
	if err != nil {
		e.t.Fatalf("连接测试库失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		conn.Close(ctx)
		e.t.Fatalf("创建源 schema 失败: %v", err)
	}
	e.t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), testsupport.DSN())
		if err == nil {
			_, _ = c.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			_ = c.Close(context.Background())
		}
	})
	return schema
}

// pgDatasource 登记一个指向测试库的数据源，返回数据源名。
func (e *testEnv) pgDatasource(name string) string {
	e.t.Helper()
	u, err := url.Parse(testsupport.DSN())
	if err != nil {
		e.t.Fatalf("解析测试库 DSN 失败: %v", err)
	}
	pass, _ := u.User.Password()
	host, port, _ := net.SplitHostPort(u.Host)
	dbName := strings.TrimPrefix(u.Path, "/")
	conf := map[string]any{
		"host": host, "port": port, "user": u.User.Username(),
		"database": dbName, "sslmode": "disable",
		// 每个用例都会登记一次数据源；把单连接器上限压到 2，
		// 避免整套用例把测试库的连接数打满。
		"max_conns": 2,
	}
	e.ok("POST", "/api/v1/datasources", map[string]any{
		"name": name, "type": "postgres", "config": conf,
		"secret": map[string]string{"password": pass},
	})
	return name
}

func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}
