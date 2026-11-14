package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/godw/godw/internal/config"
)

type ctxKey struct{}

// Fields 是贯穿全链路的关联字段。缺省时省略，避免日志噪音。
type Fields struct {
	RequestID   string
	TraceID     string
	WorkspaceID string
	InstanceID  string
}

// WithFields 把关联字段放进 context，后续所有日志自动带上。
func WithFields(ctx context.Context, f Fields) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

// FieldsFrom 读取 context 中的关联字段。
func FieldsFrom(ctx context.Context) Fields {
	if v, ok := ctx.Value(ctxKey{}).(Fields); ok {
		return v
	}
	return Fields{}
}

// New 构造结构化日志器。字段名固定为 ts/level/msg/request_id/trace_id/workspace_id/instance_id，
// 采集端只需按这一份约定建索引。
func New(cfg config.LogConfig) *slog.Logger {
	return NewWithWriter(cfg, os.Stdout)
}

func NewWithWriter(cfg config.LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: parseLevel(cfg.Level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "ts"
			case slog.LevelKey:
				a.Key = "level"
				if lv, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(strings.ToLower(lv.String()))
				}
			}
			return a
		},
	}
	var h slog.Handler
	if cfg.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// From 返回带 context 字段的 logger。业务代码统一用它，禁止直接用全局 logger。
func From(ctx context.Context, base *slog.Logger) *slog.Logger {
	f := FieldsFrom(ctx)
	args := make([]any, 0, 8)
	if f.RequestID != "" {
		args = append(args, "request_id", f.RequestID)
	}
	if f.TraceID != "" {
		args = append(args, "trace_id", f.TraceID)
	}
	if f.WorkspaceID != "" {
		args = append(args, "workspace_id", f.WorkspaceID)
	}
	if f.InstanceID != "" {
		args = append(args, "instance_id", f.InstanceID)
	}
	if len(args) == 0 {
		return base
	}
	return base.With(args...)
}
