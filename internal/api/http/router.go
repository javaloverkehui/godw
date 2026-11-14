package http

import (
	"context"
	"net/http"
	"strings"
)

// paramsKey 用于把路径参数放进 context，避免依赖 Go 1.22 的 PathValue。
type paramsKey struct{}

// Router 是一个极简前缀匹配路由：支持 /a/{id}/b 形式，够用且无依赖。
type Router struct {
	routes []route
}

type route struct {
	method string
	segs   []string
	fn     func(http.ResponseWriter, *http.Request, map[string]string)
}

func NewRouter() *Router { return &Router{} }

// Handle 注册路由。pattern 中 {name} 表示占位段。
func (r *Router) Handle(method, pattern string, fn func(http.ResponseWriter, *http.Request, map[string]string)) {
	r.routes = append(r.routes, route{method: method, segs: splitPath(pattern), fn: fn})
}

func (r *Router) GET(p string, fn func(http.ResponseWriter, *http.Request, map[string]string)) {
	r.Handle(http.MethodGet, p, fn)
}
func (r *Router) POST(p string, fn func(http.ResponseWriter, *http.Request, map[string]string)) {
	r.Handle(http.MethodPost, p, fn)
}
func (r *Router) PUT(p string, fn func(http.ResponseWriter, *http.Request, map[string]string)) {
	r.Handle(http.MethodPut, p, fn)
}
func (r *Router) DELETE(p string, fn func(http.ResponseWriter, *http.Request, map[string]string)) {
	r.Handle(http.MethodDelete, p, fn)
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	segs := splitPath(req.URL.Path)
	for _, rt := range r.routes {
		if rt.method != req.Method {
			continue
		}
		params, ok := matchSegs(rt.segs, segs)
		if !ok {
			continue
		}
		rt.fn(w, req.WithContext(context.WithValue(req.Context(), paramsKey{}, params)), params)
		return
	}
	// 路径存在但方法不对时返回 405，便于排查。
	for _, rt := range r.routes {
		if _, ok := matchSegs(rt.segs, segs); ok {
			writeJSON(w, http.StatusMethodNotAllowed, Response{Code: int(10001), Message: "方法不允许"})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, Response{Code: int(10002), Message: "接口不存在: " + req.URL.Path})
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func matchSegs(pattern, actual []string) (map[string]string, bool) {
	if len(pattern) != len(actual) {
		return nil, false
	}
	params := map[string]string{}
	for i, p := range pattern {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			params[strings.Trim(p, "{}")] = actual[i]
			continue
		}
		if p != actual[i] {
			return nil, false
		}
	}
	return params, true
}

func paramsFrom(ctx context.Context) map[string]string {
	if v, ok := ctx.Value(paramsKey{}).(map[string]string); ok {
		return v
	}
	return map[string]string{}
}
