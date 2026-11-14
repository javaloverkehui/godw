package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/godw/godw/internal/config"
)

// 登录 / 鉴权 / 中间件是每个请求的必经之路，先用最小的用例把这条路踩实。
func TestAuthAndCommon(t *testing.T) {
	e := newEnv(t)

	// 健康检查与指标不需要令牌：浏览器与 Prometheus 都拿不到登录态。
	if status, _, _ := e.doRaw("GET", "/healthz", nil); status != http.StatusOK {
		t.Fatalf("healthz 应为 200，得到 %d", status)
	}
	if status, _, _ := e.doRaw("GET", "/metrics", nil); status != http.StatusOK {
		t.Fatalf("metrics 应为 200，得到 %d", status)
	}

	// 无令牌：401，且不能泄露内部信息。
	anon := *e
	anon.token = ""
	if status, _, raw := anon.doRaw("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("匿名访问应为 401，得到 %d: %s", status, raw)
	}
	anon.token = "not-a-jwt"
	if status, _, _ := anon.doRaw("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("非法令牌应为 401，得到 %d", status)
	}

	// 登录成功：返回 token 与用户信息。
	data := obj(t, e.ok("POST", "/api/v1/auth/login", map[string]any{
		"email": "admin@test.local", "password": "test-pass",
	}))
	if field(t, data, "token") == "" {
		t.Fatal("登录应返回 token")
	}
	user := obj(t, data["user"])
	if field(t, user, "email") != "admin@test.local" {
		t.Fatalf("登录返回的用户不对: %v", user)
	}

	// 密码错误与用户不存在必须返回同样的文案，避免枚举账号。
	msg1 := e.fail("POST", "/api/v1/auth/login", map[string]any{
		"email": "admin@test.local", "password": "wrong",
	}, http.StatusUnauthorized)
	msg2 := e.fail("POST", "/api/v1/auth/login", map[string]any{
		"email": "ghost@test.local", "password": "whatever",
	}, http.StatusUnauthorized)
	if msg1 != msg2 {
		t.Fatalf("两种失败应返回同一文案，得到 %q / %q", msg1, msg2)
	}

	// 非法请求体：400。
	e.fail("POST", "/api/v1/auth/login", `{"email":`, http.StatusBadRequest)

	// me 返回当前身份。
	me := obj(t, e.ok("GET", "/api/v1/me", nil))
	if field(t, me, "userId") != e.admin || field(t, me, "env") != "test" {
		t.Fatalf("me 返回不对: %v", me)
	}
}

// 路由边界：方法不匹配 405、路径不存在 404、CORS 预检 204。
func TestRouterEdges(t *testing.T) {
	e := newEnv(t)
	if status, _, _ := e.doRaw("DELETE", "/api/v1/me", nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("方法不匹配应为 405，得到 %d", status)
	}
	if status, _, _ := e.doRaw("GET", "/api/v1/definitely-not-exist", nil); status != http.StatusNotFound {
		t.Fatalf("未知路径应为 404，得到 %d", status)
	}

	// 未配置 CORS 白名单时不能回显 Origin。
	req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("预检应为 204，得到 %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("未授权来源不应回显 CORS 头，得到 %q", got)
	}

	// 请求 ID 由网关透传或本地生成。
	req3, _ := http.NewRequest("GET", e.srv.URL+"/healthz", nil)
	req3.Header.Set("X-Request-Id", "rid-123")
	res3, err := e.srv.Client().Do(req3)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	res3.Body.Close()
	if got := res3.Header.Get("X-Request-Id"); got != "rid-123" {
		t.Fatalf("应透传 X-Request-Id，得到 %q", got)
	}
}

// CORS 白名单命中时回显 Origin（配置必须在建 handler 之前生效）。
func TestCORSAllowList(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Server.CORSOrigins = []string{"https://app.example"}
	})
	req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Origin", "https://app.example")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	res.Body.Close()
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("白名单来源应回显，得到 %q", got)
	}
}

// viewer 只能读，不能写；editor 可写；admin 全通。
func TestRoleGates(t *testing.T) {
	e := newEnv(t)
	viewer := e.as("viewer")
	viewer.fail("POST", "/api/v1/models", map[string]any{"yaml": "kind: Model"}, http.StatusForbidden)

	editor := e.as("editor")
	// editor 写操作放行到业务校验，拿到 400 而不是 403。
	if msg := editor.fail("POST", "/api/v1/models", map[string]any{"yaml": "kind: Model"}, http.StatusBadRequest); strings.Contains(msg, "无权") {
		t.Fatalf("editor 不应被权限拦截: %q", msg)
	}
}
