package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
)

// 本文件覆盖搜索与标签：空关键词/空标签、标签未定义、删除标签的级联。
// 标签过滤是「多个条件 AND」拼出来的，任何一个空值被当成条件都会让搜索变成 0 命中，
// 用户会以为数据不存在——所以空标签必须被跳过。

// 空白标签必须被跳过而不是拼成 `tag = ''`：后者会让搜索永远无结果。
func TestSearchMetadataIgnoresBlankTag(t *testing.T) {
	// 只读接口，无需独占 schema。
	r, _ := newRepoDB(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveModel(context.Background(), ws.ID, model.ModelSpec{Name: "order_detail", Layer: "dwd"}, "", "", "tester", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}

	rows, err := r.SearchMetadata(context.Background(), ws.ID, repo.SearchFilter{
		Q:    "order",
		Tags: []string{"  ", ""},
	})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "order_detail" {
		t.Fatalf("空白标签不应参与过滤，期望命中 order_detail，实际 %+v", rows)
	}
}

// 标签颜色为 NULL 时标签列表必须报错（前端按颜色渲染，NULL 会让整块列表崩）。
func TestListTagsFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "#f00", "敏感"); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	nullify(t, db, "tag", "color")

	_, err := r.ListTags(context.Background(), ws.ID)
	wantErr(t, err, "NULL")
}

// 删除标签要从 entity_tag 开始清理；第一步失败必须中止，
// 否则标签定义没了、挂载还在，反查会指向一个不存在的标签。
func TestDeleteTagFailsWhenUntagRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "", ""); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	if err := r.TagEntity(context.Background(), ws.ID, "model", "m", []string{"pii"}); err != nil {
		t.Fatalf("挂标签失败: %v", err)
	}
	failOn(t, db, "entity_tag", "DELETE", "")

	err := r.DeleteTag(context.Background(), ws.ID, "pii")
	wantErr(t, err, "注入故障: entity_tag delete")

	if n := queryInt(t, db, `SELECT count(*) FROM tag WHERE name='pii'`); n != 1 {
		t.Fatalf("摘除失败时标签定义必须保留，实际 %d 行", n)
	}
}

// 第二步（删标签定义）失败必须回滚，否则挂载被删了但标签还在，
// 标签管理页会出现一个「挂载数为 0 却仍被引用」的幻影。
func TestDeleteTagFailsWhenDefinitionDeleteRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "", ""); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	if err := r.TagEntity(context.Background(), ws.ID, "model", "m", []string{"pii"}); err != nil {
		t.Fatalf("挂标签失败: %v", err)
	}
	failOn(t, db, "tag", "DELETE", "")

	err := r.DeleteTag(context.Background(), ws.ID, "pii")
	wantErr(t, err, "注入故障: tag delete")

	if n := queryInt(t, db, `SELECT count(*) FROM entity_tag WHERE tag='pii'`); n != 1 {
		t.Fatalf("删除标签失败时挂载必须随事务回滚，实际 %d 行", n)
	}
}

// 挂载写入失败时必须报错：调用方需要知道标签没挂上，
// 否则权限规则会按「已打标」放行一张其实没标的数据表。
func TestTagEntityFailsWhenInsertRejected(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "", ""); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	failOn(t, db, "entity_tag", "INSERT", "")

	err := r.TagEntity(context.Background(), ws.ID, "model", "m", []string{"pii"})
	wantErr(t, err, "注入故障: entity_tag insert")
}

// 标签值为 NULL（历史脏数据、约束缺失）时 EntityTags 必须报错。
func TestEntityTagsFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "", ""); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	if err := r.TagEntity(context.Background(), ws.ID, "model", "m", []string{"pii"}); err != nil {
		t.Fatalf("挂标签失败: %v", err)
	}
	nullifyKeyColumn(t, db, "entity_tag", "tag")

	_, err := r.EntityTags(context.Background(), ws.ID, "model", "m")
	wantErr(t, err, "NULL")
}

// 反查标签下的实体时实体名为 NULL 必须报错，
// 否则前端会渲染一条点不开的空记录。
func TestEntitiesByTagFailsOnNullColumn(t *testing.T) {
	r, db := isolatedRepo(t)
	ws := seedWorkspace(t, r, "w")
	if _, err := r.SaveTag(context.Background(), ws.ID, "pii", "", ""); err != nil {
		t.Fatalf("保存标签失败: %v", err)
	}
	if err := r.TagEntity(context.Background(), ws.ID, "model", "m", []string{"pii"}); err != nil {
		t.Fatalf("挂标签失败: %v", err)
	}
	nullifyKeyColumn(t, db, "entity_tag", "entity_name")

	_, err := r.EntitiesByTag(context.Background(), ws.ID, "pii", 10)
	wantErr(t, err, "NULL")
}
