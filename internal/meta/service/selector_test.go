package service

import (
	"strings"
	"testing"
)

func selFixtures() ([]SelectModel, []SelectEdge, map[string][]string) {
	models := []SelectModel{
		{Name: "ods_order", Layer: "ods", Tags: []string{"core"}},
		{Name: "dwd_order", Layer: "dwd", Tags: []string{"core"}},
		{Name: "dws_order_daily", Layer: "dws"},
		{Name: "ads_order_dashboard", Layer: "ads"},
		{Name: "dwd_user", Layer: "dwd"},
	}
	edges := []SelectEdge{
		{Up: "ods_order", Down: "dwd_order"},
		{Up: "dwd_order", Down: "dws_order_daily"},
		{Up: "dws_order_daily", Down: "ads_order_dashboard"},
	}
	pipelines := map[string][]string{
		"order_daily": {"dwd_order", "dws_order_daily", "ads_order_dashboard"},
		"user_daily":  {"dwd_user"},
	}
	return models, edges, pipelines
}

func expand(t *testing.T, expr string) []string {
	t.Helper()
	terms, err := ParseSelect(expr)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", expr, err)
	}
	m, e, p := selFixtures()
	got, err := ExpandSelect(terms, m, e, p)
	if err != nil {
		t.Fatalf("展开 %q 失败: %v", expr, err)
	}
	return got
}

func wantSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
}

func TestExpandSelect(t *testing.T) {
	// 单个模型：只有自己。
	wantSet(t, expand(t, "dwd_order"), "dwd_order")
	// 下游闭包：订单明细之后的三层。
	wantSet(t, expand(t, "dwd_order+"), "ads_order_dashboard", "dwd_order", "dws_order_daily")
	// 上游闭包：从 DWS 一路回到 ODS。
	wantSet(t, expand(t, "+dws_order_daily"), "dwd_order", "dws_order_daily", "ods_order")
	// 双向。
	wantSet(t, expand(t, "+dwd_order+"),
		"ads_order_dashboard", "dwd_order", "dws_order_daily", "ods_order")
}

func TestExpandSelectByTagLayerPipeline(t *testing.T) {
	wantSet(t, expand(t, "tag:core"), "dwd_order", "ods_order")
	wantSet(t, expand(t, "layer:dwd"), "dwd_order", "dwd_user")
	// pipeline: 只返回该管道写出的模型。
	wantSet(t, expand(t, "pipeline:order_daily"),
		"ads_order_dashboard", "dwd_order", "dws_order_daily")
}

func TestExpandSelectUnionAndGlob(t *testing.T) {
	wantSet(t, expand(t, "dwd_user,dwd_order"), "dwd_order", "dwd_user")
	wantSet(t, expand(t, "layer:ads,dwd_user"), "ads_order_dashboard", "dwd_user")
	wantSet(t, expand(t, "dwd_*"), "dwd_order", "dwd_user")
	// 空格分隔等价于逗号。
	wantSet(t, expand(t, "dwd_user dwd_order"), "dwd_order", "dwd_user")
}

func TestExpandSelectErrors(t *testing.T) {
	m, e, p := selFixtures()
	terms, _ := ParseSelect("nope")
	if _, err := ExpandSelect(terms, m, e, p); err == nil {
		t.Fatal("不存在的模型应报错")
	}
	terms, _ = ParseSelect("layer:zzz")
	if _, err := ExpandSelect(terms, m, e, p); err == nil {
		t.Fatal("空分层应报错")
	}
	if _, err := ParseSelect("db:prod"); err == nil {
		t.Fatal("未知前缀应报错")
	}
	if _, err := ParseSelect("+"); err == nil {
		t.Fatal("只有加号应报错")
	}
	if _, err := ParseSelect("  "); err == nil {
		t.Fatal("空表达式应报错")
	}
}
