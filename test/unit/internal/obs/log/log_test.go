package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/obs/log"
)

// 守住采集端的字段约定：ts/level/msg 三个键必须按约定改名，
// level 必须是小写字符串，否则索引侧聚合会整批失配。
func TestJSONFormatUsesAgreedKeys(t *testing.T) {
	var buf bytes.Buffer
	l := log.NewWithWriter(config.LogConfig{Level: "info", Format: "json"}, &buf)
	l.Info("导入完成", "rows", 12)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, buf.String())
	}
	if _, ok := got["ts"]; !ok {
		t.Fatalf("缺少 ts 字段: %#v", got)
	}
	if got["level"] != "info" {
		t.Fatalf("level 应为小写 info，实际 %#v", got["level"])
	}
	if got["msg"] != "导入完成" {
		t.Fatalf("msg 不符: %#v", got["msg"])
	}
	if got["rows"] != float64(12) {
		t.Fatalf("业务字段丢失: %#v", got)
	}
}

// 守住 text 格式分支：本地调试依赖可读输出，分支写错会静默退回 JSON。
func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := log.NewWithWriter(config.LogConfig{Level: "debug", Format: "text"}, &buf)
	l.Debug("hello", "k", "v")

	out := buf.String()
	if out == "" {
		t.Fatal("text 格式应产生输出")
	}
	for _, want := range []string{"level=debug", "msg=hello", "k=v", "ts="} {
		if !strings.Contains(out, want) {
			t.Fatalf("text 输出缺少 %q: %s", want, out)
		}
	}
}

// 守住级别过滤：配置 error 时 debug/info 必须被丢弃，
// 否则生产环境会被低价值日志淹没。
func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := log.NewWithWriter(config.LogConfig{Level: "error", Format: "json"}, &buf)
	l.Debug("debug 不该出现")
	l.Info("info 不该出现")
	if buf.Len() != 0 {
		t.Fatalf("error 级别下不应输出低级别日志: %s", buf.String())
	}
	l.Error("error 应出现")
	if !strings.Contains(buf.String(), "error 应出现") {
		t.Fatalf("error 级别日志丢失: %s", buf.String())
	}
}

// 守住级别解析的每个分支，包含大小写混写与未登记值。
// 未登记值必须退回 info，不能落到 debug 把敏感信息全打出来。
// 通过 Enabled 间接观察，避免依赖未导出的解析函数。
func TestLevelParsing(t *testing.T) {
	cases := []struct {
		level     string
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
		desc      string
	}{
		{"debug", true, true, true, "debug 下所有级别都放行"},
		{"DEBUG", true, true, true, "大小写不敏感"},
		{"warn", false, false, true, "warn 只放行 warn 及以上"},
		{"Warn", false, false, true, "混合大小写同样生效"},
		{"error", false, false, false, "error 连 warn 都拒绝"},
		{"info", false, true, true, "info 为默认档"},
		{"", false, true, true, "空值退回 info"},
		{"trace", false, true, true, "未登记值退回 info"},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			l := log.NewWithWriter(config.LogConfig{Level: tc.level, Format: "json"}, io.Discard)
			if got := l.Enabled(ctx, slog.LevelDebug); got != tc.wantDebug {
				t.Fatalf("%s：debug 放行 = %v，期望 %v", tc.desc, got, tc.wantDebug)
			}
			if got := l.Enabled(ctx, slog.LevelInfo); got != tc.wantInfo {
				t.Fatalf("%s：info 放行 = %v，期望 %v", tc.desc, got, tc.wantInfo)
			}
			if got := l.Enabled(ctx, slog.LevelWarn); got != tc.wantWarn {
				t.Fatalf("%s：warn 放行 = %v，期望 %v", tc.desc, got, tc.wantWarn)
			}
		})
	}
}

// 守住 context 关联字段的注入：四个字段都要出现在日志里，
// 少了 request_id 就无法把一条请求的多条日志串起来。
func TestFromInjectsContextFields(t *testing.T) {
	var buf bytes.Buffer
	base := log.NewWithWriter(config.LogConfig{Level: "info", Format: "json"}, &buf)
	ctx := log.WithFields(context.Background(), log.Fields{
		RequestID:   "req-1",
		TraceID:     "trace-1",
		WorkspaceID: "ws-1",
		InstanceID:  "inst-1",
	})

	log.From(ctx, base).Info("run")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, buf.String())
	}
	want := map[string]string{
		"request_id":   "req-1",
		"trace_id":     "trace-1",
		"workspace_id": "ws-1",
		"instance_id":  "inst-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("字段 %s = %#v，期望 %q（完整输出 %#v）", k, got[k], v, got)
		}
	}
}

// 守住「缺省时省略」的约定：只填 trace_id 时不应出现空的 request_id 键，
// 否则日志体积膨胀且索引区分度下降。
func TestFromOmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	base := log.NewWithWriter(config.LogConfig{Level: "info", Format: "json"}, &buf)
	ctx := log.WithFields(context.Background(), log.Fields{TraceID: "trace-only"})

	log.From(ctx, base).Info("run")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, buf.String())
	}
	if got["trace_id"] != "trace-only" {
		t.Fatalf("trace_id 丢失: %#v", got)
	}
	for _, k := range []string{"request_id", "workspace_id", "instance_id"} {
		if _, ok := got[k]; ok {
			t.Fatalf("空字段 %s 不应出现: %#v", k, got)
		}
	}
}

// 守住零字段时直接返回原 logger：否则每次请求都会多包一层 With，
// 长期运行会累积无意义的 handler 链。
func TestFromWithoutFieldsReturnsBase(t *testing.T) {
	var buf bytes.Buffer
	base := log.NewWithWriter(config.LogConfig{Level: "info", Format: "json"}, &buf)
	if got := log.From(context.Background(), base); got != base {
		t.Fatal("无关联字段时应返回原 logger 指针")
	}
}

// 守住 FieldsFrom 的零值兜底：context 中没有关联字段时必须返回零值而不是 panic。
func TestFieldsFromEmptyContext(t *testing.T) {
	if got := log.FieldsFrom(context.Background()); got != (log.Fields{}) {
		t.Fatalf("空 context 应返回零值，实际 %#v", got)
	}
}

// 守住 New 的默认输出目标可用（stdout），避免启动时拿到 nil logger 直接 panic。
func TestNewReturnsUsableLogger(t *testing.T) {
	l := log.New(config.LogConfig{Level: "error", Format: "json"})
	if l == nil {
		t.Fatal("New 不应返回 nil")
	}
	if l.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("error 级别下 info 应被禁用")
	}
}
