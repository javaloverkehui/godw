package modelc_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/modelc"
)

// Diff 只有一条核心判定：会不会让下游读不到/读错数据。
// 判错方向的代价是要么误拦正常发布，要么把破坏性变更放出去。
func TestDiff(t *testing.T) {
	old := baseSpec()

	t.Run("无变化", func(t *testing.T) {
		got := modelc.Diff(old, old)
		if len(got.Changes) != 0 || got.Breaking != 0 || got.Safe != 0 || got.HasBreaking() {
			t.Fatalf("无变化时结果错误: %+v", got)
		}
	})

	cases := []struct {
		name        string
		mutate      func(*model.ModelSpec)
		kind        modelc.ChangeKind
		column      string
		breaking    bool
		reasonEmpty bool
	}{
		{"删列", func(s *model.ModelSpec) { s.Columns = s.Columns[:2] }, modelc.ChangeDropColumn, "dt", true, false},
		{"加可空列", func(s *model.ModelSpec) {
			s.Columns = append(s.Columns, model.ColumnSpec{Name: "note", Type: "string", Nullable: true})
		}, modelc.ChangeAddColumn, "note", false, true},
		{"加带默认值列", func(s *model.ModelSpec) {
			s.Columns = append(s.Columns, model.ColumnSpec{Name: "n", Type: "int", Default: "0"})
		}, modelc.ChangeAddColumn, "n", false, true},
		{"加非空无默认列", func(s *model.ModelSpec) {
			s.Columns = append(s.Columns, model.ColumnSpec{Name: "n", Type: "int"})
		}, modelc.ChangeAddColumn, "n", true, false},
		{"改类型", func(s *model.ModelSpec) { s.Columns[1].Type = "double" }, modelc.ChangeModifyType, "amount", true, false},
		{"可空改非空", func(s *model.ModelSpec) { s.Columns[1].Nullable = false }, modelc.ChangeModifyNull, "amount", true, false},
		{"非空改可空", func(s *model.ModelSpec) { s.Columns[0].Nullable = true }, modelc.ChangeModifyNull, "id", false, true},
		{"改注释", func(s *model.ModelSpec) { s.Columns[0].Comment = "主键" }, modelc.ChangeModifyComment, "id", false, true},
		{"改主键", func(s *model.ModelSpec) { s.PrimaryKey = []string{"id", "dt"} }, modelc.ChangeModifyPK, "", true, false},
		{"改分区", func(s *model.ModelSpec) { s.PartitionBy = []string{"date_trunc('month', dt)"} }, modelc.ChangeModifyPart, "", true, false},
		{"改 TTL", func(s *model.ModelSpec) { s.TTL = "30d" }, modelc.ChangeModifyTTL, "", false, false},
		{"改物化", func(s *model.ModelSpec) { s.Materialization = "view" }, modelc.ChangeModifyMat, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := old
			next.Columns = append([]model.ColumnSpec{}, old.Columns...)
			tc.mutate(&next)
			got := modelc.Diff(old, next)
			if len(got.Changes) != 1 {
				t.Fatalf("变更数错误: %+v", got.Changes)
			}
			c := got.Changes[0]
			if c.Kind != tc.kind || c.Column != tc.column || c.Breaking != tc.breaking {
				t.Fatalf("变更错误: %+v", c)
			}
			if tc.reasonEmpty && c.Reason != "" {
				t.Fatalf("安全变更不应有原因: %+v", c)
			}
			if !tc.reasonEmpty && c.Reason == "" {
				t.Fatalf("破坏性变更必须说明原因: %+v", c)
			}
			if got.HasBreaking() != tc.breaking {
				t.Fatalf("HasBreaking 错误: %+v", got)
			}
			if got.Breaking+got.Safe != 1 {
				t.Fatalf("计数错误: %+v", got)
			}
		})
	}

	// 主键大小写不同不算变更：YAML 里大小写漂移不应该触发审批。
	upper := old
	upper.PrimaryKey = []string{"ID"}
	if changes := modelc.Diff(old, upper).Changes; len(changes) != 0 {
		t.Fatalf("主键大小写不应算变更: %+v", changes)
	}
}

func migrationSpec() model.ModelSpec {
	s := baseSpec()
	s.Schema = "dwd"
	return s
}

