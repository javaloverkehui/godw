package api_test

import (
	"net/http"
	"testing"

	"github.com/godw/godw/internal/config"
)

// dev 环境允许匿名访问（本地调试便利），这条分支只在 Env=dev 时生效，
// 生产必须走 token。同时顺带覆盖查询参数的兜底解析。
func TestDevAnonymousAndQueryFallbacks(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) { c.Env = "dev" })

	anon := *e
	anon.token = ""
	me := obj(t, anon.ok("GET", "/api/v1/me", nil))
	if field(t, me, "role") != "admin" || field(t, me, "env") != "dev" {
		t.Fatalf("dev 匿名身份不对: %v", me)
	}
	if field(t, me, "userId") != "dev" {
		t.Fatalf("dev 匿名用户名不对: %v", me)
	}

	// 非法 limit 回落到默认值（50），而不是 400。
	page := obj(t, e.ok("GET", "/api/v1/instances?limit=abc&offset=xyz", nil))
	if fxInt(t, page, "limit") != 50 {
		t.Fatalf("非法 limit 应回落默认值: %v", page)
	}
	// pageSize 超上限被夹到 200；页码超过总数时返回空页而不是越界。
	dsPage := obj(t, e.ok("GET", "/api/v1/datasets?pageSize=999&page=99", nil))
	if fxInt(t, dsPage, "pageSize") != 200 {
		t.Fatalf("pageSize 应被夹到 200: %v", dsPage)
	}
	if items := list(t, dsPage["items"]); len(items) != 0 {
		t.Fatalf("超出范围的页码应返回空页，得到 %d", len(items))
	}

	// X-Forwarded-For 优先作为客户端地址，只取第一跳。
	req, _ := http.NewRequest("GET", e.srv.URL+"/healthz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应为 200，得到 %d", res.StatusCode)
	}
}
