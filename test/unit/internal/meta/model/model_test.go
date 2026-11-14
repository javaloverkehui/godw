package model_test

import (
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 分层校验是管道与模型校验的第一道闸门：多认一个非法值，
// 下游所有「按层排序/按层着色/按层引用」的假设都会失效。
func TestValidLayer(t *testing.T) {
	for _, l := range []string{"ods", "dwd", "dws", "ads", "dim"} {
		if !model.ValidLayer(l) {
			t.Fatalf("%s 应为合法分层", l)
		}
	}
	for _, l := range []string{"", "ODS", "ods ", "mid", "dm"} {
		if model.ValidLayer(l) {
			t.Fatalf("%q 不应为合法分层", l)
		}
	}
}

// 物理表名缺省必须回落到模型名，否则建表 SQL 会指向空表名。
func TestTableName(t *testing.T) {
	if got := (model.ModelSpec{Name: "dwd_order"}).TableName(); got != "dwd_order" {
		t.Fatalf("缺省表名 = %q", got)
	}
	if got := (model.ModelSpec{Name: "dwd_order", Table: "order_di"}).TableName(); got != "order_di" {
		t.Fatalf("显式表名 = %q", got)
	}
}

// 分级折算：新老两套写法都要认，未知值按 0 处理——
// 拼错的分级不能把全站导出锁死，但也不能被抬成高危。
func TestClassificationRank(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{model.ClassificationSecret, 3},
		{model.ClassificationHigh, 3},
		{"HIGH", 3},
		{"  secret  ", 3},
		{model.ClassificationConfidential, 2},
		{model.ClassificationMedium, 2},
		{model.ClassificationInternal, 1},
		{model.ClassificationPublic, 1},
		{model.ClassificationLow, 1},
		{"", 0},
		{"none", 0},
		{"confidentiall", 0},
	}
	for _, tc := range cases {
		if got := model.ClassificationRank(tc.in); got != tc.want {
			t.Fatalf("ClassificationRank(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// 展示名必须统一为 high/medium/low/none，前端只认这四个值。
func TestClassificationName(t *testing.T) {
	cases := map[int]string{3: "high", 2: "medium", 1: "low", 0: "none", 9: "none", -1: "none"}
	for rank, want := range cases {
		if got := model.ClassificationName(rank); got != want {
			t.Fatalf("ClassificationName(%d) = %q，期望 %q", rank, got, want)
		}
	}
}

// 节点类型白名单：漏登记会让合法节点在执行器里找不到实现。
func TestValidNodeType(t *testing.T) {
	for _, typ := range []string{model.NodeSource, model.NodeSQL, model.NodeQuality,
		model.NodeBranch, model.NodeSink, model.NodeShell, model.NodeHTTP} {
		if !model.ValidNodeType(typ) {
			t.Fatalf("%s 应为合法节点类型", typ)
		}
	}
	for _, typ := range []string{"", "select", "SQL"} {
		if model.ValidNodeType(typ) {
			t.Fatalf("%q 不应为合法节点类型", typ)
		}
	}
}

// 写入模式：空值表示继承默认值，必须放行；未知值必须拒绝。
func TestValidWriteMode(t *testing.T) {
	for _, m := range []string{"", model.WriteAppend, model.WriteOverwrite,
		model.WriteUpsert, model.WriteOverwritePartition} {
		if !model.ValidWriteMode(m) {
			t.Fatalf("%q 应为合法写入模式", m)
		}
	}
	for _, m := range []string{"merge", "UPSERT", "overwrite-all"} {
		if model.ValidWriteMode(m) {
			t.Fatalf("%q 不应为合法写入模式", m)
		}
	}
}
