package compile_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/pkg/errs"
)

// 严格解析的意义：字段名拼错必须报错。早先宽松解析让 `depends_on` 写成
// `depend_on` 时静默生效，管道跑到一半才因为缺依赖失败。
func TestParse(t *testing.T) {
	t.Run("合法", func(t *testing.T) {
		spec, err := compile.Parse([]byte("kind: Pipeline\nname: p\nnodes:\n  - id: a\n    type: source\n    sql: select 1\n"))
		if err != nil {
			t.Fatal(err)
		}
		if spec.Name != "p" || len(spec.Nodes) != 1 || spec.Nodes[0].ID != "a" {
			t.Fatalf("解析结果错误: %+v", spec)
		}
	})
	t.Run("kind 缺省允许", func(t *testing.T) {
		if _, err := compile.Parse([]byte("name: p\nnodes: []\n")); err != nil {
			t.Fatalf("kind 为空应允许: %v", err)
		}
	})
	t.Run("kind 非法", func(t *testing.T) {
		_, err := compile.Parse([]byte("kind: Model\nname: p\nnodes: []\n"))
		if !errs.Is(err, errs.CodePipelineInvalid) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("未知字段", func(t *testing.T) {
		_, err := compile.Parse([]byte("name: p\nnodes: []\nnope: 1\n"))
		if !errs.Is(err, errs.CodePipelineInvalid) {
			t.Fatalf("未知字段必须报错: %v", err)
		}
	})
	t.Run("YAML 非法", func(t *testing.T) {
		_, err := compile.Parse([]byte("nodes: [\n"))
		if !errs.Is(err, errs.CodePipelineInvalid) {
			t.Fatalf("YAML 语法错误应包装为管道错误: %v", err)
		}
	})
}

func node(id, typ string) model.NodeSpec {
	return model.NodeSpec{ID: id, Type: typ, SQL: "select 1"}
}

func problems(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	ae, ok := err.(*errs.AppError)
	if !ok {
		t.Fatalf("应为 AppError: %T", err)
	}
	list, _ := ae.Meta["problems"].([]string)
	return strings.Join(list, "\n")
}

// 校验必须一次列出全部问题，且覆盖每类节点/边的约束。
func TestValidateProblems(t *testing.T) {
	valid := &model.PipelineSpec{
		Name: "ok",
		Nodes: []model.NodeSpec{
			{ID: "src", Type: model.NodeSource, SQL: "select 1"},
			{ID: "sql", Type: model.NodeSQL, SQL: "select * from t where dt = '${biz_date}'", DependsOn: []string{"src"}},
			{ID: "q", Type: model.NodeQuality, Model: "m", DependsOn: []string{"sql"}},
			{ID: "out", Type: model.NodeSink, Config: map[string]any{"path": "/tmp/x"}, DependsOn: []string{"q"}},
		},
	}
	if err := compile.Validate(valid); err != nil {
		t.Fatalf("合法管道不应报错: %v", err)
	}

	cases := []struct {
		name string
		spec *model.PipelineSpec
		want string
	}{
		{"name 为空", &model.PipelineSpec{Nodes: []model.NodeSpec{node("a", model.NodeSQL)}}, "name 不能为空"},
		{"nodes 为空", &model.PipelineSpec{Name: "p"}, "nodes 不能为空"},
		{"节点 ID 为空", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{{Type: model.NodeSQL, SQL: "s"}}}, "nodes[0].id 不能为空"},
		{"ID 重复", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{node("a", model.NodeSQL), node("a", model.NodeSQL)}}, "节点 ID 重复: a"},
		{"类型不支持", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{node("a", "unknown")}}, "不受支持"},
		{"写入模式不支持", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "s", WriteMode: "bad"}}}, "write_mode"},
		{"source 缺 SQL", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{{ID: "a", Type: model.NodeSource}}}, "缺少 sql"},
		{"sql 节点缺 SQL", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{{ID: "a", Type: model.NodeSQL, SQL: "  "}}}, "缺少 sql"},
		{"质量节点缺 model", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{{ID: "a", Type: model.NodeQuality}}}, "必须指定 model"},
		{"输出节点缺 model 与 path", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{{ID: "a", Type: model.NodeSink}}}, "需指定 model 或 config.path"},
		{"自依赖", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "s", DependsOn: []string{"a"}}}}, "依赖自身"},
		{"依赖不存在", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "s", DependsOn: []string{"missing"}}}}, "依赖不存在的节点 missing"},
		{"环", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "s", DependsOn: []string{"b"}},
			{ID: "b", Type: model.NodeSQL, SQL: "s", DependsOn: []string{"a"}},
		}}, "检测到环"},
		{"未声明参数", &model.PipelineSpec{Name: "p", Nodes: []model.NodeSpec{
			{ID: "a", Type: model.NodeSQL, SQL: "select ${nope}"}}}, "未声明的参数 ${nope}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compile.Validate(tc.spec)
			if err == nil {
				t.Fatal("应报错")
			}
			if !errs.Is(err, errs.CodePipelineInvalid) {
				t.Fatalf("错误码错误: %v", err)
			}
			if got := problems(t, err); !strings.Contains(got, tc.want) {
				t.Fatalf("问题列表缺少 %q:\n%s", tc.want, got)
			}
		})
	}
}

