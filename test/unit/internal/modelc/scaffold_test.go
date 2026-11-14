package modelc_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
)

func hasWarning(list []string, sub string) bool {
	for _, w := range list {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func columnNames(cols []model.ColumnSpec) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return strings.Join(out, ",")
}

// 脚手架的目标是「天生合规、改两行就能用」：
// 名称前缀、分区列、审计列、主键候选都必须预置好，否则用户第一步就卡住。
func TestScaffoldBasics(t *testing.T) {
	t.Run("非法层", func(t *testing.T) {
		if _, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "mid", Name: "x"}); err == nil {
			t.Fatal("非法层应报错")
		}
	})
	t.Run("缺名称", func(t *testing.T) {
		if _, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd"}); err == nil {
			t.Fatal("缺名称应报错")
		}
	})
	t.Run("自动补前缀", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "order"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Name != "dwd_order" || got.Spec.Layer != "dwd" {
			t.Fatalf("名称错误: %+v", got.Spec)
		}
		// 缺省数据源与 schema。
		if got.Spec.Datasource != "pg_dw" || got.Spec.Schema != "dwd" {
			t.Fatalf("缺省值错误: %+v", got.Spec)
		}
		// 无上游时补 dt 分区列，并提示无法推断主键。
		if got.Spec.PartitionBy[0] != "dt" || got.Spec.Columns[0].Name != "dt" {
			t.Fatalf("分区列错误: %+v", got.Spec)
		}
		if !hasWarning(got.Warnings, "未能推断主键") {
			t.Fatalf("缺少主键提示: %v", got.Warnings)
		}
		if !strings.Contains(got.YAML, "name: dwd_order") {
			t.Fatalf("YAML 输出错误:\n%s", got.YAML)
		}
	})
	t.Run("ods 前缀被替换", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "ods_order"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Name != "dwd_order" {
			t.Fatalf("名称错误: %s", got.Spec.Name)
		}
	})
	t.Run("source_name 作为兜底名称", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", SourceName: "order"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Name != "dwd_order" {
			t.Fatalf("名称错误: %s", got.Spec.Name)
		}
	})
	t.Run("mart 标签", func(t *testing.T) {
		for _, layer := range []string{"dws", "ads"} {
			got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: layer, Name: "sales"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Spec.Tags) != 1 || got.Spec.Tags[0] != "mart" {
				t.Fatalf("%s 应打 mart 标签: %+v", layer, got.Spec.Tags)
			}
		}
	})
}

// 上游结构归一：列名清洗、类型归一、重名列跳过、源表主键与日期列沿用。
// 这四件事任一错位，生成的 YAML 都不能直接用。
func TestScaffoldUpstream(t *testing.T) {
	upstream := []model.ColumnSpec{
		{Name: "ID", Type: "int8", PrimaryKey: true},
		{Name: "id", Type: "int8"}, // 归一后重名
		{Name: "Amount", Type: "NUMERIC(10,2)"},
		{Name: "Order Date", Type: "date"},
	}
	got, err := modelc.Scaffold(modelc.ScaffoldOptions{
		Layer: "dwd", Name: "order", Upstream: upstream, BizDateColumn: "order_date",
	})
	if err != nil {
		t.Fatal(err)
	}
	if columnNames(got.Spec.Columns) != "id,amount,order_date" {
		t.Fatalf("列错误: %v", columnNames(got.Spec.Columns))
	}
	if got.Spec.Columns[0].Type != "bigint" || got.Spec.Columns[1].Type != "decimal(20,4)" {
		t.Fatalf("类型归一错误: %+v", got.Spec.Columns)
	}
	if !hasWarning(got.Warnings, "重名列") {
		t.Fatalf("重名列应告警: %v", got.Warnings)
	}
	// 源表声明的主键直接沿用，不需要用户再回答一次。
	if len(got.Spec.PrimaryKey) != 1 || got.Spec.PrimaryKey[0] != "id" {
		t.Fatalf("主键错误: %v", got.Spec.PrimaryKey)
	}
	if len(got.Spec.UniqueKey) != 1 || got.Spec.UniqueKey[0] != "id" {
		t.Fatalf("唯一键应跟随主键: %v", got.Spec.UniqueKey)
	}
	if got.Spec.PartitionBy[0] != "order_date" {
		t.Fatalf("分区列错误: %v", got.Spec.PartitionBy)
	}
	if got.KeyCandidates[0] != "id" {
		t.Fatalf("主键候选应把源表主键排最前: %v", got.KeyCandidates)
	}
}

