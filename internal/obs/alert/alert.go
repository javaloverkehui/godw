// Package alert 实现 SLA 告警：规则判定、抑制聚合、渠道推送、状态流转。
//
// 设计取舍：告警的价值不在「发出去」，而在「不重复打扰且不漏报」。
// 因此三条机制是必需的：
//  1. 去重键：同管道同规则在同一窗口内只产生一条事件；
//  2. 静默窗口：维护期不发；
//  3. 状态机：open → acknowledged → resolved，处理过的不再重复提醒。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind 是告警类型。
type Kind string

const (
	KindDeadline     Kind = "sla_deadline"   // 未在 HH:mm 前完成
	KindDuration     Kind = "duration"       // 执行时长超阈值
	KindFailure      Kind = "failure"        // 执行失败
	KindRowCountZero Kind = "row_count_zero" // 产出为 0 行
	KindVolumeDrop   Kind = "volume_drop"    // 数据量环比下跌超阈值
	KindSensorTimout Kind = "sensor_timeout" // 传感器超时
)

// Rule 是一条告警规则。
type Rule struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspaceId,omitempty"`
	Name        string   `json:"name"`
	Kind        Kind     `json:"kind"`
	Target      string   `json:"target"` // 管道名或 *
	Threshold   float64  `json:"threshold"`
	Deadline    string   `json:"deadline,omitempty"` // HH:mm
	Severity    string   `json:"severity"`
	Channels    []string `json:"channels"`
	Enabled     bool     `json:"enabled"`
	SilenceFrom string   `json:"silenceFrom,omitempty"`
	SilenceTo   string   `json:"silenceTo,omitempty"`
}

