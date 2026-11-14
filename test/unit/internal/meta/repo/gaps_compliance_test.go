package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
)

// 合规面：保留策略、删除权任务、表敏感级别。
// 这里三张表互不相关，共用一个独占 schema 即可（各自只改自己的列），
// 避免为三条断言各建一次库。

// 保留天数被写成文本（结构漂移）时策略列表必须报错：
// 读成 0 会让归档任务把所有分区立刻删掉。
func TestComplianceListScanFailures(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	ctx := context.Background()

	if err := r.SaveRetentionPolicy(ctx, ws.ID, model.RetentionPolicy{ModelName: "dwd_order", RetainDays: 30}); err != nil {
		t.Fatalf("保存保留策略失败: %v", err)
	}
	if err := r.CreateErasureJob(ctx, model.ErasureJob{
		ID: "er1", WorkspaceID: ws.ID, ModelName: "dwd_order", SubjectKey: "u1", Status: "pending",
	}); err != nil {
		t.Fatalf("创建删除权任务失败: %v", err)
	}
	if err := r.SaveColumnPolicy(ctx, ws.ID, model.ColumnPolicy{
		TableName: "dwd_order", ColumnName: "phone", Classification: "secret",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}

	t.Run("保留策略", func(t *testing.T) {
		textColumn(t, db, "retention_policy", "retain_days")
		_, err := r.ListRetentionPolicies(ctx, ws.ID)
		wantErr(t, err, "scan")
	})

	t.Run("删除权任务", func(t *testing.T) {
		textColumn(t, db, "erasure_job", "rows_deleted")
		_, err := r.ListErasureJobs(ctx, ws.ID, 10)
		wantErr(t, err, "scan")
	})

	t.Run("表敏感级别", func(t *testing.T) {
		// 级别为 NULL 时不能当成 none 放行：把 secret 表识别成 public
		// 等于直接漏数据。
		nullify(t, db, "column_policy", "classification")
		_, err := r.TableClassification(ctx, ws.ID, []string{"dwd_order"})
		wantErr(t, err, "NULL")
	})
}
