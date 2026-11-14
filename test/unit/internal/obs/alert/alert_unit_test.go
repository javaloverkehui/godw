package alert_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/obs/alert"
)

func fact() alert.Fact {
	now := time.Date(2025, 11, 10, 3, 0, 0, 0, time.Local)
	return alert.Fact{
		Pipeline: "dwd_order_daily", InstanceID: "ins_1", Status: "success",
		BizDate: "2025-11-09", StartedAt: now.Add(-20 * time.Minute), FinishedAt: now,
		RowsTotal: 1000, PrevRows: 1000, Now: now,
	}
}

// 守住总开关：未启用的规则一律不触发，无论事实多严重。
func TestEvaluateDisabled(t *testing.T) {
	f := fact()
	f.Status = "failed"
	if ev := alert.Evaluate(alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: false}, f); ev != nil {
		t.Fatalf("禁用规则不应触发: %+v", ev)
	}
}

// 守住目标匹配：* 与空串是通配，具体名字只匹配同名管道。
func TestEvaluateTargetMatching(t *testing.T) {
	f := fact()
	f.Status = "failed"
	cases := []struct {
		target string
		want   bool
	}{
		{"*", true},
		{"", true},
		{"dwd_order_daily", true},
		{"other_pipeline", false},
	}
	for _, c := range cases {
		got := alert.Evaluate(alert.Rule{Kind: alert.KindFailure, Target: c.target, Enabled: true}, f) != nil
		if got != c.want {
			t.Errorf("target=%q 触发=%v，期望 %v", c.target, got, c.want)
		}
	}
}

// 守住六类规则的命中与不命中：表驱动覆盖每个分支的两侧。
func TestEvaluateKinds(t *testing.T) {
	base := fact()
	at := func(h, m int) time.Time { return time.Date(2025, 11, 10, h, m, 0, 0, time.Local) }

	cases := []struct {
		name string
		rule alert.Rule
		fact alert.Fact
		want bool
	}{
		{
			name: "失败命中",
			rule: alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "failed"; f.Error = "boom"; return f }(),
			want: true,
		},
		{
			name: "失败但状态成功不命中",
			rule: alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true},
			fact: base,
			want: false,
		},
		{
			name: "deadline 未配置不触发",
			rule: alert.Rule{Kind: alert.KindDeadline, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "running"; return f }(),
			want: false,
		},
		{
			name: "deadline 格式非法不触发",
			rule: alert.Rule{Kind: alert.KindDeadline, Deadline: "25:99", Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "running"; f.Now = at(9, 30); return f }(),
			want: false,
		},
		{
			name: "超过 SLA 命中",
			rule: alert.Rule{Kind: alert.KindDeadline, Deadline: "09:00", Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "running"; f.Now = at(9, 30); return f }(),
			want: true,
		},
		{
			name: "未到 SLA 不触发",
			rule: alert.Rule{Kind: alert.KindDeadline, Deadline: "09:00", Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "running"; f.Now = at(8, 59); return f }(),
			want: false,
		},
		{
			name: "已完成不触发 deadline",
			rule: alert.Rule{Kind: alert.KindDeadline, Deadline: "09:00", Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "success"; f.Now = at(10, 0); return f }(),
			want: false,
		},
		{
			name: "时长超阈值命中",
			rule: alert.Rule{Kind: alert.KindDuration, Threshold: 60, Target: "*", Enabled: true},
			fact: func() alert.Fact {
				f := base
				f.StartedAt = at(2, 0)
				f.FinishedAt = at(3, 0)
				return f
			}(),
			want: true,
		},
		{
			name: "时长未超阈值不触发",
			rule: alert.Rule{Kind: alert.KindDuration, Threshold: 3600, Target: "*", Enabled: true},
			fact: func() alert.Fact {
				f := base
				f.StartedAt = at(2, 0)
				f.FinishedAt = at(3, 0)
				return f
			}(),
			want: false,
		},
		{
			name: "运行中不判断时长",
			rule: alert.Rule{Kind: alert.KindDuration, Threshold: 1, Target: "*", Enabled: true},
			fact: func() alert.Fact {
				f := base
				f.Status = "running"
				f.StartedAt = at(2, 0)
				f.FinishedAt = at(3, 0)
				return f
			}(),
			want: false,
		},
		{
			name: "零行命中",
			rule: alert.Rule{Kind: alert.KindRowCountZero, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 0; return f }(),
			want: true,
		},
		{
			name: "有行不报零行",
			rule: alert.Rule{Kind: alert.KindRowCountZero, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 1; return f }(),
			want: false,
		},
		{
			name: "量跌超阈值命中",
			rule: alert.Rule{Kind: alert.KindVolumeDrop, Threshold: 30, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 500; f.PrevRows = 1000; return f }(),
			want: true,
		},
		{
			name: "量跌未超阈值不触发",
			rule: alert.Rule{Kind: alert.KindVolumeDrop, Threshold: 60, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 500; f.PrevRows = 1000; return f }(),
			want: false,
		},
		{
			name: "无上期数据不比较",
			rule: alert.Rule{Kind: alert.KindVolumeDrop, Threshold: 1, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 0; f.PrevRows = 0; return f }(),
			want: false,
		},
		{
			name: "阈值为 0 时不比较（关闭规则）",
			rule: alert.Rule{Kind: alert.KindVolumeDrop, Threshold: 0, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 0; f.PrevRows = 1000; return f }(),
			want: false,
		},
		{
			name: "数据量上涨不触发",
			rule: alert.Rule{Kind: alert.KindVolumeDrop, Threshold: 10, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.RowsTotal = 2000; f.PrevRows = 1000; return f }(),
			want: false,
		},
		{
			name: "传感器超时命中",
			rule: alert.Rule{Kind: alert.KindSensorTimout, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Status = "failed"; f.Error = "sensor 读取超时"; return f }(),
			want: true,
		},
		{
			name: "只含 sensor 不触发",
			rule: alert.Rule{Kind: alert.KindSensorTimout, Target: "*", Enabled: true},
			fact: func() alert.Fact { f := base; f.Error = "sensor 正常"; return f }(),
			want: false,
		},
		{
			name: "未知类型不触发",
			rule: alert.Rule{Kind: alert.Kind("mystery"), Target: "*", Enabled: true},
			fact: base,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := alert.Evaluate(c.rule, c.fact)
			if got := ev != nil; got != c.want {
				t.Fatalf("触发=%v，期望 %v（event=%+v）", got, c.want, ev)
			}
		})
	}
}