// 已声明参数与内置参数（biz_date/biz_ts）必须放行，否则正常管道全部无法发布。
func TestValidateDeclaredParams(t *testing.T) {
	spec := &model.PipelineSpec{
		Name:   "p",
		Params: []model.ParamSpec{{Name: "window", Type: "int"}},
		Nodes: []model.NodeSpec{{
			ID: "a", Type: model.NodeSQL,
			SQL: "select * from t where dt = ${biz_date} and ts < ${biz_ts} and n = ${window}",
		}},
	}
	if err := compile.Validate(spec); err != nil {
		t.Fatalf("已声明参数不应报错: %v", err)
	}
}

// 层级计算：无依赖为 0，其余为依赖最大层 +1；环不能导致栈溢出。
func TestLevelTopoStages(t *testing.T) {
	nodes := []model.NodeSpec{
		{ID: "d", DependsOn: []string{"c"}},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a", "b"}},
		{ID: "a"},
	}
	lv := compile.Level(nodes)
	want := map[string]int{"a": 0, "b": 1, "c": 2, "d": 3}
	for id, w := range want {
		if lv[id] != w {
			t.Fatalf("%s 层级 = %d，期望 %d", id, lv[id], w)
		}
	}
	topo := compile.Topo(nodes)
	got := make([]string, len(topo))
	for i, n := range topo {
		got[i] = n.ID
	}
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("拓扑序错误: %v", got)
	}
	stages := compile.Stages(nodes)
	if len(stages) != 4 {
		t.Fatalf("层数错误: %d", len(stages))
	}
	for i, s := range stages {
		if len(s) != 1 || s[0].ID != got[i : i+1][0] {
			t.Fatalf("第 %d 层错误: %+v", i, s)
		}
	}
}

// 同层多个节点按 ID 排序，保证快照可比。
func TestTopoStableSort(t *testing.T) {
	nodes := []model.NodeSpec{{ID: "z"}, {ID: "a"}, {ID: "m"}}
	topo := compile.Topo(nodes)
	if topo[0].ID != "a" || topo[1].ID != "m" || topo[2].ID != "z" {
		t.Fatalf("同层排序错误: %+v", topo)
	}
	empty := compile.Stages(nil)
	if len(empty) != 1 || len(empty[0]) != 0 {
		t.Fatalf("空管道的层结构错误: %+v", empty)
	}
}

// 环存在时 Level 必须返回而不是死循环。
func TestLevelCycle(t *testing.T) {
	lv := compile.Level([]model.NodeSpec{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	})
	if len(lv) != 2 {
		t.Fatalf("层级结果错误: %+v", lv)
	}
	self := compile.Level([]model.NodeSpec{{ID: "s", DependsOn: []string{"s"}}})
	if self["s"] != 1 {
		t.Fatalf("自依赖层级错误: %+v", self)
	}
}

