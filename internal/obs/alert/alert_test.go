package alert

import (
	"context"
	"testing"
	"time"
)

func baseFact() Fact {
	now := time.Date(2025, 11, 10, 3, 0, 0, 0, time.Local)
	return Fact{
		Pipeline: "dwd_order_daily", InstanceID: "ins_1", Status: "success",
		BizDate: "2025-11-09", StartedAt: now.Add(-20 * time.Minute), FinishedAt: now,
		RowsTotal: 1000, PrevRows: 1000, Now: now,
	}
}

// T0512 验收：超时触发；重复告警合并；静默窗口内不发。
func TestFailure(t *testing.T) {
	f := baseFact()
	f.Status = "failed"
	f.Error = "连接超时"
	ev := Evaluate(Rule{Name: "失败即告警", Kind: KindFailure, Target: "*", Enabled: true, Severity: "critical"}, f)
	if ev == nil || ev.Severity != "critical" {
		t.Fatalf("失败应触发: %+v", ev)
	}
	// 目标不匹配
	if ev := Evaluate(Rule{Kind: KindFailure, Target: "other", Enabled: true}, f); ev != nil {
		t.Fatal("目标不匹配不应触发")
	}
}

func TestDeadline(t *testing.T) {
	f := baseFact()
	f.Status = "running"
	f.Now = time.Date(2025, 11, 10, 9, 30, 0, 0, time.Local)
	r := Rule{Kind: KindDeadline, Deadline: "09:00", Enabled: true, Target: "*"}

	ev := Evaluate(r, f)
	if ev == nil {
		t.Fatal("超 SLA 应触发")
	}
	f.Now = time.Date(2025, 11, 10, 8, 30, 0, 0, time.Local)
	if ev := Evaluate(r, f); ev != nil {
		t.Fatal("未到 deadline 不应触发")
	}
	// 已完成不触发
	f.Now = time.Date(2025, 11, 10, 9, 30, 0, 0, time.Local)
	f.Status = "success"
	if ev := Evaluate(r, f); ev != nil {
		t.Fatal("已成功不应触发")
	}
}

func TestDedupeKeyStable(t *testing.T) {
	f := baseFact()
	f.Status = "failed"
	r := Rule{ID: "r1", Kind: KindFailure, Target: "*", Enabled: true}
	ev1 := Evaluate(r, f)
	ev2 := Evaluate(r, f)
	if ev1.DedupeKey != ev2.DedupeKey {
		t.Fatal("同窗口内去重键必须一致，否则无法合并")
	}
	// 下一小时应产生新键
	f.Now = f.Now.Add(time.Hour)
	ev3 := Evaluate(r, f)
	if ev3.DedupeKey == ev1.DedupeKey {
		t.Fatal("跨小时应产生新去重键")
	}
}

func TestSilence(t *testing.T) {
	f := baseFact()
	f.Status = "failed"
	r := Rule{
		Kind: KindFailure, Target: "*", Enabled: true,
		SilenceFrom: "02:00", SilenceTo: "06:00",
	}
	// f.Now 是 03:00，落在静默窗口
	if ev := Evaluate(r, f); ev != nil {
		t.Fatal("静默窗口内不应发送")
	}
	f.Now = time.Date(2025, 11, 10, 7, 0, 0, 0, time.Local)
	if ev := Evaluate(r, f); ev == nil {
		t.Fatal("静默窗口外应发送")
	}
}

func TestSilenceCrossMidnight(t *testing.T) {
	// 22:00–06:00
	r := Rule{SilenceFrom: "22:00", SilenceTo: "06:00"}
	for _, c := range []struct {
		h    int
		want bool
	}{{23, true}, {2, true}, {6, false}, {12, false}, {22, true}, {21, false}} {
		now := time.Date(2025, 11, 10, c.h, 0, 0, 0, time.Local)
		if got := InSilence(r, now); got != c.want {
			t.Fatalf("%02d:00 静默判定 %v，期望 %v", c.h, got, c.want)
		}
	}
}

func TestVolumeDrop(t *testing.T) {
	f := baseFact()
	f.RowsTotal = 500
	f.PrevRows = 1000
	r := Rule{Kind: KindVolumeDrop, Target: "*", Threshold: 30, Enabled: true}
	ev := Evaluate(r, f)
	if ev == nil {
		t.Fatal("下跌 50% 超过阈值 30% 应触发")
	}
	r.Threshold = 60
	if ev := Evaluate(r, f); ev != nil {
		t.Fatal("未超阈值不应触发")
	}
}

func TestRowCountZero(t *testing.T) {
	f := baseFact()
	f.RowsTotal = 0
	if ev := Evaluate(Rule{Kind: KindRowCountZero, Target: "*", Enabled: true}, f); ev == nil {
		t.Fatal("0 行应触发")
	}
	f.Status = "failed"
	if ev := Evaluate(Rule{Kind: KindRowCountZero, Target: "*", Enabled: true}, f); ev != nil {
		t.Fatal("失败场景由 failure 规则覆盖，不应重复报 0 行")
	}
}

func TestRender(t *testing.T) {
	ev := Event{Title: "T", Detail: "D", Pipeline: "p", InstanceID: "i", Severity: "critical"}
	out := Render("[${severity}] ${title} / ${pipeline}", ev)
	if out != "[critical] T / p" {
		t.Fatalf("模板渲染: %s", out)
	}
	if Render("", ev) == "" {
		t.Fatal("默认模板不应为空")
	}
}

type fakeChannel struct {
	name string
	fail bool
	sent int
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Send(ctx context.Context, ev Event) error {
	f.sent++
	if f.fail {
		return errFake
	}
	return nil
}

type fakeErr struct{}

func (fakeErr) Error() string { return "boom" }

var errFake = fakeErr{}

func TestDispatcherMultiChannel(t *testing.T) {
	ok := &fakeChannel{name: "webhook"}
	bad := &fakeChannel{name: "email", fail: true}
	d := NewDispatcher(ok, bad)
	res := d.Send(context.Background(), Event{}, []string{"webhook", "email", "missing"})
	if res["webhook"] != "ok" || res["email"] != "boom" || res["missing"] == "ok" {
		t.Fatalf("多渠道结果异常: %+v", res)
	}
	if ok.sent != 1 {
		t.Fatal("成功渠道应被调用")
	}
}