// 守住默认严重级别：未填 severity 时必须是 warning，前端与路由依赖这个值。
func TestEvaluateDefaultSeverity(t *testing.T) {
	f := fact()
	f.Status = "failed"
	ev := alert.Evaluate(alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true}, f)
	if ev == nil || ev.Severity != "warning" {
		t.Fatalf("默认严重级别错误: %+v", ev)
	}
	ev = alert.Evaluate(alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true, Severity: "critical"}, f)
	if ev == nil || ev.Severity != "critical" {
		t.Fatalf("显式严重级别被覆盖: %+v", ev)
	}
}

// 守住去重键：规则+管道+业务日期+小时，跨小时必须换键，同小时必须稳定。
func TestEvaluateDedupeKey(t *testing.T) {
	f := fact()
	f.Status = "failed"
	r := alert.Rule{ID: "r1", Kind: alert.KindFailure, Target: "*", Enabled: true}
	ev1 := alert.Evaluate(r, f)
	ev2 := alert.Evaluate(r, f)
	if ev1 == nil || ev2 == nil {
		t.Fatal("应触发")
	}
	if ev1.DedupeKey != ev2.DedupeKey {
		t.Fatalf("同窗口去重键应稳定: %q vs %q", ev1.DedupeKey, ev2.DedupeKey)
	}
	want := "r1:dwd_order_daily:2025-11-09:2025111003"
	if ev1.DedupeKey != want {
		t.Fatalf("去重键格式错误: %q，期望 %q", ev1.DedupeKey, want)
	}
	f.Now = f.Now.Add(time.Hour)
	ev3 := alert.Evaluate(r, f)
	if ev3 == nil || ev3.DedupeKey == ev1.DedupeKey {
		t.Fatal("跨小时应产生新去重键")
	}
	if !ev1.At.Equal(time.Date(2025, 11, 10, 3, 0, 0, 0, time.Local)) {
		t.Fatalf("事件时间应取 Fact.Now: %v", ev1.At)
	}
}