// 渲染：空 SQL 跳过；缺变量返回编译错误；正常变量要变成字面量。
func TestRender(t *testing.T) {
	spec := &model.PipelineSpec{Nodes: []model.NodeSpec{
		{ID: "a", SQL: "select * from t where dt = ${biz_date}"},
		{ID: "b", SQL: "   "},
	}}
	out, err := compile.Render(spec, map[string]any{"biz_date": "2024-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("空 SQL 节点不应出现在结果中: %+v", out)
	}
	if !strings.Contains(out["a"], "'2024-01-01'") {
		t.Fatalf("变量渲染错误: %s", out["a"])
	}

	bad := &model.PipelineSpec{Nodes: []model.NodeSpec{{ID: "a", SQL: "select ${missing}"}}}
	_, err = compile.Render(bad, nil)
	if !errs.Is(err, errs.CodeCompileFailed) {
		t.Fatalf("缺变量应返回编译错误: %v", err)
	}
	if !strings.Contains(err.Error(), "节点 a") {
		t.Fatalf("错误应带节点 ID: %v", err)
	}
}

func TestUpstream(t *testing.T) {
	nodes := []model.NodeSpec{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}}
	if got := compile.Upstream(nodes, "b"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("上游错误: %v", got)
	}
	if got := compile.Upstream(nodes, "missing"); got != nil {
		t.Fatalf("未知节点应返回 nil: %v", got)
	}
}

func modelWith(cols ...model.ColumnSpec) model.ModelSpec {
	return model.ModelSpec{Name: "dwd_order", Columns: cols}
}

