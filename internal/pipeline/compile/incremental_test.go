package compile

import (
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

func partModel() model.ModelSpec {
	return model.ModelSpec{
		Name: "dwd_order", Layer: "dwd",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "dt", Type: "date"},
			{Name: "updated_at", Type: "datetime", Nullable: true},
		},
		PartitionBy: []string{"dt"},
		PrimaryKey:  []string{"order_id"},
	}
}

// T0407 验收：分区优先；无分区降级时间列；都无法推导时明确报错。
func TestInferPartition(t *testing.T) {
	inf, err := InferIncremental(partModel(), "2025-11-10", "")
	if err != nil {
		t.Fatal(err)
	}
	if inf.Kind != InferPartition || inf.Where != "dt = '2025-11-10'" {
		t.Fatalf("分区推导错误: %+v", inf)
	}
	if len(inf.ScannedPartitions) != 1 || inf.ScannedPartitions[0] != "2025-11-10" {
		t.Fatalf("扫描分区错误: %+v", inf.ScannedPartitions)
	}

	sqlText := ApplyWhere("SELECT * FROM ods.`order` WHERE status <> 'x'", inf)
	want := "SELECT * FROM (\nSELECT * FROM ods.`order` WHERE status <> 'x'\n) godw_incremental WHERE dt = '2025-11-10'"
	if sqlText != want {
		t.Fatalf("注入结果:\n%s\n期望:\n%s", sqlText, want)
	}
}

func TestInferTimeColumn(t *testing.T) {
	m := partModel()
	m.PartitionBy = nil
	inf, err := InferIncremental(m, "2025-11-10", "")
	if err != nil {
		t.Fatal(err)
	}
	if inf.Kind != InferTimeColumn || inf.Column != "updated_at" {
		t.Fatalf("时间列推导错误: %+v", inf)
	}
	if inf.Where != "updated_at >= '2025-11-10' AND updated_at < '2025-11-11'" {
		t.Fatalf("半开区间错误: %s", inf.Where)
	}
	if len(inf.Warn) == 0 {
		t.Fatal("时间列推导应有性能告警")
	}
}

func TestInferFails(t *testing.T) {
	m := model.ModelSpec{
		Name: "dwd_x", Layer: "dwd",
		Columns: []model.ColumnSpec{{Name: "a", Type: "string"}},
	}
	if _, err := InferIncremental(m, "2025-11-10", ""); err == nil {
		t.Fatal("无法推导时应报错，不得默认全表")
	} else if err.(interface{ Error() string }) == nil {
		t.Fatal("错误信息缺失")
	}

	// 显式 where 优先
	inf, err := InferIncremental(m, "2025-11-10", "a = 'x'")
	if err != nil || inf.Kind != InferExplicit {
		t.Fatalf("显式条件应直接采用: %+v err=%v", inf, err)
	}

	// 非法日期
	if _, err := InferIncremental(partModel(), "2025/11/10", ""); err == nil {
		t.Fatal("非法 biz_date 应报错")
	}
}

func TestPartitionExpr(t *testing.T) {
	m := partModel()
	m.PartitionBy = []string{"date_trunc('month', dt)"}
	inf, err := InferIncremental(m, "2025-11-10", "")
	if err != nil {
		t.Fatal(err)
	}
	if inf.Column != "dt" {
		t.Fatalf("表达式分区应解析出列名 dt，实际 %s", inf.Column)
	}
}

func TestValidateIncremental(t *testing.T) {
	m := partModel()
	inf, _ := InferIncremental(m, "2025-11-10", "")
	if w := ValidateIncremental(m, inf); len(w) != 0 {
		t.Fatalf("正常分区不应告警: %+v", w)
	}
	// 分区列可空 → 告警
	m2 := partModel()
	m2.Columns[1].Nullable = true
	inf2, _ := InferIncremental(m2, "2025-11-10", "")
	w := ValidateIncremental(m2, inf2)
	if len(w) == 0 {
		t.Fatal("分区列可空应告警")
	}
	// 全量 → 告警
	if w := ValidateIncremental(m, Inference{Kind: InferNone}); len(w) == 0 {
		t.Fatal("全量扫描应告警")
	}
}