// 守住长错误截断：超长错误不能把消息撑爆渠道限制。
func TestEvaluateTruncateLongError(t *testing.T) {
	f := fact()
	f.Status = "failed"
	f.Error = strings.Repeat("x", 400)
	ev := alert.Evaluate(alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true}, f)
	if ev == nil {
		t.Fatal("应触发")
	}
	if !strings.HasSuffix(ev.Detail, "...") {
		t.Fatalf("长错误应被截断: %q", ev.Detail)
	}
	// 300 字符 + "..."，再加上前面「业务日期 xxx，错误：」。
	if !strings.Contains(ev.Detail, strings.Repeat("x", 300)+"...") {
		t.Fatalf("截断长度不符: %d", len(ev.Detail))
	}
}

// 守住静默窗口：空配置、非法配置、同起止都不静默；跨零点要正确。
func TestInSilence(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2025, 11, 10, h, m, 0, 0, time.Local) }
	cases := []struct {
		name       string
		from, to   string
		h, m       int
		wantSilent bool
	}{
		{"未配置", "", "", 3, 0, false},
		{"只配置一半", "02:00", "", 3, 0, false},
		{"起止非法", "bad", "06:00", 3, 0, false},
		{"起止相同视为关闭", "03:00", "03:00", 3, 0, false},
		{"同日窗口内", "02:00", "06:00", 2, 0, true},
		{"同日窗口起点含", "02:00", "06:00", 2, 0, true},
		{"同日窗口终点不含", "02:00", "06:00", 6, 0, false},
		{"同日窗口外", "02:00", "06:00", 7, 0, false},
		{"跨零点深夜", "22:00", "06:00", 23, 0, true},
		{"跨零点凌晨", "22:00", "06:00", 2, 0, true},
		{"跨零点终点不含", "22:00", "06:00", 6, 0, false},
		{"跨零点白天", "22:00", "06:00", 12, 0, false},
		{"跨零点起点含", "22:00", "06:00", 22, 0, true},
		{"分钟边界", "02:30", "03:00", 2, 29, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := alert.InSilence(alert.Rule{SilenceFrom: c.from, SilenceTo: c.to}, at(c.h, c.m))
			if got != c.wantSilent {
				t.Fatalf("%02d:%02d 静默=%v，期望 %v", c.h, c.m, got, c.wantSilent)
			}
		})
	}
}

// 守住静默对 Evaluate 的短路：窗口内即使命中规则也不产出事件。
func TestEvaluateSuppressedBySilence(t *testing.T) {
	f := fact()
	f.Status = "failed"
	r := alert.Rule{Kind: alert.KindFailure, Target: "*", Enabled: true, SilenceFrom: "02:00", SilenceTo: "06:00"}
	if ev := alert.Evaluate(r, f); ev != nil {
		t.Fatalf("静默窗口内不应产出事件: %+v", ev)
	}
	f.Now = time.Date(2025, 11, 10, 7, 0, 0, 0, time.Local)
	if ev := alert.Evaluate(r, f); ev == nil {
		t.Fatal("窗口外应产出事件")
	}
}

// 守住默认渲染格式：模板为空时输出必须包含级别、标题、详情、管道与实例。
func TestRenderDefault(t *testing.T) {
	ev := alert.Event{
		Title: "T", Detail: "D", Pipeline: "p", InstanceID: "i", Severity: "critical",
	}
	out := alert.Render("", ev)
	for _, want := range []string{"[CRITICAL]", "T", "D", "管道：p", "实例：i"} {
		if !strings.Contains(out, want) {
			t.Fatalf("默认渲染缺少 %q: %q", want, out)
		}
	}
}

// 守住模板占位替换：所有占位符都要生效，未知占位保持原样。
func TestRenderTemplate(t *testing.T) {
	ev := alert.Event{
		Title: "T", Detail: "D", Pipeline: "p", InstanceID: "i", Severity: "warn",
	}
	tpl := "[${severity}] ${title} ${detail} ${pipeline} ${instance} ${unknown}"
	want := "[warn] T D p i ${unknown}"
	if got := alert.Render(tpl, ev); got != want {
		t.Fatalf("模板渲染 = %q，期望 %q", got, want)
	}
}

