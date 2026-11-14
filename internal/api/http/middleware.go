package http

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/godw/godw/internal/auth"
	"github.com/godw/godw/internal/auth/rbac"
	"github.com/godw/godw/internal/obs"
	"github.com/godw/godw/internal/obs/log"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxActor
	ctxWorkspace
	ctxRole
)

func requestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func actorOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxActor).(string); ok {
		return v
	}
	return "anonymous"
}

func workspaceOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxWorkspace).(string); ok {
		return v
	}
	return ""
}

func roleOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRole).(string); ok {
		return v
	}
	return "viewer"
}

// withCommon 组合 recover → requestID → accessLog → cors。
func (h *Handler) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = ids.ULID()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)

		// recover 必须最外层：任何 panic 都转成统一 JSON 并记录堆栈。
		defer func() {
			if rec := recover(); rec != nil {
				h.Log.Error("panic recovered",
					"request_id", rid, "path", r.URL.Path,
					"panic", rec, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, Response{
					Code: int(errs.CodeInternal), Message: "服务内部错误", RequestID: rid,
				})
			}
		}()

		if origin := r.Header.Get("Origin"); origin != "" && h.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))

		elapsed := time.Since(start)
		lg := log.From(ctx, h.Log)
		lg.Info("http",
			"request_id", rid, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", elapsed.Milliseconds(), "ip", clientIP(r))

		// 指标：按路由模板而不是原始路径打标签，避免 /instances/xxx 这类高基数标签。
		route := r.Pattern
		if route == "" {
			route = r.URL.Path
		}
		obs.Default.Inc("godw_http_requests_total", "HTTP 请求总数",
			fmt.Sprintf(`method="%s",route="%s",status="%d"`, r.Method, route, sw.status))
		obs.Default.Observe("godw_http_request_duration_seconds", "HTTP 请求耗时（秒）", elapsed.Seconds())
	})
}

// withAuth 解析 Bearer token。dev 环境允许匿名，方便本地验证；
// 生产环境必须登录，未带 token 直接 401。
func (h *Handler) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if hdr := r.Header.Get("Authorization"); strings.HasPrefix(hdr, "Bearer ") {
			token = strings.TrimPrefix(hdr, "Bearer ")
		}
		if token == "" {
			if h.Cfg.Env == "dev" {
				ctx := context.WithValue(r.Context(), ctxActor, "dev")
				ctx = context.WithValue(ctx, ctxWorkspace, h.DefaultWorkspaceID)
				ctx = context.WithValue(ctx, ctxRole, "admin")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			fail(w, r, errs.New(errs.CodeUnauthorized, "缺少访问令牌"))
			return
		}
		claims, err := h.Svc.Signer.Verify(token)
		if err != nil {
			fail(w, r, errs.New(errs.CodeUnauthorized, "令牌无效或已过期"))
			return
		}
		ctx := context.WithValue(r.Context(), ctxActor, claims.Sub)
		ctx = context.WithValue(ctx, ctxWorkspace, claims.WorkspaceID)
		ctx = context.WithValue(ctx, ctxRole, claims.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole 用于写操作。按角色等级判断：admin 天然满足 editor 的要求，
// 不需要每个 handler 都把角色列表写全，避免「加了新角色却忘了改判断」。
func requireRole(w http.ResponseWriter, r *http.Request, roles ...string) bool {
	cur := rbac.Normalize(roleOf(r.Context()))
	for _, required := range roles {
		if rbac.AtLeast(cur, rbac.Normalize(required)) {
			return true
		}
	}
	fail(w, r, errs.New(errs.CodeForbidden,
		"当前角色无权执行该操作").WithMeta("role", cur).WithMeta("required", roles))
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.Split(v, ",")[0]
	}
	return strings.TrimSuffix(r.RemoteAddr, ":0")
}

func (h *Handler) originAllowed(origin string) bool {
	for _, o := range h.Cfg.Server.CORSOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

var _ = auth.Mask

// Middleware 返回带完整中间件链的入口。
// 静态资源与健康检查不鉴权：浏览器加载页面时无法携带 Authorization 头。
func (h *Handler) Middleware() http.Handler {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.withAuth(h.Router).ServeHTTP(w, r)
			return
		}
		h.Router.ServeHTTP(w, r)
	})
	return h.withCommon(base)
}
