package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
)

// prepareSearch 造一组合适的搜索语料：
//
//	model   orders          (dwd)  描述「订单主表」
//	model   orders_archive  (ads)  描述「归档」
//	dataset order_items     (public) 描述「订单明细」，带 pii 标签
//	pipeline sync_orders    描述「同步订单」
//	chart   orders_kpi
func prepareSearch(t *testing.T, r *repo.Repo, db *repo.DB, wsID, dsID string) {
	t.Helper()
	ctx := context.Background()
	for _, m := range []struct{ name, layer, desc string }{
		{"orders", "dwd", "订单主表"},
		{"orders_archive", "ads", "归档"},
	} {
		if _, err := r.SaveModel(ctx, wsID, model.ModelSpec{
			Kind: "model", Name: m.name, Layer: m.layer, Description: m.desc,
		}, "", "", "tester", true); err != nil {
			t.Fatalf("准备模型 %s 失败: %v", m.name, err)
		}
	}
	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: dsID, Schema: "public", Name: "order_items", Type: "table", Comment: "订单明细",
	}, wsID); err != nil {
		t.Fatalf("准备数据集失败: %v", err)
	}
	if _, err := r.SavePipeline(ctx, wsID, model.PipelineSpec{Name: "sync_orders", Description: "同步订单"}, "tester", true); err != nil {
		t.Fatalf("准备管道失败: %v", err)
	}
	execSQL(t, db, `INSERT INTO chart(id,workspace_id,name,dataset) VALUES('cht_1',$1,'orders_kpi','order_items')`, wsID)
}

func rowNames(rows []repo.SearchRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Type + ":" + r.Name
	}
	return out
}