// Fact 是被观察到的执行事实，用于规则判定。
type Fact struct {
	Pipeline   string    `json:"pipeline"`
	InstanceID string    `json:"instanceId"`
	Status     string    `json:"status"`
	BizDate    string    `json:"bizDate"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	RowsTotal  int64     `json:"rowsTotal"`
	PrevRows   int64     `json:"prevRows"`
	Error      string    `json:"error,omitempty"`
	Now        time.Time `json:"now"`
}

// Event 是一条告警事件。
type Event struct {
	RuleID     string    `json:"ruleId"`
	RuleName   string    `json:"ruleName"`
	Pipeline   string    `json:"pipeline"`
	InstanceID string    `json:"instanceId"`
	Severity   string    `json:"severity"`
	Title      string    `json:"title"`
	Detail     string    `json:"detail"`
	DedupeKey  string    `json:"dedupeKey"`
	At         time.Time `json:"at"`
}

// Evaluate 判定单条规则是否触发。返回 nil 表示不触发。
func Evaluate(rule Rule, f Fact) *Event {
	if !rule.Enabled {
		return nil
	}
	if rule.Target != "*" && rule.Target != "" && rule.Target != f.Pipeline {
		return nil
	}
	if InSilence(rule, f.Now) {
		return nil
	}

	var hit bool
	var title, detail string

	switch rule.Kind {
	case KindFailure:
		if f.Status == "failed" {
			hit = true
			title = fmt.Sprintf("%s 执行失败", f.Pipeline)
			detail = fmt.Sprintf("业务日期 %s，错误：%s", f.BizDate, truncate(f.Error, 300))
		}
	case KindDeadline:
		if rule.Deadline == "" {
			return nil
		}
		hh, mm, ok := parseHHMM(rule.Deadline)
		if !ok {
			return nil
		}
		deadline := time.Date(f.Now.Year(), f.Now.Month(), f.Now.Day(), hh, mm, 0, 0, f.Now.Location())
		if f.Status != "success" && f.Now.After(deadline) {
			hit = true
			title = fmt.Sprintf("%s 未在 %s 前完成", f.Pipeline, rule.Deadline)
			detail = fmt.Sprintf("当前状态 %s，已超过 SLA %s", f.Status, time.Since(deadline).Truncate(time.Minute))
		}
	case KindDuration:
		if f.Status == "success" || f.Status == "failed" {
			d := f.FinishedAt.Sub(f.StartedAt)
			if d > time.Duration(rule.Threshold)*time.Second {
				hit = true
				title = fmt.Sprintf("%s 执行时长超阈值", f.Pipeline)
				detail = fmt.Sprintf("耗时 %s，阈值 %.0fs", d.Truncate(time.Second), rule.Threshold)
			}
		}
	case KindRowCountZero:
		if f.Status == "success" && f.RowsTotal == 0 {
			hit = true
			title = fmt.Sprintf("%s 产出 0 行", f.Pipeline)
			detail = fmt.Sprintf("业务日期 %s 全部节点写入行数为 0", f.BizDate)
		}
	case KindVolumeDrop:
		if f.Status == "success" && f.PrevRows > 0 && rule.Threshold > 0 {
			drop := float64(f.PrevRows-f.RowsTotal) / float64(f.PrevRows) * 100
			if drop > rule.Threshold {
				hit = true
				title = fmt.Sprintf("%s 数据量下跌 %.1f%%", f.Pipeline, drop)
				detail = fmt.Sprintf("本期 %d 行，上期 %d 行，阈值 %.1f%%", f.RowsTotal, f.PrevRows, rule.Threshold)
			}
		}
	case KindSensorTimout:
		if strings.Contains(f.Error, "sensor") && strings.Contains(f.Error, "超时") {
			hit = true
			title = fmt.Sprintf("%s 传感器超时", f.Pipeline)
			detail = truncate(f.Error, 300)
		}
	}
	if !hit {
		return nil
	}

	sev := rule.Severity
	if sev == "" {
		sev = "warning"
	}
	return &Event{
		RuleID: rule.ID, RuleName: rule.Name, Pipeline: f.Pipeline, InstanceID: f.InstanceID,
		Severity: sev, Title: title, Detail: detail,
		// 去重键：规则 + 管道 + 业务日期 + 小时。
		// 小时粒度既能合并同一批抖动，又不会把第二天的同类问题吞掉。
		DedupeKey: fmt.Sprintf("%s:%s:%s:%s", rule.ID, f.Pipeline, f.BizDate, f.Now.Format("2006010215")),
		At:        f.Now,
	}
}

// InSilence 判断当前是否处于静默窗口。支持跨零点（22:00–06:00）。
func InSilence(rule Rule, now time.Time) bool {
	if rule.SilenceFrom == "" || rule.SilenceTo == "" {
		return false
	}
	fh, fm, ok1 := parseHHMM(rule.SilenceFrom)
	th, tm, ok2 := parseHHMM(rule.SilenceTo)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	from := fh*60 + fm
	to := th*60 + tm
	if from == to {
		return false
	}
	if from < to {
		return cur >= from && cur < to
	}
	// 跨零点
	return cur >= from || cur < to
}

func parseHHMM(s string) (int, int, bool) {
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil {
		return 0, 0, false
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// ---------- 渠道 ----------

// Channel 是一个推送渠道。
type Channel interface {
	Name() string
	Send(ctx context.Context, ev Event) error
}

// WebhookChannel 发 JSON 到任意 HTTP 端点（飞书/钉钉/企业微信都兼容这个形态）。
type WebhookChannel struct {
	ChannelName string
	URL         string
	// Template 可用 ${title} ${detail} ${pipeline} ${severity} 占位。
	Template string
	Client   *http.Client
}

func (w WebhookChannel) Name() string {
	if w.ChannelName != "" {
		return w.ChannelName
	}
	return "webhook"
}

func (w WebhookChannel) Send(ctx context.Context, ev Event) error {
	if w.URL == "" {
		return fmt.Errorf("webhook 未配置 URL")
	}
	text := Render(w.Template, ev)
	body, err := json.Marshal(map[string]any{
		// msgtype=text 是钉钉/企微通用格式；飞书用 text.content，额外带一份。
		"msgtype": "text",
		"text":    map[string]string{"content": text},
		"event":   ev,
	})
	if err != nil {
		return err
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 返回 %d", resp.StatusCode)
	}
	return nil
}

// Render 渲染告警文本。模板为空时用默认格式。
func Render(tpl string, ev Event) string {
	if tpl == "" {
		return fmt.Sprintf("[%s] %s\n%s\n管道：%s　实例：%s",
			strings.ToUpper(ev.Severity), ev.Title, ev.Detail, ev.Pipeline, ev.InstanceID)
	}
	r := strings.NewReplacer(
		"${title}", ev.Title,
		"${detail}", ev.Detail,
		"${pipeline}", ev.Pipeline,
		"${instance}", ev.InstanceID,
		"${severity}", ev.Severity,
	)
	return r.Replace(tpl)
}

// Dispatcher 把事件发到所有配置的渠道，单渠道失败不影响其它渠道。
type Dispatcher struct {
	channels map[string]Channel
}

func NewDispatcher(chs ...Channel) *Dispatcher {
	m := map[string]Channel{}
	for _, c := range chs {
		m[c.Name()] = c
	}
	return &Dispatcher{channels: m}
}

func (d *Dispatcher) Send(ctx context.Context, ev Event, names []string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		c, ok := d.channels[n]
		if !ok {
			out[n] = "渠道未配置"
			continue
		}
		if err := c.Send(ctx, ev); err != nil {
			out[n] = err.Error()
			continue
		}
		out[n] = "ok"
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