// 迁移方案只自动处理能安全 ALTER 的部分；
// 不能自动完成的必须进入 Manual，不能假装能做。
func TestPlanMigration(t *testing.T) {
	old := migrationSpec()

	t.Run("加可空列", func(t *testing.T) {
		next := old
		next.Columns = append(append([]model.ColumnSpec{}, old.Columns...),
			model.ColumnSpec{Name: "note", Type: "string", Nullable: true})
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		if len(got.Statements) != 1 || !strings.Contains(got.Statements[0], `ADD COLUMN "note" varchar(255)`) {
			t.Fatalf("语句错误: %+v", got)
		}
		if len(got.Manual) != 0 || got.Rebuild {
			t.Fatalf("不应需要人工步骤: %+v", got)
		}
	})
	t.Run("加非空带默认列", func(t *testing.T) {
		next := old
		next.Columns = append(append([]model.ColumnSpec{}, old.Columns...),
			model.ColumnSpec{Name: "n", Type: "int", Default: "0"})
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		if !strings.Contains(got.Statements[0], `NOT NULL DEFAULT 0`) {
			t.Fatalf("语句错误: %+v", got.Statements)
		}
	})
	t.Run("加非空无默认列需要两步", func(t *testing.T) {
		next := old
		next.Columns = append(append([]model.ColumnSpec{}, old.Columns...),
			model.ColumnSpec{Name: "n", Type: "int"})
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		if len(got.Manual) != 1 || !strings.Contains(got.Manual[0], "回填") {
			t.Fatalf("应给出手工回填步骤: %+v", got)
		}
	})
	t.Run("加列带注释", func(t *testing.T) {
		next := old
		next.Columns = append(append([]model.ColumnSpec{}, old.Columns...),
			model.ColumnSpec{Name: "note", Type: "string", Nullable: true, Comment: "备注"})
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		joined := strings.Join(got.Statements, "\n")
		if !strings.Contains(joined, "COMMENT ON COLUMN") || strings.Count(joined, "COMMENT ON COLUMN") != 2 {
			t.Fatalf("注释语句错误:\n%s", joined)
		}
	})
	t.Run("clickhouse 加列保持列序", func(t *testing.T) {
		next := old
		next.Columns = append(append([]model.ColumnSpec{}, old.Columns...),
			model.ColumnSpec{Name: "note", Type: "string", Nullable: true})
		got := modelc.PlanMigration(old, next, dialect.ClickHouse{})
		if !strings.Contains(got.Statements[0], "AFTER `dt`") {
			t.Fatalf("ClickHouse 应带 AFTER: %+v", got.Statements)
		}
	})
	t.Run("clickhouse 首列新增不带 AFTER", func(t *testing.T) {
		next := old
		next.Columns = append([]model.ColumnSpec{{Name: "note", Type: "string", Nullable: true}}, old.Columns...)
		got := modelc.PlanMigration(old, next, dialect.ClickHouse{})
		if strings.Contains(got.Statements[0], "AFTER") {
			t.Fatalf("首列新增不应带 AFTER: %+v", got.Statements)
		}
	})
	t.Run("删列", func(t *testing.T) {
		next := old
		next.Columns = old.Columns[:2]
		for name, d := range map[string]dialect.Dialect{"postgres": dialect.Postgres{}, "clickhouse": dialect.ClickHouse{}} {
			got := modelc.PlanMigration(old, next, d)
			if len(got.Statements) != 1 || !strings.Contains(got.Statements[0], "DROP COLUMN "+d.QuoteIdent("dt")) {
				t.Fatalf("%s 删列语句错误: %+v", name, got.Statements)
			}
		}
	})
	t.Run("可空性变更", func(t *testing.T) {
		toNotNull := old
		toNotNull.Columns = append([]model.ColumnSpec{}, old.Columns...)
		toNotNull.Columns[1].Nullable = false
		got := modelc.PlanMigration(old, toNotNull, dialect.Postgres{})
		if !strings.Contains(got.Statements[0], "SET NOT NULL") {
			t.Fatalf("语句错误: %+v", got.Statements)
		}
		toNull := old
		toNull.Columns = append([]model.ColumnSpec{}, old.Columns...)
		toNull.Columns[0].Nullable = true
		got = modelc.PlanMigration(old, toNull, dialect.Postgres{})
		if !strings.Contains(got.Statements[0], "DROP NOT NULL") {
			t.Fatalf("语句错误: %+v", got.Statements)
		}
	})
	t.Run("改类型", func(t *testing.T) {
		next := old
		next.Columns = append([]model.ColumnSpec{}, old.Columns...)
		next.Columns[1].Type = "double"
		ch := modelc.PlanMigration(old, next, dialect.ClickHouse{})
		if len(ch.Statements) != 1 || !strings.Contains(ch.Statements[0], "MODIFY COLUMN `amount` Float64") {
			t.Fatalf("ClickHouse 改类型错误: %+v", ch.Statements)
		}
		pg := modelc.PlanMigration(old, next, dialect.Postgres{})
		if len(pg.Statements) != 0 || len(pg.Manual) != 1 || !strings.Contains(pg.Manual[0], "双写") {
			t.Fatalf("PG 改类型应转人工: %+v", pg)
		}
	})
	t.Run("分区变更触发重建", func(t *testing.T) {
		next := old
		next.PartitionBy = []string{"date_trunc('month', dt)"}
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		if !got.Rebuild || len(got.Manual) < 2 {
			t.Fatalf("应要求重建并给出步骤: %+v", got)
		}
		if !strings.Contains(strings.Join(got.Manual, "\n"), "RENAME") {
			t.Fatalf("缺少重建步骤: %+v", got.Manual)
		}
	})
	t.Run("物化方式变更触发重建", func(t *testing.T) {
		next := old
		next.Materialization = "view"
		if got := modelc.PlanMigration(old, next, dialect.Postgres{}); !got.Rebuild {
			t.Fatalf("物化变更应要求重建: %+v", got)
		}
	})
	t.Run("注释同步", func(t *testing.T) {
		next := old
		next.Description = "订单明细"
		next.Columns = append([]model.ColumnSpec{}, old.Columns...)
		next.Columns[0].Comment = "主键"
		got := modelc.PlanMigration(old, next, dialect.Postgres{})
		joined := strings.Join(got.Statements, "\n")
		if !strings.Contains(joined, `COMMENT ON TABLE "dwd"."dwd_order" IS '订单明细';`) {
			t.Fatalf("表注释缺失:\n%s", joined)
		}
		if !strings.Contains(joined, `COMMENT ON COLUMN "dwd"."dwd_order"."id" IS '主键';`) {
			t.Fatalf("列注释缺失:\n%s", joined)
		}
		// 非 PG 引擎不产出 COMMENT 语句。
		if got := modelc.PlanMigration(old, next, dialect.MySQL{}); len(got.Statements) != 0 {
			t.Fatalf("MySQL 不应产出注释语句: %+v", got.Statements)
		}
	})
	t.Run("无变化", func(t *testing.T) {
		got := modelc.PlanMigration(old, old, dialect.Postgres{})
		if len(got.Statements) != 0 || len(got.Manual) != 0 || got.Rebuild {
			t.Fatalf("无变化不应产出方案: %+v", got)
		}
	})
}