// 关键词搜索是双通道：中文靠 ILIKE 保证子串命中，英文靠 tsquery。
// 命中档位决定排序：全等 > 前缀 > 包含 > 仅描述命中。
func TestSearchMetadataKeyword(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")
	prepareSearch(t, r, db, ws.ID, ds.ID)

	// 英文标识符：全等优先，其次前缀，最后包含。
	rows, err := r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "orders"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	want := []string{"model:orders", "model:orders_archive", "chart:orders_kpi", "pipeline:sync_orders"}
	if got := rowNames(rows); len(got) != len(want) {
		t.Fatalf("命中数量不对: %v", got)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("排序不对，第 %d 条应是 %s，实际 %v", i, want[i], got)
			}
		}
	}
	if rows[0].Score <= rows[1].Score {
		t.Fatalf("全等命中的分数应高于前缀命中: %v vs %v", rows[0].Score, rows[1].Score)
	}
	if rows[1].Layer != "ads" || rows[0].Layer != "dwd" {
		t.Fatalf("模型应带分层: %+v", rows[:2])
	}
	if rows[3].Comment != "同步订单" || rows[3].Layer != "" {
		t.Fatalf("无分层实体应返回空分层: %+v", rows[3])
	}

	// 中文子串：simple 分词器不切中文，必须靠 ILIKE 命中描述。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "订单"})
	if err != nil {
		t.Fatalf("中文搜索失败: %v", err)
	}
	got := rowNames(rows)
	if len(got) != 3 {
		t.Fatalf("中文应命中 3 条（模型/数据集/管道），实际 %v", got)
	}
	// 同为 1 分，按名称升序。
	want = []string{"dataset:order_items", "model:orders", "pipeline:sync_orders"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("中文命中排序不对: %v", got)
		}
	}

	// 前后空格不影响结果。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "  订单  "})
	if err != nil {
		t.Fatalf("带空格搜索失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("trim 后应命中 3 条，实际 %d", len(rows))
	}

	// 大小写不敏感。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "ORDERS"})
	if err != nil {
		t.Fatalf("大写搜索失败: %v", err)
	}
	if len(rows) == 0 || rows[0].Name != "orders" {
		t.Fatalf("大写也应命中全等项: %+v", rows)
	}

	// 无关键词 = 列出全部实体。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{})
	if err != nil {
		t.Fatalf("空关键词搜索失败: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("空关键词应列出全部 5 个实体，实际 %d: %v", len(rows), rowNames(rows))
	}
	if rows[0].Score != 0 {
		t.Fatalf("无关键词时分数应为 0: %v", rows[0].Score)
	}

	// 搜索不到时返回空集合。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "不存在的词"})
	if err != nil {
		t.Fatalf("空结果搜索失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("应为空集合: %v", rowNames(rows))
	}
	// 跨工作区隔离。
	if rows, err := r.SearchMetadata(ctx, "ws_none", repo.SearchFilter{Q: "orders"}); err != nil || len(rows) != 0 {
		t.Fatalf("其他工作区不应命中: %v %v", rows, err)
	}
}

// 过滤维度：类型、分层、标签、责任人、数据源，以及 limit。
func TestSearchMetadataFilters(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")
	other := seedDatasource(t, r, ws.ID, "ch")
	prepareSearch(t, r, db, ws.ID, ds.ID)

	// 类型过滤：只查数据集。
	rows, err := r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Q: "订单", Types: []string{"dataset"}})
	if err != nil {
		t.Fatalf("类型过滤失败: %v", err)
	}
	if got := rowNames(rows); len(got) != 1 || got[0] != "dataset:order_items" {
		t.Fatalf("类型过滤结果不对: %v", got)
	}
	// 类型名大小写与空格都要容错。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Types: []string{" DATASET "}})
	if err != nil {
		t.Fatalf("类型容错失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != "dataset" {
		t.Fatalf("类型名应容错: %+v", rows)
	}
	// 未知类型导致没有可查分支，直接返回空。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Types: []string{"unknown"}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("未知类型应返回空: %v %v", rows, err)
	}

	// 分层过滤：只有模型有分层，其余实体被排除。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Layer: "dwd"})
	if err != nil {
		t.Fatalf("分层过滤失败: %v", err)
	}
	if got := rowNames(rows); len(got) != 1 || got[0] != "model:orders" {
		t.Fatalf("分层过滤结果不对: %v", got)
	}

	// 标签过滤：entity_tag 与 dataset.tags 两条来源都要命中。
	if _, err := r.SaveTag(ctx, ws.ID, "pii", "red", "敏感"); err != nil {
		t.Fatalf("创建标签失败: %v", err)
	}
	if err := r.TagEntity(ctx, ws.ID, "model", "orders", []string{"pii"}); err != nil {
		t.Fatalf("给模型打标失败: %v", err)
	}
	execSQL(t, db, `UPDATE dataset SET tags='{pii}' WHERE table_name='order_items'`)
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Tags: []string{"pii"}})
	if err != nil {
		t.Fatalf("标签过滤失败: %v", err)
	}
	got := rowNames(rows)
	if len(got) != 2 || got[0] != "dataset:order_items" || got[1] != "model:orders" {
		t.Fatalf("标签过滤结果不对: %v", got)
	}
	if len(rows[1].Tags) != 1 || rows[1].Tags[0] != "pii" {
		t.Fatalf("结果应带标签: %+v", rows[1].Tags)
	}

	// 责任人：约定用 owner:<user> 标签表达，搜索结果里要拆出来。
	if err := r.TagEntity(ctx, ws.ID, "model", "orders", []string{"owner:alice"}); err != nil {
		t.Fatalf("设置责任人失败: %v", err)
	}
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Owner: "alice"})
	if err != nil {
		t.Fatalf("责任人过滤失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "orders" {
		t.Fatalf("责任人过滤结果不对: %+v", rows)
	}
	if rows[0].Owner != "alice" {
		t.Fatalf("owner 字段应被拆出: %+v", rows[0])
	}
	// owner 标签不应留在普通标签里。
	for _, tag := range rows[0].Tags {
		if strings.HasPrefix(tag, "owner:") {
			t.Fatalf("owner 标签不应出现在 tags 中: %+v", rows[0].Tags)
		}
	}

	// 数据源过滤只对数据集有意义：其他类型的分支被整体跳过。
	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: other.ID, Schema: "public", Name: "clock", Type: "table",
	}, ws.ID); err != nil {
		t.Fatalf("准备第二个数据源的表失败: %v", err)
	}
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Datasource: ds.ID})
	if err != nil {
		t.Fatalf("数据源过滤失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "order_items" {
		t.Fatalf("数据源过滤结果不对: %+v", rows)
	}

	// limit 生效：SQL 里下推，不是取回再截断。
	rows, err = r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Limit: 2})
	if err != nil {
		t.Fatalf("limit 搜索失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit=2 应只返回 2 条，实际 %d", len(rows))
	}
	// 非法 limit 回落到 50。
	if rows, err := r.SearchMetadata(ctx, ws.ID, repo.SearchFilter{Limit: -1}); err != nil || len(rows) != 6 {
		t.Fatalf("非法 limit 应回落默认值: %d %v", len(rows), err)
	}
}