// 增量推导的顺序是硬约束：显式条件 > 分区列 > 时间列 > 报错。
// 顺序颠倒会带来静默的全表扫描，比报错危险得多。
func TestInferIncremental(t *testing.T) {
	t.Run("显式条件优先", func(t *testing.T) {
		inf, err := compile.InferIncremental(modelWith(), "bad-date", "  dt = '2024-01-01'  ")
		if err != nil {
			t.Fatal(err)
		}
		if inf.Kind != compile.InferExplicit || inf.Where != "dt = '2024-01-01'" {
			t.Fatalf("显式条件处理错误: %+v", inf)
		}
	})
	t.Run("日期非法", func(t *testing.T) {
		_, err := compile.InferIncremental(modelWith(), "2024/01/01", "")
		if !errs.Is(err, errs.CodeInvalidArgument) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("分区列", func(t *testing.T) {
		spec := modelWith(model.ColumnSpec{Name: "dt", Type: "date"})
		spec.PartitionBy = []string{"dt"}
		inf, err := compile.InferIncremental(spec, "2024-01-01", "")
		if err != nil {
			t.Fatal(err)
		}
		if inf.Kind != compile.InferPartition || inf.Column != "dt" || inf.Where != "dt = '2024-01-01'" {
			t.Fatalf("分区推导错误: %+v", inf)
		}
		if len(inf.ScannedPartitions) != 1 || inf.ScannedPartitions[0] != "2024-01-01" {
			t.Fatalf("扫描分区错误: %+v", inf.ScannedPartitions)
		}
	})
	t.Run("分区表达式", func(t *testing.T) {
		cases := []string{"toYYYYMM(dt)", "date_trunc('month', dt)", `date_trunc("day", "dt")`}
		for _, expr := range cases {
			spec := modelWith(model.ColumnSpec{Name: "dt", Type: "date"})
			spec.PartitionBy = []string{expr}
			inf, err := compile.InferIncremental(spec, "2024-01-01", "")
			if err != nil {
				t.Fatalf("%s: %v", expr, err)
			}
			if inf.Column != "dt" || inf.Kind != compile.InferPartition {
				t.Fatalf("%s 分区列解析错误: %+v", expr, inf)
			}
		}
	})
	t.Run("分区列不存在", func(t *testing.T) {
		spec := modelWith(model.ColumnSpec{Name: "id", Type: "bigint"})
		spec.PartitionBy = []string{"dt"}
		_, err := compile.InferIncremental(spec, "2024-01-01", "")
		if !errs.Is(err, errs.CodeModelInvalid) {
			t.Fatalf("错误码错误: %v", err)
		}
	})
	t.Run("时间列", func(t *testing.T) {
		spec := modelWith(model.ColumnSpec{Name: "Updated_At", Type: "datetime"})
		inf, err := compile.InferIncremental(spec, "2024-01-01", "")
		if err != nil {
			t.Fatal(err)
		}
		if inf.Kind != compile.InferTimeColumn || inf.Column != "updated_at" {
			t.Fatalf("时间列推导错误: %+v", inf)
		}
		want := "updated_at >= '2024-01-01' AND updated_at < '2024-01-02'"
		if inf.Where != want {
			t.Fatalf("半开区间错误: %s", inf.Where)
		}
		if len(inf.Warn) == 0 {
			t.Fatal("时间列推导应给出性能告警")
		}
	})
	t.Run("候选时间列全量覆盖", func(t *testing.T) {
		for _, cand := range []string{"updated_at", "update_time", "modified_at", "gmt_modified",
			"created_at", "create_time", "gmt_create", "event_time", "ts"} {
			spec := modelWith(model.ColumnSpec{Name: cand, Type: "datetime"})
			inf, err := compile.InferIncremental(spec, "2024-01-01", "")
			if err != nil || inf.Column != cand {
				t.Fatalf("%s 未命中: %+v %v", cand, inf, err)
			}
		}
	})
	t.Run("无法推导必须报错", func(t *testing.T) {
		_, err := compile.InferIncremental(modelWith(model.ColumnSpec{Name: "id", Type: "bigint"}), "2024-01-01", "")
		if !errs.Is(err, errs.CodeModelInvalid) {
			t.Fatalf("错误码错误: %v", err)
		}
		ae := err.(*errs.AppError)
		if ae.Meta["hint"] == nil || ae.Meta["candidates"] == nil {
			t.Fatalf("错误应带修复提示: %+v", ae.Meta)
		}
	})
	t.Run("空分区表达式回退时间列", func(t *testing.T) {
		spec := modelWith(model.ColumnSpec{Name: "dt", Type: "date"}, model.ColumnSpec{Name: "updated_at", Type: "datetime"})
		spec.PartitionBy = []string{"  "}
		inf, err := compile.InferIncremental(spec, "2024-01-01", "")
		if err != nil {
			t.Fatal(err)
		}
		if inf.Kind != compile.InferTimeColumn {
			t.Fatalf("空分区表达式应回退时间列: %+v", inf)
		}
	})
}

// 条件注入必须包一层子查询：直接改 WHERE 会破坏 GROUP BY/UNION/LIMIT。
func TestApplyWhere(t *testing.T) {
	if got := compile.ApplyWhere("select 1", compile.Inference{}); got != "select 1" {
		t.Fatalf("空条件不应改写 SQL: %s", got)
	}
	inf := compile.Inference{Where: "dt = '2024-01-01'"}
	got := compile.ApplyWhere("SELECT a FROM t;\n", inf)
	want := "SELECT * FROM (\nSELECT a FROM t\n) godw_incremental WHERE dt = '2024-01-01'"
	if got != want {
		t.Fatalf("注入结果错误:\n%s", got)
	}
}

// 发布前静态检查：全表扫描与可空分区列都必须告警，
// 否则 NULL 行会永远不被增量覆盖。
func TestValidateIncremental(t *testing.T) {
	spec := modelWith(model.ColumnSpec{Name: "dt", Type: "date", Nullable: true})
	cases := []struct {
		name string
		inf  compile.Inference
		want int
	}{
		{"全量", compile.Inference{Kind: compile.InferNone}, 1},
		{"条件为空", compile.Inference{Kind: compile.InferExplicit}, 1},
		{"时间列告警", compile.Inference{Kind: compile.InferTimeColumn, Where: "x", Warn: []string{"慢"}}, 1},
		{"分区列可空", compile.Inference{Kind: compile.InferPartition, Column: "dt", Where: "x"}, 1},
		{"分区列非空", compile.Inference{Kind: compile.InferPartition, Column: "id", Where: "x"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compile.ValidateIncremental(spec, tc.inf); len(got) != tc.want {
				t.Fatalf("告警数 = %d，期望 %d: %v", len(got), tc.want, got)
			}
		})
	}
}
