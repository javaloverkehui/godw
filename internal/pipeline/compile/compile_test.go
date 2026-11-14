package compile

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

const sample = `
kind: Pipeline
name: demo
schedule: "0 2 * * *"
nodes:
  - id: a
    type: source
    datasource: pg
    sql: SELECT 1
  - id: b
    type: sql
    depends_on: [a]
    model: dwd_x
    sql: SELECT * FROM t WHERE dt = '${biz_date}'
  - id: c
    type: sql
    depends_on: [a]
    model: dws_x
    sql: SELECT count(*) FROM b WHERE dt = '${biz_date}'
  - id: d
    type: sql
    depends_on: [b, c]
    model: ads_x
    sql: SELECT 1
`

func TestParseAndValidate(t *testing.T) {
	spec, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(spec); err != nil {
		t.Fatal(err)
	}
	if spec.Name != "demo" || len(spec.Nodes) != 4 {
		t.Fatalf("解析结果异常: %+v", spec)
	}
}

func TestLevels(t *testing.T) {
	spec, _ := Parse([]byte(sample))
	lv := Level(spec.Nodes)
	want := map[string]int{"a": 0, "b": 1, "c": 1, "d": 2}
	for k, v := range want {
		if lv[k] != v {
			t.Errorf("%s level = %d, want %d", k, lv[k], v)
		}
	}
	stages := Stages(spec.Nodes)
	if len(stages) != 3 {
		t.Fatalf("应有 3 层，实际 %d", len(stages))
	}
	if len(stages[1]) != 2 {
		t.Errorf("第二层应可并发 2 个节点，实际 %d", len(stages[1]))
	}
}

func TestCycleDetected(t *testing.T) {
	spec := &model.PipelineSpec{
		Name: "cyc",
		Nodes: []model.NodeSpec{
			{ID: "a", Type: "sql", SQL: "select 1", DependsOn: []string{"b"}},
			{ID: "b", Type: "sql", SQL: "select 1", DependsOn: []string{"a"}},
		},
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("环应被检出")
	}
	if !strings.Contains(err.Error(), "环") {
		t.Errorf("错误信息应说明环: %v", err)
	}
}

func TestUnknownDepAndParam(t *testing.T) {
	spec := &model.PipelineSpec{
		Name: "bad",
		Nodes: []model.NodeSpec{
			{ID: "a", Type: "sql", SQL: "select ${undefined_var}", DependsOn: []string{"ghost"}},
		},
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("应校验失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "不存在的节点") {
		t.Errorf("缺少依赖检查: %s", msg)
	}
	if !strings.Contains(msg, "undefined_var") {
		t.Errorf("缺少未声明参数检查: %s", msg)
	}
}

// 未知字段必须报错，避免 YAML 拼写错误静默生效。
func TestParseStrict(t *testing.T) {
	_, err := Parse([]byte("kind: Pipeline\nname: x\nnode:\n  - id: a\n"))
	if err == nil {
		t.Fatal("未知字段应报错")
	}
}

func TestTopoStable(t *testing.T) {
	spec, _ := Parse([]byte(sample))
	first := Topo(spec.Nodes)
	second := Topo(spec.Nodes)
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatal("拓扑排序必须稳定")
		}
	}
	if first[0].ID != "a" {
		t.Errorf("首节点应为 a，实际 %s", first[0].ID)
	}
}