// 分区列推导：显式指定 > 上游日期列 > 补 dt。
func TestScaffoldPartition(t *testing.T) {
	t.Run("自动识别日期列", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
			Upstream: []model.ColumnSpec{{Name: "biz_dt", Type: "timestamp"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.PartitionBy[0] != "biz_dt" {
			t.Fatalf("应选首个日期列: %v", got.Spec.PartitionBy)
		}
	})
	t.Run("显式日期列不存在时补列", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
			BizDateColumn: "stat_date", Upstream: []model.ColumnSpec{{Name: "id", Type: "bigint"}}})
		if err != nil {
			t.Fatal(err)
		}
		if columnNames(got.Spec.Columns) != "stat_date,id" {
			t.Fatalf("分区列应补在最前: %v", columnNames(got.Spec.Columns))
		}
		if got.Spec.Columns[0].Comment != "分区日期" || got.Spec.Columns[0].Nullable {
			t.Fatalf("补列属性错误: %+v", got.Spec.Columns[0])
		}
	})
}

// 主键推断：显式指定 > 源表声明 > 列名启发式；推不出来必须告警。
func TestScaffoldKeys(t *testing.T) {
	t.Run("显式指定优先", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
			PrimaryKey: []string{"amount"},
			Upstream: []model.ColumnSpec{
				{Name: "id", Type: "bigint", PrimaryKey: true},
				{Name: "amount", Type: "bigint"},
			}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Spec.PrimaryKey) != 1 || got.Spec.PrimaryKey[0] != "amount" {
			t.Fatalf("主键错误: %v", got.Spec.PrimaryKey)
		}
	})
	t.Run("列名启发式", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
			Upstream: []model.ColumnSpec{
				{Name: "tenant_id", Type: "bigint"},
				{Name: "name", Type: "string"},
			}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Spec.PrimaryKey) != 1 || got.Spec.PrimaryKey[0] != "tenant_id" {
			t.Fatalf("启发式主键错误: %v", got.Spec.PrimaryKey)
		}
	})
	t.Run("候选包含 _no 与可空列", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
			Upstream: []model.ColumnSpec{
				{Name: "order_id", Type: "bigint", Nullable: true},
				{Name: "order_no", Type: "string"},
			}})
		if err != nil {
			t.Fatal(err)
		}
		// _no 只是候选，不参与启发式主键；可空列同样不参与，推不出来要告警。
		if len(got.Spec.PrimaryKey) != 0 {
			t.Fatalf("不应推断出主键: %v", got.Spec.PrimaryKey)
		}
		if !hasWarning(got.Warnings, "未能推断主键") {
			t.Fatalf("缺少主键提示: %v", got.Warnings)
		}
		cands := strings.Join(got.KeyCandidates, ",")
		if !strings.Contains(cands, "order_id") || !strings.Contains(cands, "order_no") {
			t.Fatalf("候选错误: %v", got.KeyCandidates)
		}
	})
	t.Run("ODS 不设主键", func(t *testing.T) {
		got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "ods", Name: "order",
			Upstream: []model.ColumnSpec{{Name: "id", Type: "bigint", PrimaryKey: true}}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.PrimaryKey != nil || len(got.Spec.UniqueKey) != 0 {
			t.Fatalf("ODS 不应有主键: %+v", got.Spec)
		}
		// ODS 追加审计列。
		if columnNames(got.Spec.Columns) != "dt,id,_godw_src,_godw_load_at" {
			t.Fatalf("ODS 审计列错误: %v", columnNames(got.Spec.Columns))
		}
	})
}

// 名称清洗：带空格/数字开头/纯符号的上游列名必须变成合法标识符，
// 否则生成的 YAML 连 Validate 都过不去。
func TestScaffoldSanitize(t *testing.T) {
	got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t",
		Upstream: []model.ColumnSpec{
			{Name: "1col", Type: "string"},
			{Name: "a-b", Type: "string"},
			{Name: "__x__", Type: "string"},
			{Name: "中文", Type: "string"},
			{Name: "Order ID", Type: "string"},
			{Name: "_1", Type: "string"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if columnNames(got.Spec.Columns) != "dt,c1col,a_b,x,col,order_id,c_1" {
		t.Fatalf("名称清洗错误: %v", columnNames(got.Spec.Columns))
	}
}

// 有损类型必须作为 warning 抛出，不能静默截断。
func TestScaffoldTypeWarning(t *testing.T) {
	got, err := modelc.Scaffold(modelc.ScaffoldOptions{Layer: "dwd", Name: "t", Engine: "mysql",
		Upstream: []model.ColumnSpec{{Name: "updated_at", Type: "timestamp"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "会丢失信息") {
		t.Fatalf("有损映射应告警: %v", got.Warnings)
	}
}
