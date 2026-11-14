package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 数据源是元数据的根：名称在工作区内唯一、凭证单独加密、状态由探测任务回写。
// 三个点都靠 SQL 约束实现，必须对着真库验证。
func TestDatasourceLifecycle(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	ds, err := r.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: ws.ID,
		Name:        "pg_main",
		Type:        "postgres",
		Config:      map[string]any{"host": "10.0.0.1", "port": float64(5432)},
	}, []byte("cipher"))
	if err != nil {
		t.Fatalf("创建数据源失败: %v", err)
	}
	if !strings.HasPrefix(ds.ID, "ds_") {
		t.Fatalf("ID 应以 ds_ 开头: %q", ds.ID)
	}
	if ds.CreatedAt.IsZero() || ds.UpdatedAt.IsZero() {
		t.Fatalf("时间戳应被填充: %+v", ds)
	}

	// 同名再建必须给出「已存在」业务码，而不是把 pgx 的唯一键冲突抛给上层。
	if _, err := r.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: ws.ID, Name: "pg_main", Type: "mysql",
	}, nil); err == nil {
		t.Fatal("同名数据源应报错")
	} else if errs.From(err).Code != errs.CodeDatasourceDuplicate {
		t.Fatalf("应返回 CodeDatasourceDuplicate，实际 %v", errs.From(err).Code)
	}

	// 工作区外键不存在时不能静默成功。
	if _, err := r.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: "ws_missing", Name: "orphan", Type: "postgres",
	}, nil); err == nil {
		t.Fatal("工作区不存在应触发外键错误")
	}

	// 第二个数据源的名字排在前，用来验证 ORDER BY name。
	if _, err := r.CreateDatasource(ctx, model.Datasource{
		WorkspaceID: ws.ID, Name: "aa_clickhouse", Type: "clickhouse",
		Config: map[string]any{"host": "10.0.0.2"},
	}, nil); err != nil {
		t.Fatalf("创建第二个数据源失败: %v", err)
	}

	list, err := r.ListDatasources(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列出数据源失败: %v", err)
	}
	if len(list) != 2 || list[0].Name != "aa_clickhouse" || list[1].Name != "pg_main" {
		t.Fatalf("应按名称排序: %+v", list)
	}
	if list[1].Status != "unknown" {
		t.Fatalf("新数据源状态应是 unknown，实际 %q", list[1].Status)
	}
	if list[1].Config["host"] != "10.0.0.1" {
		t.Fatalf("config 应往返: %+v", list[1].Config)
	}
	if list[1].LastError != "" || list[1].CheckedAt != nil {
		t.Fatalf("未探测时不应有错误与检测时间: %+v", list[1])
	}

	// 按名取要能带回密文：service 解密时依赖它。
	got, secret, err := r.DatasourceByName(ctx, ws.ID, "pg_main")
	if err != nil {
		t.Fatalf("按名查询失败: %v", err)
	}
	if got.ID != ds.ID || string(secret) != "cipher" {
		t.Fatalf("应取回原记录与密文: %+v %q", got, secret)
	}
	if got.Config["port"] != float64(5432) {
		t.Fatalf("config 中数字应还原为 float64: %+v", got.Config)
	}
	if _, _, err := r.DatasourceByName(ctx, ws.ID, "nope"); err == nil {
		t.Fatal("不存在的数据源应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// 状态回写要同时刷 checked_at 与 updated_at。
	if err := r.UpdateDatasourceStatus(ctx, ds.ID, "error", "connection refused"); err != nil {
		t.Fatalf("更新状态失败: %v", err)
	}
	got, _, err = r.DatasourceByName(ctx, ws.ID, "pg_main")
	if err != nil {
		t.Fatalf("复查数据源失败: %v", err)
	}
	if got.Status != "error" || got.LastError != "connection refused" || got.CheckedAt == nil {
		t.Fatalf("状态回写不完整: %+v", got)
	}
	// status 列有 CHECK 约束，非法状态必须被库拒绝，不能写进脏值。
	if err := r.UpdateDatasourceStatus(ctx, ds.ID, "broken", ""); err == nil {
		t.Fatal("非法状态应被 CHECK 约束拒绝")
	}

	// 删除后按名查不到，再删一次给出 NotFound。
	if err := r.DeleteDatasource(ctx, ws.ID, "aa_clickhouse"); err != nil {
		t.Fatalf("删除数据源失败: %v", err)
	}
	if _, _, err := r.DatasourceByName(ctx, ws.ID, "aa_clickhouse"); err == nil {
		t.Fatal("删除后不应还能查到")
	}
	if err := r.DeleteDatasource(ctx, ws.ID, "aa_clickhouse"); err == nil {
		t.Fatal("重复删除应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// 跨工作区不可见。
	other := seedWorkspace(t, r, "other")
	if lst, err := r.ListDatasources(ctx, other.ID); err != nil || len(lst) != 0 {
		t.Fatalf("其他工作区不应看到数据源: %v %v", lst, err)
	}
	if got, err := r.DatasourceNameByID(ctx, ws.ID, ds.ID); err != nil || got != "pg_main" {
		t.Fatalf("按 ID 取名失败: %q %v", got, err)
	}
	if _, err := r.DatasourceNameByID(ctx, ws.ID, "ds_missing"); err == nil {
		t.Fatal("不存在的 ID 应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}

	// 删数据源要连带删掉其下的表快照（外键 ON DELETE CASCADE）。
	if n := queryInt(t, db, `SELECT count(*) FROM dataset WHERE datasource_id=$1`, ds.ID); n != 0 {
		t.Fatalf("删除数据源后不应残留 dataset: %d", n)
	}
}

// UpsertDataset 的口径是「按 (datasource, schema, table) 覆盖」：
// 重复采集只更新统计值与列定义，不新增行，也不能换 id。
func TestUpsertDatasetOverwritesSameTable(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")

	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: ds.ID,
		Schema:       "public",
		Name:         "orders",
		Type:         "table",
		Comment:      "订单",
		RowCount:     10,
		SizeBytes:    1024,
		Columns:      []model.ColumnSpec{{Name: "id", Type: "bigint", PrimaryKey: true}},
	}, ws.ID); err != nil {
		t.Fatalf("首次写入表快照失败: %v", err)
	}

	list, err := r.ListDatasets(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("列出表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有一条快照，实际 %d", len(list))
	}
	first := list[0]
	if first.Name != "orders" || first.RowCount != 10 || first.Comment != "订单" {
		t.Fatalf("字段不对: %+v", first)
	}
	if len(first.Columns) != 1 || !first.Columns[0].PrimaryKey {
		t.Fatalf("列定义应往返: %+v", first.Columns)
	}
	if first.PII != "none" || first.PIIReason != "" {
		t.Fatalf("默认敏感级别应为 none: %+v", first)
	}
	if first.Tags == nil || len(first.Tags) != 0 {
		t.Fatalf("默认标签应为空数组而不是 nil: %#v", first.Tags)
	}

	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: ds.ID,
		Schema:       "public",
		Name:         "orders",
		Type:         "view",
		Comment:      "订单视图",
		RowCount:     99,
		SizeBytes:    2048,
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint", PrimaryKey: true},
			{Name: "amount", Type: "numeric"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("覆盖写入表快照失败: %v", err)
	}

	list, err = r.ListDatasets(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("复查表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("覆盖写不应新增行，实际 %d", len(list))
	}
	if list[0].ID != first.ID {
		t.Fatalf("覆盖写应保留原 id: %s != %s", list[0].ID, first.ID)
	}
	if list[0].RowCount != 99 || list[0].Type != "view" || len(list[0].Columns) != 2 {
		t.Fatalf("覆盖写未生效: %+v", list[0])
	}
	if n := queryInt(t, db, `SELECT count(*) FROM dataset`); n != 1 {
		t.Fatalf("物理行数应为 1，实际 %d", n)
	}
}

// ListDatasets 的过滤与排序：按 schema/table 升序，datasource 过滤只保留指定源。
func TestListDatasetsFilterAndOrder(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds1 := seedDatasource(t, r, ws.ID, "pg1")
	ds2 := seedDatasource(t, r, ws.ID, "pg2")

	seed := func(dsID, schema, table string) {
		t.Helper()
		if err := r.UpsertDataset(ctx, model.Dataset{
			DatasourceID: dsID, Schema: schema, Name: table, Type: "table",
		}, ws.ID); err != nil {
			t.Fatalf("写入 %s.%s 失败: %v", schema, table, err)
		}
	}
	seed(ds1.ID, "sales", "orders")
	seed(ds1.ID, "public", "users")
	seed(ds2.ID, "public", "products")

	all, err := r.ListDatasets(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("列出全部表失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("应有 3 张表，实际 %d", len(all))
	}
	wantOrder := []string{"products", "users", "orders"} // public < sales
	for i, name := range wantOrder {
		if all[i].Name != name {
			t.Fatalf("第 %d 条应为 %s，实际 %s", i, name, all[i].Name)
		}
	}

	only, err := r.ListDatasets(ctx, ws.ID, ds2.ID)
	if err != nil {
		t.Fatalf("按数据源过滤失败: %v", err)
	}
	if len(only) != 1 || only[0].Name != "products" {
		t.Fatalf("过滤结果不对: %+v", only)
	}

	empty, err := r.ListDatasets(ctx, ws.ID, "ds_none")
	if err != nil {
		t.Fatalf("无匹配时应成功: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("无匹配时应返回空集合: %+v", empty)
	}
}

// 建模脚手架按表名找列定义，同名表要取最近采集的那一条。
func TestDatasetColumnsByNamePicksLatest(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")

	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: ds.ID, Schema: "old", Name: "orders", Type: "table",
		Columns: []model.ColumnSpec{{Name: "stale", Type: "text"}},
	}, ws.ID); err != nil {
		t.Fatalf("写入旧快照失败: %v", err)
	}
	// 两条同名表分处不同 schema，靠 collected_at 决出「最近」。
	execSQL(t, db, `UPDATE dataset SET collected_at = now() - interval '2 hours' WHERE schema_name='old'`)

	if err := r.UpsertDataset(ctx, model.Dataset{
		DatasourceID: ds.ID, Schema: "new", Name: "orders", Type: "table",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint", PrimaryKey: true},
			{Name: "amount", Type: "numeric"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写入新快照失败: %v", err)
	}

	cols, err := r.DatasetColumnsByName(ctx, ws.ID, "orders")
	if err != nil {
		t.Fatalf("按表名取列失败: %v", err)
	}
	if len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "amount" {
		t.Fatalf("应取最近采集的列定义: %+v", cols)
	}

	if _, err := r.DatasetColumnsByName(ctx, ws.ID, "no_such_table"); err == nil {
		t.Fatal("表不存在时应报错")
	}
}

// 敏感级别回写与「人工覆盖」列表：列表只认 pii_override=true 的行。
func TestDatasetPIIWriteAndOverrideList(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg")

	for _, table := range []string{"users", "orders"} {
		if err := r.UpsertDataset(ctx, model.Dataset{
			DatasourceID: ds.ID, Schema: "public", Name: table, Type: "table",
		}, ws.ID); err != nil {
			t.Fatalf("写入 %s 失败: %v", table, err)
		}
	}

	if err := r.UpdateDatasetPII(ctx, ws.ID, "users", "high", "包含手机号"); err != nil {
		t.Fatalf("回写敏感级别失败: %v", err)
	}
	// 名称不匹配时不应误伤别的表。
	if err := r.UpdateDatasetPII(ctx, ws.ID, "users", "high", "包含手机号"); err != nil {
		t.Fatalf("重复回写失败: %v", err)
	}

	list, err := r.ListDatasets(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("列出表失败: %v", err)
	}
	levels := map[string][2]string{}
	for _, d := range list {
		levels[d.Name] = [2]string{d.PII, d.PIIReason}
	}
	if levels["users"] != [2]string{"high", "包含手机号"} {
		t.Fatalf("users 的敏感级别不对: %+v", levels["users"])
	}
	if levels["orders"] != [2]string{"none", ""} {
		t.Fatalf("orders 不应被改动: %+v", levels["orders"])
	}

	// UpdateDatasetPII 只写级别，不置 pii_override；覆盖列表因此还是空的。
	overrides, err := r.ListPIIOverrides(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("列出人工覆盖失败: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("未标记人工覆盖时列表应为空: %+v", overrides)
	}

	execSQL(t, db, `UPDATE dataset SET pii_override=true WHERE table_name='users'`)
	overrides, err = r.ListPIIOverrides(ctx, ws.ID, "")
	if err != nil {
		t.Fatalf("复查人工覆盖失败: %v", err)
	}
	if len(overrides) != 1 || overrides[0].Table != "users" || overrides[0].Level != "high" ||
		overrides[0].Reason != "包含手机号" {
		t.Fatalf("人工覆盖内容不对: %+v", overrides)
	}

	// 跨工作区隔离。
	other := seedWorkspace(t, r, "other")
	if got, err := r.ListPIIOverrides(ctx, other.ID, ""); err != nil || len(got) != 0 {
		t.Fatalf("其他工作区不应看到覆盖: %v %v", got, err)
	}
}

// 结构变更按时间倒序返回，detail 里拼起来的 "旧 -> 新" 要能拆回 From/To；
// 采集任务记录则要落到正确状态与计数。
func TestSchemaChangesAndCollectJob(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	save := func(c model.SchemaChange) {
		t.Helper()
		if err := r.SaveSchemaChange(ctx, ws.ID, "pg", c); err != nil {
			t.Fatalf("写入结构变更失败: %v", err)
		}
	}
	save(model.SchemaChange{Schema: "public", Table: "orders", Kind: "add_column", Column: "amount", To: "numeric"})
	save(model.SchemaChange{Schema: "public", Table: "orders", Kind: "modify_column", Column: "id", From: "int", To: "bigint", Breaking: true})
	save(model.SchemaChange{Schema: "public", Table: "users", Kind: "drop_table", Breaking: true})

	// 三条同一时刻写入，created_at 可能相同；显式拉开时间才能验证倒序。
	execSQL(t, db, `UPDATE schema_change SET created_at = now() - interval '3 hours' WHERE table_name='orders' AND column_name='amount'`)
	execSQL(t, db, `UPDATE schema_change SET created_at = now() - interval '2 hours' WHERE table_name='orders' AND column_name='id'`)
	execSQL(t, db, `UPDATE schema_change SET created_at = now() - interval '1 hour' WHERE table_name='users'`)

	list, err := r.ListSchemaChanges(ctx, ws.ID, 10)
	if err != nil {
		t.Fatalf("列出结构变更失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 条变更，实际 %d", len(list))
	}
	if list[0].Table != "users" || !list[0].Breaking {
		t.Fatalf("最近一条应是 users 的 drop_table: %+v", list[0])
	}
	if list[1].From != "int" || list[1].To != "bigint" || !list[1].Breaking {
		t.Fatalf("detail 应能拆回 From/To: %+v", list[1])
	}
	if list[1].Datasource != "pg" || list[1].Column != "id" {
		t.Fatalf("变更字段不对: %+v", list[1])
	}
	// 新增列时旧值为空，拆分后 From 应是空串而不是残缺的文本。
	if list[2].From != "" || list[2].To != "numeric" {
		t.Fatalf("空旧值应保持空串: %+v", list[2])
	}

	// limit 生效，且非法 limit 会被规整成默认值（不报错）。
	if got, err := r.ListSchemaChanges(ctx, ws.ID, 2); err != nil || len(got) != 2 {
		t.Fatalf("limit=2 应只返回 2 条: %d %v", len(got), err)
	}
	if got, err := r.ListSchemaChanges(ctx, ws.ID, -1); err != nil || len(got) != 3 {
		t.Fatalf("非法 limit 应回落到默认值: %d %v", len(got), err)
	}
	if got, err := r.ListSchemaChanges(ctx, ws.ID, 10000); err != nil || len(got) != 3 {
		t.Fatalf("超大 limit 应被钳制但仍返回全部: %d %v", len(got), err)
	}

	// 采集任务：成功任务要带 tables_seen/changes/finished_at。
	if err := r.RecordCollectJob(ctx, ws.ID, "pg", "success", 12, 3, ""); err != nil {
		t.Fatalf("记录采集任务失败: %v", err)
	}
	if err := r.RecordCollectJob(ctx, ws.ID, "pg", "failed", 0, 0, "连接超时"); err != nil {
		t.Fatalf("记录失败任务失败: %v", err)
	}
	if n := queryInt(t, db, `SELECT tables_seen FROM collect_job WHERE status='success'`); n != 12 {
		t.Fatalf("tables_seen 应写入: %d", n)
	}
	if n := queryInt(t, db, `SELECT changes FROM collect_job WHERE status='success'`); n != 3 {
		t.Fatalf("changes 应写入: %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM collect_job WHERE finished_at IS NOT NULL`); n != 2 {
		t.Fatalf("两个任务都应有结束时间，实际 %d", n)
	}
	if msg := queryStr(t, db, `SELECT error FROM collect_job WHERE status='failed'`); msg != "连接超时" {
		t.Fatalf("错误信息应落库: %q", msg)
	}

	// 非法状态会被 CHECK 约束挡住。
	if err := r.RecordCollectJob(ctx, ws.ID, "pg", "failed", 0, 0, "x"); err != nil {
		t.Fatalf("记录任务失败: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO collect_job(id,workspace_id,datasource,status) VALUES('cjb_x',$1,'pg','weird')`, ws.ID); err == nil {
		t.Fatal("非法采集状态应被拒绝")
	}
}

// 软删工作区只改名不动数据：审计需要事后还能找到这些记录。
func TestSoftDeleteWorkspaceKeepsRows(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "prod")
	seedDatasource(t, r, ws.ID, "pg")

	if err := r.SoftDeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("软删工作区失败: %v", err)
	}
	got, err := r.WorkspaceByID(ctx, ws.ID)
	if err != nil {
		t.Fatalf("软删后仍应能查到: %v", err)
	}
	if !strings.HasPrefix(got.Name, "prod__deleted_") {
		t.Fatalf("名称应带删除后缀: %q", got.Name)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM datasource WHERE workspace_id=$1`, ws.ID); n != 1 {
		t.Fatalf("软删不应丢数据: %d", n)
	}
}
