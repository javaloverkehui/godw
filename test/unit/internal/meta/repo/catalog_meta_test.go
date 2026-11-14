package repo_test

import (
	"context"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// 环境是「同一模型在不同环境指向不同数据源」的载体：
// EnsureEnvironment 必须幂等（重复初始化不能报错，也不能覆盖人工写的描述）。
func TestEnvironmentAndBindings(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	ds := seedDatasource(t, r, ws.ID, "pg_prod")
	dsDev := seedDatasource(t, r, ws.ID, "pg_dev")

	if err := r.EnsureEnvironment(ctx, ws.ID, "prod", "生产"); err != nil {
		t.Fatalf("创建环境失败: %v", err)
	}
	// 重复初始化不报错，且不覆盖已有描述（DO NOTHING）。
	if err := r.EnsureEnvironment(ctx, ws.ID, "prod", "另一个描述"); err != nil {
		t.Fatalf("重复创建环境应成功: %v", err)
	}
	if err := r.EnsureEnvironment(ctx, ws.ID, "dev", "开发"); err != nil {
		t.Fatalf("创建第二个环境失败: %v", err)
	}

	envs, err := r.ListEnvironments(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列环境失败: %v", err)
	}
	if len(envs) != 2 || envs[0].Name != "dev" || envs[1].Name != "prod" {
		t.Fatalf("应按名称排序: %+v", envs)
	}
	if envs[1].Description != "生产" {
		t.Fatalf("重复创建不应覆盖描述: %q", envs[1].Description)
	}
	if envs[1].ID == "" || envs[1].CreatedAt.IsZero() || envs[1].WorkspaceID != ws.ID {
		t.Fatalf("环境字段不完整: %+v", envs[1])
	}

	// 绑定：未绑定时返回空串而不是错误。
	if id, err := r.EnvBinding(ctx, ws.ID, "prod", "main"); err != nil || id != "" {
		t.Fatalf("未绑定应返回空串: %q %v", id, err)
	}
	if err := r.BindEnvDatasource(ctx, ws.ID, "prod", "main", ds.ID); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	if err := r.BindEnvDatasource(ctx, ws.ID, "dev", "main", dsDev.ID); err != nil {
		t.Fatalf("dev 绑定失败: %v", err)
	}
	if id, err := r.EnvBinding(ctx, ws.ID, "prod", "main"); err != nil || id != ds.ID {
		t.Fatalf("绑定应可查: %q %v", id, err)
	}
	// 同一 (env, alias) 重新绑定是覆盖，不报唯一键冲突。
	if err := r.BindEnvDatasource(ctx, ws.ID, "prod", "main", dsDev.ID); err != nil {
		t.Fatalf("重新绑定失败: %v", err)
	}
	if id, err := r.EnvBinding(ctx, ws.ID, "prod", "main"); err != nil || id != dsDev.ID {
		t.Fatalf("重新绑定应覆盖: %q %v", id, err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM env_binding WHERE env='prod'`); n != 1 {
		t.Fatalf("重新绑定不应新增行: %d", n)
	}

	// 列表返回「别名 → 数据源名」，供前端直接展示。
	if err := r.BindEnvDatasource(ctx, ws.ID, "prod", "report", ds.ID); err != nil {
		t.Fatalf("绑定 report 失败: %v", err)
	}
	bindings, err := r.ListEnvBindings(ctx, ws.ID, "prod")
	if err != nil {
		t.Fatalf("列绑定失败: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("应有 2 条绑定，实际 %d: %+v", len(bindings), bindings)
	}
	if bindings["main"] != "pg_dev" || bindings["report"] != "pg_prod" {
		t.Fatalf("绑定内容不对: %+v", bindings)
	}

	// 指向不存在数据源的绑定不会出现在列表里（JOIN 掉了），但 EnvBinding 仍能取回 ID。
	execSQL(t, db, `INSERT INTO env_binding(workspace_id,env,alias,datasource_id) VALUES($1,'prod','ghost','ds_ghost')`, ws.ID)
	bindings, err = r.ListEnvBindings(ctx, ws.ID, "prod")
	if err != nil {
		t.Fatalf("复查绑定失败: %v", err)
	}
	if _, ok := bindings["ghost"]; ok {
		t.Fatalf("数据源不存在时不应出现在列表: %+v", bindings)
	}
	if id, _ := r.EnvBinding(ctx, ws.ID, "prod", "ghost"); id != "ds_ghost" {
		t.Fatalf("EnvBinding 不做 JOIN，应原样返回: %q", id)
	}

	// 跨工作区隔离。
	other := seedWorkspace(t, r, "other")
	if id, err := r.EnvBinding(ctx, other.ID, "prod", "main"); err != nil || id != "" {
		t.Fatalf("其他工作区不应看到绑定: %q %v", id, err)
	}
	if envs, err := r.ListEnvironments(ctx, other.ID); err != nil || len(envs) != 0 {
		t.Fatalf("其他工作区不应看到环境: %v %v", envs, err)
	}
}

// 表级血缘是整图渲染的数据源：必须稳定排序，否则前端每次刷新图都在抖。
func TestListLineageOrderAndIsolation(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	execSQL(t, db, `INSERT INTO lineage_edge(workspace_id,upstream,downstream,node_id) VALUES
		($1,'ods_order','dwd_order','n2'),
		($1,'dwd_order','dws_trade',''),
		($1,'ods_user','dwd_order','n1')`, ws.ID)
	execSQL(t, db, `INSERT INTO lineage_edge(workspace_id,upstream,downstream) VALUES($1,'x','y')`, other.ID)

	edges, err := r.ListLineage(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列血缘失败: %v", err)
	}
	if len(edges) != 3 {
		t.Fatalf("应有 3 条边，实际 %d", len(edges))
	}
	want := [][2]string{{"dwd_order", "dws_trade"}, {"ods_order", "dwd_order"}, {"ods_user", "dwd_order"}}
	for i, w := range want {
		if edges[i].Upstream != w[0] || edges[i].Downstream != w[1] {
			t.Fatalf("第 %d 条应是 %v，实际 %+v", i, w, edges[i])
		}
	}
	if edges[1].NodeID != "n2" || edges[0].NodeID != "" {
		t.Fatalf("node_id 应原样返回: %+v", edges)
	}

	if got, err := r.ListLineage(ctx, "ws_none"); err != nil || len(got) != 0 {
		t.Fatalf("空工作区应返回空集合: %v %v", got, err)
	}
}

// RunningInstances 供配额与「当前负载」展示，必须只数未终态的三种状态。
func TestRunningInstancesCountsNonTerminal(t *testing.T) {
	r, db := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")
	p := seedPipeline(t, r, ws.ID, "p1")

	for i, status := range []string{"queued", "running", "waiting", "success", "failed", "canceled"} {
		seedInstance(t, r, model.Instance{
			WorkspaceID: ws.ID, PipelineID: p.ID, PipelineVer: 1,
			BizDate: "2024-01-0" + string(rune('1'+i)), TriggerType: "manual",
			IdempotencyKey: "k" + status, Status: status,
		})
	}
	// 其他工作区的实例不能算进来。
	otherP := seedPipeline(t, r, other.ID, "p1")
	seedInstance(t, r, model.Instance{
		WorkspaceID: other.ID, PipelineID: otherP.ID, PipelineVer: 1,
		BizDate: "2024-01-01", TriggerType: "manual", IdempotencyKey: "k", Status: "running",
	})

	n, err := r.RunningInstances(ctx, ws.ID)
	if err != nil {
		t.Fatalf("统计运行实例失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("只应统计 queued/running/waiting，实际 %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM instance WHERE workspace_id=$1`, ws.ID); n != 6 {
		t.Fatalf("实例应全部落库: %d", n)
	}
}

// 成员表与角色查询：RoleOf 找不到人时返回空串，由上层决定拒绝还是降级。
func TestWorkspaceMembersAndRoleOf(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")

	if got := r.RoleOf(ctx, ws.ID, "usr_1"); got != "" {
		t.Fatalf("未登记时应返回空串: %q", got)
	}
	if err := r.AddMember(ctx, ws.ID, "usr_1", "editor"); err != nil {
		t.Fatalf("添加成员失败: %v", err)
	}
	if err := r.AddMember(ctx, ws.ID, "usr_2", "viewer"); err != nil {
		t.Fatalf("添加第二个成员失败: %v", err)
	}
	// 重复添加同一个人是改角色，不是报错。
	if err := r.AddMember(ctx, ws.ID, "usr_1", "admin"); err != nil {
		t.Fatalf("更新成员角色失败: %v", err)
	}
	if got := r.RoleOf(ctx, ws.ID, "usr_1"); got != "admin" {
		t.Fatalf("角色应被更新: %q", got)
	}

	members, err := r.ListMembers(ctx, ws.ID)
	if err != nil {
		t.Fatalf("列成员失败: %v", err)
	}
	if len(members) != 2 || members[0].UserID != "usr_1" || members[1].UserID != "usr_2" {
		t.Fatalf("应按 user_id 排序: %+v", members)
	}
	if members[0].Role != "admin" || members[0].WorkspaceID != ws.ID || members[0].CreatedAt.IsZero() {
		t.Fatalf("成员字段不对: %+v", members[0])
	}

	if err := r.RemoveMember(ctx, ws.ID, "usr_2"); err != nil {
		t.Fatalf("移除成员失败: %v", err)
	}
	if members, err := r.ListMembers(ctx, ws.ID); err != nil || len(members) != 1 {
		t.Fatalf("移除后应剩 1 人: %v %v", members, err)
	}
	if got := r.RoleOf(ctx, ws.ID, "usr_2"); got != "" {
		t.Fatalf("移除后角色应为空: %q", got)
	}
}

// WorkspaceByID / CreateWorkspace 的错误语义：重名是冲突，不存在是 404。
func TestWorkspaceLookupErrors(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()

	ws, err := r.CreateWorkspace(ctx, "alpha", "描述")
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	got, err := r.WorkspaceByID(ctx, ws.ID)
	if err != nil {
		t.Fatalf("按 ID 取工作区失败: %v", err)
	}
	if got.Name != "alpha" || got.Description != "描述" {
		t.Fatalf("工作区字段不对: %+v", got)
	}
	if _, err := r.WorkspaceByID(ctx, "ws_none"); err == nil {
		t.Fatal("不存在的工作区应报错")
	} else if errs.From(err).Code != errs.CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 %v", errs.From(err).Code)
	}
	if _, err := r.CreateWorkspace(ctx, "alpha", "重复"); err == nil {
		t.Fatal("重名工作区应报错")
	} else if errs.From(err).Code != errs.CodeConflict {
		t.Fatalf("应返回 CodeConflict，实际 %v", errs.From(err).Code)
	}
}

// 多租户可见性：admin 看全部，其他人只看自己是成员的工作区。
func TestListWorkspacesFor(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()

	a := seedWorkspace(t, r, "a")
	b := seedWorkspace(t, r, "b")
	seedWorkspace(t, r, "c")

	if err := r.AddMember(ctx, a.ID, "usr_1", "editor"); err != nil {
		t.Fatalf("加入 a 失败: %v", err)
	}
	if err := r.AddMember(ctx, b.ID, "usr_2", "viewer"); err != nil {
		t.Fatalf("加入 b 失败: %v", err)
	}

	admin, err := r.ListWorkspacesFor(ctx, "usr_1", "admin")
	if err != nil {
		t.Fatalf("admin 列工作区失败: %v", err)
	}
	if len(admin) != 3 {
		t.Fatalf("admin 应看到全部 3 个工作区，实际 %d", len(admin))
	}

	mine, err := r.ListWorkspacesFor(ctx, "usr_1", "editor")
	if err != nil {
		t.Fatalf("普通用户列工作区失败: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("普通用户只应看到自己加入的工作区: %+v", mine)
	}

	none, err := r.ListWorkspacesFor(ctx, "usr_none", "viewer")
	if err != nil {
		t.Fatalf("无成员关系时应成功: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("无成员关系应返回空: %+v", none)
	}
}

// TenantUsage 是配额与成本核算的输入，六个计数都要对得上。
func TestTenantUsageCounts(t *testing.T) {
	r, _ := newRepoDB(t)
	ctx := context.Background()
	ws := seedWorkspace(t, r, "ws")
	other := seedWorkspace(t, r, "other")

	seedDatasource(t, r, ws.ID, "pg1")
	seedDatasource(t, r, ws.ID, "pg2")
	if _, err := r.SaveModel(ctx, ws.ID, model.ModelSpec{Kind: "model", Name: "m1", Layer: "ods"}, "", "", "t", true); err != nil {
		t.Fatalf("保存模型失败: %v", err)
	}
	p := seedPipeline(t, r, ws.ID, "p1")
	seedInstance(t, r, model.Instance{
		WorkspaceID: ws.ID, PipelineID: p.ID, PipelineVer: 1, BizDate: "2024-01-01",
		TriggerType: "manual", IdempotencyKey: "k1",
	})

	// 其他工作区的数据不能算进来。
	seedDatasource(t, r, other.ID, "pg3")

	usage, err := r.TenantUsage(ctx, ws.ID)
	if err != nil {
		t.Fatalf("统计用量失败: %v", err)
	}
	if usage["datasources"] != int64(2) {
		t.Fatalf("数据源计数不对: %v", usage["datasources"])
	}
	if usage["models"] != int64(1) || usage["pipelines"] != int64(1) || usage["instances"] != int64(1) {
		t.Fatalf("资源计数不对: %+v", usage)
	}
	if usage["runsLast30d"] != int64(1) {
		t.Fatalf("近 30 天运行数不对: %v", usage["runsLast30d"])
	}
	if usage["modelSpecBytes"].(int64) <= 0 {
		t.Fatalf("模型快照字节数应大于 0: %v", usage["modelSpecBytes"])
	}

	empty, err := r.TenantUsage(ctx, "ws_none")
	if err != nil {
		t.Fatalf("空工作区统计应成功: %v", err)
	}
	if empty["datasources"] != int64(0) || empty["modelSpecBytes"] != int64(0) {
		t.Fatalf("空工作区应全为 0: %+v", empty)
	}
}