// 标签定义：同名 upsert 只改颜色与描述；删除标签要连带摘掉所有挂载。
func TestTagDefinitionLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	if _, err := r.SaveTag(ctx, ws.ID, "   ", "", ""); err == nil {
		t.Fatal("空标签名应报错")
	}

	tag, err := r.SaveTag(ctx, ws.ID, " core ", "blue", "核心资产")
	if err != nil {
		t.Fatalf("创建标签失败: %v", err)
	}
	// 名称会被 trim。
	if tag.Name != "core" || tag.Color != "blue" || tag.Description != "核心资产" || tag.CreatedAt.IsZero() {
		t.Fatalf("标签字段不对: %+v", tag)
	}

	updated, err := r.SaveTag(ctx, ws.ID, "core", "red", "核心资产 v2")
	if err != nil {
		t.Fatalf("更新标签失败: %v", err)
	}
	if updated.ID != tag.ID {
		t.Fatalf("同名 upsert 应复用 ID: %s != %s", updated.ID, tag.ID)
	}
	if updated.Color != "red" || updated.Description != "核心资产 v2" {
		t.Fatalf("更新未生效: %+v", updated)
	}

	if _, err := r.SaveTag(ctx, other.ID, "core", "", ""); err != nil {
		t.Fatalf("其他工作区创建同名标签失败: %v", err)
	}

	// 挂载数：两个实体挂 core，另一个标签没人用。
	if err := r.TagEntity(ctx, ws.ID, "model", "m1", []string{"core"}); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if err := r.TagEntity(ctx, ws.ID, "pipeline", "p1", []string{"core"}); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if _, err := r.SaveTag(ctx, ws.ID, "empty", "", ""); err != nil {
		t.Fatalf("创建空标签失败: %v", err)
	}

	tags, err := r.ListTags(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列标签失败: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "core" || tags[1].Name != "empty" {
		t.Fatalf("应按名称排序: %+v", tags)
	}
	if tags[0].Count != 2 {
		t.Fatalf("core 的挂载数应为 2，实际 %d", tags[0].Count)
	}
	if tags[1].Count != 0 {
		t.Fatalf("empty 的挂载数应为 0，实际 %d", tags[1].Count)
	}
	if got, err := r.ListTags(ctx, other.ID); err != nil || len(got) != 1 {
		t.Fatalf("其他工作区应只有一个标签: %v %v", got, err)
	}

	// 删除标签定义会摘掉所有挂载，避免留下孤儿标签。
	if err := r.DeleteTag(ctx, ws.ID, "core"); err != nil {
		t.Fatalf("删除标签失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM entity_tag WHERE workspace_id=$1 AND tag='core'`, ws.ID); n != 0 {
		t.Fatalf("挂载应被摘掉: %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM tag WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("标签定义应只剩 empty: %d", n)
	}
	if tags, err := r.EntityTags(ctx, ws.ID, "model", "m1"); err != nil || len(tags) != 0 {
		t.Fatalf("删除后实体不应还有标签: %v %v", tags, err)
	}
}

// 打标必须校验：实体类型白名单、标签必须先定义（owner: 例外）。
func TestTagEntityValidation(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	if _, err := r.SaveTag(ctx, ws.ID, "core", "", ""); err != nil {
		t.Fatalf("创建标签失败: %v", err)
	}

	if err := r.TagEntity(ctx, ws.ID, "unknown", "x", []string{"core"}); err == nil {
		t.Fatal("非法实体类型应报错")
	}
	if err := r.TagEntity(ctx, ws.ID, "model", "  ", []string{"core"}); err == nil {
		t.Fatal("空实体名应报错")
	}
	// 未定义的标签视为手误，直接拒绝：否则标签体系会退化成自由文本。
	if err := r.TagEntity(ctx, ws.ID, "model", "m1", []string{"core", "undefined"}); err == nil {
		t.Fatal("未定义的标签应报错")
	}
	// 前面的标签已写入，报错发生在中途——这是「尽力而为」的语义，值得钉住。
	if tags, err := r.EntityTags(ctx, ws.ID, "model", "m1"); err != nil || len(tags) != 1 {
		t.Fatalf("已写入的标签应保留: %v %v", tags, err)
	}

	// owner: 前缀可以不走 tag 表（责任人不是标签体系的一部分）。
	if err := r.TagEntity(ctx, ws.ID, "model", "m1", []string{"owner:alice"}); err != nil {
		t.Fatalf("owner 标签应被允许: %v", err)
	}
	if tags, err := r.EntityTags(ctx, ws.ID, "model", "m1"); err != nil || len(tags) != 2 {
		t.Fatalf("应有 2 个标签: %v %v", tags, err)
	}

	// 空标签与空白标签被跳过。
	if err := r.TagEntity(ctx, ws.ID, "model", "m2", []string{"", "  ", "core"}); err != nil {
		t.Fatalf("空白标签应被跳过: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM entity_tag WHERE entity_name='m2'`); n != 1 {
		t.Fatalf("只应写入 core: %d", n)
	}

	// 重复打标是幂等的。
	if err := r.TagEntity(ctx, ws.ID, "model", "m1", []string{"core", "core"}); err != nil {
		t.Fatalf("重复打标应成功: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM entity_tag WHERE entity_name='m1' AND tag='core'`); n != 1 {
		t.Fatalf("重复打标不应产生多行: %d", n)
	}

	// 实体名两端的空格会被 trim。
	if err := r.TagEntity(ctx, ws.ID, " model ", "  m3  ", []string{"core"}); err != nil {
		t.Fatalf("带空格打标失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM entity_tag WHERE entity_type='model' AND entity_name='m3'`); n != 1 {
		t.Fatalf("实体名应被 trim: %d", n)
	}

	// 摘标签：只摘指定那个，其余保留。
	if err := r.UntagEntity(ctx, ws.ID, "model", "m1", "owner:alice"); err != nil {
		t.Fatalf("摘标签失败: %v", err)
	}
	tags, err := r.EntityTags(ctx, ws.ID, "model", "m1")
	if err != nil {
		t.Fatalf("查实体标签失败: %v", err)
	}
	if len(tags) != 1 || tags[0] != "core" {
		t.Fatalf("应只剩 core: %v", tags)
	}
	// 摘不存在的标签不报错。
	if err := r.UntagEntity(ctx, ws.ID, "model", "m1", "nope"); err != nil {
		t.Fatalf("摘不存在的标签应成功: %v", err)
	}

	// 反查：某标签下挂了哪些实体，按类型+名称排序。
	entities, err := r.EntitiesByTag(ctx, ws.ID, "core", 10)
	if err != nil {
		t.Fatalf("按标签反查失败: %v", err)
	}
	if len(entities) != 3 {
		t.Fatalf("core 应挂在 3 个实体上，实际 %d: %+v", len(entities), entities)
	}
	if entities[0].Type != "model" || entities[0].Name != "m1" || entities[1].Name != "m2" || entities[2].Name != "m3" {
		t.Fatalf("反查排序不对: %+v", entities)
	}
	// limit 生效与默认值。
	if one, err := r.EntitiesByTag(ctx, ws.ID, "core", 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 未生效: %v %v", one, err)
	}
	if three, err := r.EntitiesByTag(ctx, ws.ID, "core", 0); err != nil || len(three) != 3 {
		t.Fatalf("非法 limit 应回落默认值: %d %v", len(three), err)
	}
	if none, err := r.EntitiesByTag(ctx, ws.ID, "nope", 10); err != nil || len(none) != 0 {
		t.Fatalf("无挂载的标签应返回空: %v %v", none, err)
	}
}
