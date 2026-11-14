package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 模型的核心不变式：每一次保存都追加一个版本快照，publish 才切换 active_version。
// 历史执行依赖「已发布版本不可改」，所以这里逐版本核对 status 与 published_at。
func TestSaveModelVersionsAndPublish(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")

	spec := model.ModelSpec{
		Kind: "model", Name: "dwd_order", Layer: "dwd",
		Description: "订单明细", Datasource: "pg",
		Columns: []model.ColumnSpec{{Name: "id", Type: "bigint"}},
	}

	// 第一版：草稿，不切 active_version。
	m, err := r.SaveModel(ctx, ws.ID, spec, ds.ID, "CREATE TABLE ...", "alice", false)
	if err != nil {
		t.Fatalf("保存草稿失败: %v", err)
	}
	if m.ID == "" || m.ActiveVersion != 0 {
		t.Fatalf("草稿不应有生效版本: %+v", m)
	}
	if m.DatasourceID != ds.ID || m.Layer != "dwd" {
		t.Fatalf("模型字段不对: %+v", m)
	}

	versions, err := r.ModelVersions(ctx, ws.ID, "dwd_order")
	if err != nil {
		t.Fatalf("列出版本失败: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("应有 1 个版本，实际 %d", len(versions))
	}
	if versions[0].Version != 1 || versions[0].Status != "draft" {
		t.Fatalf("首版应是 draft: %+v", versions[0])
	}
	// published_at 只有在真正发布时才写；published_by 记录的是「谁保存的」，草稿也有。
	if versions[0].PublishedAt != nil {
		t.Fatalf("草稿不应有发布时间: %+v", versions[0])
	}
	if versions[0].PublishedBy != "alice" {
		t.Fatalf("草稿也应记录保存人: %+v", versions[0])
	}
	if versions[0].DDL != "CREATE TABLE ..." || versions[0].Spec.Name != "dwd_order" {
		t.Fatalf("版本快照不完整: %+v", versions[0])
	}

	// 未发布时执行器取不到口径。
	if _, err := r.PublishedModelSpec(ctx, ws.ID, "dwd_order"); err == nil {
		t.Fatal("未发布的模型不应返回生效定义")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// 第二版并发布：旧版本归档，active_version 指向 2。
	spec.Description = "订单明细 v2"
	m, err = r.SaveModel(ctx, ws.ID, spec, ds.ID, "CREATE TABLE v2", "alice", true)
	if err != nil {
		t.Fatalf("发布模型失败: %v", err)
	}
	if m.ActiveVersion != 2 {
		t.Fatalf("发布后 active_version 应为 2，实际 %d", m.ActiveVersion)
	}

	versions, err = r.ModelVersions(ctx, ws.ID, "dwd_order")
	if err != nil {
		t.Fatalf("复查版本失败: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("应有 2 个版本，实际 %d", len(versions))
	}
	if versions[0].Version != 2 || versions[0].Status != "published" {
		t.Fatalf("最新版本应是 published: %+v", versions[0])
	}
	if versions[0].PublishedAt == nil || versions[0].PublishedBy != "alice" {
		t.Fatalf("发布信息应写入: %+v", versions[0])
	}
	// 归档只作用于「曾经发布过」的版本；v1 一直是草稿，所以仍是 draft。
	if versions[1].Version != 1 || versions[1].Status != "draft" {
		t.Fatalf("未发布过的旧版本应保持 draft: %+v", versions[1])
	}
	// 历史版本的快照内容不能跟着改。
	if versions[1].Spec.Description != "订单明细" || versions[1].DDL != "CREATE TABLE ..." {
		t.Fatalf("历史快照被改写: %+v", versions[1])
	}

	published, err := r.PublishedModelSpec(ctx, ws.ID, "dwd_order")
	if err != nil {
		t.Fatalf("取生效定义失败: %v", err)
	}
	if published.Description != "订单明细 v2" {
		t.Fatalf("生效定义应是第 2 版: %+v", published)
	}

	// 列表要带上生效版本的 spec 与 ddl。
	models, err := r.ListModels(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列模型失败: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("应有 1 个模型，实际 %d", len(models))
	}
	if models[0].ActiveVersion != 2 || models[0].Spec == nil || models[0].DDL != "CREATE TABLE v2" {
		t.Fatalf("列表未带生效版本: %+v", models[0])
	}
	if models[0].Spec.Description != "订单明细 v2" {
		t.Fatalf("列表里的 spec 应是生效版本: %+v", models[0].Spec)
	}

	// 更新描述/分层会刷新 updated_at，但不允许把 active_version 覆盖掉。
	before := models[0].UpdatedAt
	spec.Layer = "dws"
	if _, err := r.SaveModel(ctx, ws.ID, spec, "", "", "bob", false); err != nil {
		t.Fatalf("保存第 3 版失败: %v", err)
	}
	var layer, dsID string
	var active int
	if err := db.Pool.QueryRow(ctx,
		`SELECT layer, COALESCE(datasource_id,''), active_version FROM model WHERE id=$1`, models[0].ID).
		Scan(&layer, &dsID, &active); err != nil {
		t.Fatalf("读取模型行失败: %v", err)
	}
	if layer != "dws" {
		t.Fatalf("分层应被更新: %q", layer)
	}
	if active != 2 {
		t.Fatalf("保存草稿不应改 active_version: %d", active)
	}
	if dsID != "" {
		t.Fatalf("datasourceID 传空应写成 NULL: %q", dsID)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM model WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("重复保存不应新增模型行: %d", n)
	}
	if after := queryStr(t, db, `SELECT updated_at::text FROM model WHERE id=$1`, models[0].ID); after == before.String() {
		t.Fatalf("updated_at 应被刷新: %q vs %v", after, before)
	}
}

// 回滚只改指针与状态，不动任何 DDL 与快照。
func TestRollbackModelMovesActiveVersionBack(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	spec := model.ModelSpec{Kind: "model", Name: "dim_user", Layer: "dim", Description: "v1"}
	if _, err := r.SaveModel(ctx, ws.ID, spec, "", "DDL v1", "alice", true); err != nil {
		t.Fatalf("发布 v1 失败: %v", err)
	}
	spec.Description = "v2"
	if _, err := r.SaveModel(ctx, ws.ID, spec, "", "DDL v2", "alice", true); err != nil {
		t.Fatalf("发布 v2 失败: %v", err)
	}

	if err := r.RollbackModel(ctx, ws.ID, "dim_user", 1); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT active_version FROM model WHERE name='dim_user'`); n != 1 {
		t.Fatalf("active_version 应回退到 1，实际 %d", n)
	}
	// 状态机：回滚目标 published，其余全部 archived。
	var published int
	var archived int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='published'), count(*) FILTER (WHERE status='archived')
		 FROM model_version`).Scan(&published, &archived); err != nil {
		t.Fatalf("统计版本状态失败: %v", err)
	}
	if published != 1 || archived != 1 {
		t.Fatalf("应恰好 1 published + 1 archived，实际 %d/%d", published, archived)
	}
	if v := queryInt(t, db, `SELECT version FROM model_version WHERE status='published'`); v != 1 {
		t.Fatalf("published 的应是 v1，实际 %d", v)
	}

	got, err := r.PublishedModelSpec(ctx, ws.ID, "dim_user")
	if err != nil {
		t.Fatalf("回滚后取生效定义失败: %v", err)
	}
	if got.Description != "v1" {
		t.Fatalf("回滚后应生效 v1: %+v", got)
	}
	// DDL 快照不随回滚变化：v2 的 DDL 仍属于 v2 那一行。
	if ddl := queryStr(t, db, `SELECT ddl FROM model_version WHERE version=2`); ddl != "DDL v2" {
		t.Fatalf("回滚不应改历史 DDL: %q", ddl)
	}

	if err := r.RollbackModel(ctx, ws.ID, "dim_user", 9); err == nil {
		t.Fatal("不存在的版本应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	if err := r.RollbackModel(ctx, ws.ID, "no_model", 1); err == nil {
		t.Fatal("不存在的模型应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	// 回滚失败不应破坏原有状态。
	if n := queryInt(t, db, `SELECT active_version FROM model WHERE name='dim_user'`); n != 1 {
		t.Fatalf("失败的滚不应改动 active_version: %d", n)
	}
}

// 列表按分层顺序输出（ods → dim → dwd → dws → ads），同层按名称。
// 建模页依赖这个顺序，否则一屏里 ods 与 ads 混排。
func TestListModelsOrdersByLayer(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	save := func(wsID, name, layer string, publish bool) {
		t.Helper()
		spec := model.ModelSpec{Kind: "model", Name: name, Layer: layer}
		if _, err := r.SaveModel(ctx, wsID, spec, "", "", "tester", publish); err != nil {
			t.Fatalf("保存 %s 失败: %v", name, err)
		}
	}
	save(ws.ID, "z_ads", "ads", false)
	save(ws.ID, "b_dwd", "dwd", false)
	save(ws.ID, "a_ods", "ods", false)
	save(ws.ID, "c_dws", "dws", false)
	save(ws.ID, "d_dim", "dim", false)
	save(other.ID, "hidden", "ods", false)

	list, err := r.ListModels(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列模型失败: %v", err)
	}
	want := []string{"a_ods", "d_dim", "b_dwd", "c_dws", "z_ads"}
	if len(list) != len(want) {
		t.Fatalf("应有 %d 个模型，实际 %d", len(want), len(list))
	}
	for i, name := range want {
		if list[i].Name != name {
			t.Fatalf("第 %d 个应是 %s，实际 %s", i, name, list[i].Name)
		}
	}
	// 没有生效版本时 Spec 必须为空，不能被空 JSON 解出零值指针。
	if list[0].Spec != nil || list[0].ActiveVersion != 0 {
		t.Fatalf("未发布的模型不应带 spec: %+v", list[0])
	}

	if got, err := r.ListModels(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("无模型的工作区应返回空集合: %v %v", got, err)
	}
	// 版本列表对不存在的模型返回空集合而不是错误（service 直接展示）。
	if got, err := r.ModelVersions(ctx, ws.ID, "no_model"); err != nil || len(got) != 0 {
		t.Fatalf("不存在模型的版本列表应为空: %v %v", got, err)
	}
}