// 守住渠道名称：未显式命名时默认 webhook，供 Dispatcher 的 key 使用。
func TestWebhookChannelName(t *testing.T) {
	if got := (alert.WebhookChannel{}).Name(); got != "webhook" {
		t.Fatalf("默认名称 = %q", got)
	}
	if got := (alert.WebhookChannel{ChannelName: "feishu"}).Name(); got != "feishu" {
		t.Fatalf("自定义名称 = %q", got)
	}
}

// 守住 webhook 未配置：必须报错而不是发空请求。
func TestWebhookSendNoURL(t *testing.T) {
	err := (alert.WebhookChannel{}).Send(context.Background(), alert.Event{})
	if err == nil || !strings.Contains(err.Error(), "未配置 URL") {
		t.Fatalf("缺 URL 应报错: %v", err)
	}
}

// 守住 URL 非法：构造请求失败要返回错误，不能 panic。
func TestWebhookSendInvalidURL(t *testing.T) {
	w := alert.WebhookChannel{URL: "://bad-url"}
	if err := w.Send(context.Background(), alert.Event{}); err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// 守住正常推送：请求头是 JSON、body 同时含 text 与 event，且使用自定义模板。
func TestWebhookSendSuccess(t *testing.T) {
	var gotBody map[string]any
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ev := alert.Event{Title: "T", Detail: "D", Pipeline: "p", InstanceID: "i", Severity: "critical"}
	w := alert.WebhookChannel{URL: srv.URL, Template: "${title}/${pipeline}"}
	if err := w.Send(context.Background(), ev); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	if gotBody["msgtype"] != "text" {
		t.Fatalf("msgtype 错误: %#v", gotBody["msgtype"])
	}
	text, _ := gotBody["text"].(map[string]any)
	if text["content"] != "T/p" {
		t.Fatalf("text.content = %#v", text["content"])
	}
	if _, ok := gotBody["event"]; !ok {
		t.Fatal("body 应附带完整 event 供下游结构化消费")
	}
}

// 守住非 2xx：>=300 必须视为失败，否则渠道故障会被静默吞掉。
func TestWebhookSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	err := (alert.WebhookChannel{URL: srv.URL}).Send(context.Background(), alert.Event{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("502 应报错: %v", err)
	}
}

// 守住网络错误：连接失败要原样返回错误，便于上层标记渠道不可用。
func TestWebhookSendNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 关闭后再请求，必然失败
	if err := (alert.WebhookChannel{URL: url}).Send(context.Background(), alert.Event{}); err == nil {
		t.Fatal("连接失败应报错")
	}
}

type stubChannel struct {
	name string
	err  error
	sent int
}

func (s *stubChannel) Name() string { return s.name }
func (s *stubChannel) Send(ctx context.Context, ev alert.Event) error {
	s.sent++
	return s.err
}

type stubErr struct{}

func (stubErr) Error() string { return "boom" }

// 守住多渠道分发：单渠道失败不影响其它渠道，未配置的渠道单独标注。
func TestDispatcherSend(t *testing.T) {
	okCh := &stubChannel{name: "webhook"}
	badCh := &stubChannel{name: "email", err: stubErr{}}
	d := alert.NewDispatcher(okCh, badCh)

	res := d.Send(context.Background(), alert.Event{}, []string{"webhook", "email", "missing"})
	want := map[string]string{"webhook": "ok", "email": "boom", "missing": "渠道未配置"}
	for k, v := range want {
		if res[k] != v {
			t.Errorf("%s = %q，期望 %q", k, res[k], v)
		}
	}
	if okCh.sent != 1 || badCh.sent != 1 {
		t.Fatalf("每个渠道应各调用一次: ok=%d bad=%d", okCh.sent, badCh.sent)
	}
	// 空渠道列表不应报错。
	if got := d.Send(context.Background(), alert.Event{}, nil); len(got) != 0 {
		t.Fatalf("空列表应返回空 map: %#v", got)
	}
}
