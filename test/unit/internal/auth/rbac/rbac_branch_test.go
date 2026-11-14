package rbac_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/godw/godw/internal/auth/rbac"
)

// 守住角色清单顺序：前端按此顺序渲染角色选择，权限必须从高到低。
func TestRolesOrder(t *testing.T) {
	want := []string{rbac.RoleOwner, rbac.RoleAdmin, rbac.RoleEditor, rbac.RoleRunner, rbac.RoleViewer}
	if got := rbac.Roles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("角色顺序错误: %v", got)
	}
}

// 守住 5×资源×动作矩阵：表驱动穷举，任何一格被误改都能立刻暴露。
func TestEnforceMatrix(t *testing.T) {
	type row struct {
		role     string
		resource string
		action   string
		want     bool
	}
	cases := []row{
		// owner 全量
		{rbac.RoleOwner, rbac.ResPipeline, rbac.ActManage, true},
		{rbac.RoleOwner, rbac.ResSettings, rbac.ActDelete, false}, // manage 不蕴含 delete
		{rbac.RoleOwner, rbac.ResSettings, rbac.ActRun, true},     // manage 蕴含除 delete 外的动作
		{rbac.RoleOwner, rbac.ResAlert, rbac.ActDelete, false},
		// admin 边界
		{rbac.RoleAdmin, rbac.ResWorkspace, rbac.ActManage, true},
		{rbac.RoleAdmin, rbac.ResWorkspace, rbac.ActDelete, false},
		{rbac.RoleAdmin, rbac.ResMember, rbac.ActWrite, true},
		{rbac.RoleAdmin, rbac.ResMember, rbac.ActDelete, false},
		{rbac.RoleAdmin, rbac.ResSettings, rbac.ActManage, false}, // admin 只有 settings 读写
		// editor
		{rbac.RoleEditor, rbac.ResModel, rbac.ActWrite, true},
		{rbac.RoleEditor, rbac.ResDatasource, rbac.ActWrite, false},
		{rbac.RoleEditor, rbac.ResPipeline, rbac.ActRun, true},
		{rbac.RoleEditor, rbac.ResMember, rbac.ActRead, false}, // editor 无 member 权限
		// runner
		{rbac.RoleRunner, rbac.ResPipeline, rbac.ActRun, true},
		{rbac.RoleRunner, rbac.ResPipeline, rbac.ActWrite, false},
		// viewer
		{rbac.RoleViewer, rbac.ResChart, rbac.ActRead, true},
		{rbac.RoleViewer, rbac.ResChart, rbac.ActWrite, false},
		// 未知资源与未知动作
		{rbac.RoleOwner, "unknown", rbac.ActRead, false},
		{rbac.RoleViewer, rbac.ResModel, "publish", false},
		// 未知角色
		{"root", rbac.ResModel, rbac.ActRead, false},
	}
	e := rbac.New()
	for _, c := range cases {
		if got := e.Enforce(c.role, c.resource, c.action); got != c.want {
			t.Errorf("Enforce(%s,%s,%s) = %v，期望 %v", c.role, c.resource, c.action, got, c.want)
		}
	}
}

// 守住公开分享免鉴权：只读放行、写操作仍拒绝，避免公开链接变成写入口。
func TestPublicShareOnlyRead(t *testing.T) {
	e := rbac.New()
	if !e.Enforce("", "public:dashboard:abc", rbac.ActRead) {
		t.Fatal("公开资源应允许匿名读")
	}
	if e.Enforce("", "public:dashboard:abc", rbac.ActWrite) {
		t.Fatal("公开资源不应允许写")
	}
	if e.Enforce("", "public:dashboard:abc", rbac.ActRun) {
		t.Fatal("公开资源不应允许触发")
	}
	// 非 public: 前缀不享受豁免。
	if e.Enforce("", "dashboard:abc", rbac.ActRead) {
		t.Fatal("非公开资源不应免鉴权")
	}
}

// 守住单资源授权的隔离性：只对「角色+资源类型+ID+动作」四元组生效。
func TestResourceGrantIsolation(t *testing.T) {
	e := rbac.New()
	// 默认授予优先命中时不查 Grants（快路径）。
	if !e.EnforceResource(rbac.RoleOwner, rbac.ResModel, "m1", rbac.ActRead) {
		t.Fatal("owner 读模型应直接按默认授予放行")
	}
	e.Allow(rbac.RoleViewer, rbac.ResDashboard, "dash_1", rbac.ActWrite)

	if !e.EnforceResource(rbac.RoleViewer, rbac.ResDashboard, "dash_1", rbac.ActWrite) {
		t.Fatal("授权后应允许")
	}
	checks := []struct {
		name     string
		role     string
		resource string
		id       string
		action   string
	}{
		{"其它资源 ID", rbac.RoleViewer, rbac.ResDashboard, "dash_2", rbac.ActWrite},
		{"其它角色", rbac.RoleRunner, rbac.ResDashboard, "dash_1", rbac.ActWrite},
		{"其它资源类型", rbac.RoleViewer, rbac.ResChart, "dash_1", rbac.ActWrite},
		{"其它动作", rbac.RoleViewer, rbac.ResDashboard, "dash_1", rbac.ActDelete},
	}
	for _, c := range checks {
		if e.EnforceResource(c.role, c.resource, c.id, c.action) {
			t.Errorf("%s 不应被授权", c.name)
		}
	}

	// 撤销后立即失效。
	e.Revoke(rbac.RoleViewer, rbac.ResDashboard, "dash_1", rbac.ActWrite)
	if e.EnforceResource(rbac.RoleViewer, rbac.ResDashboard, "dash_1", rbac.ActWrite) {
		t.Fatal("撤销后应拒绝")
	}
	// 撤销不存在的授权不应 panic。
	e.Revoke(rbac.RoleViewer, rbac.ResDashboard, "never", rbac.ActWrite)
}

// 守住零值可用：Enforcer 零值（Grants 为 nil）调用 Allow 必须自动初始化。
func TestZeroValueEnforcer(t *testing.T) {
	var e rbac.Enforcer
	if e.EnforceResource(rbac.RoleViewer, rbac.ResChart, "c1", rbac.ActWrite) {
		t.Fatal("零值 enforcer 默认应拒绝")
	}
	e.Allow(rbac.RoleViewer, rbac.ResChart, "c1", rbac.ActWrite)
	if e.Grants == nil {
		t.Fatal("Allow 应初始化 Grants map")
	}
	if !e.EnforceResource(rbac.RoleViewer, rbac.ResChart, "c1", rbac.ActWrite) {
		t.Fatal("授权后应允许")
	}
}

// 守住 Permissions 输出：只含角色实际拥有的资源，动作列表与 Enforce 一致。
func TestPermissions(t *testing.T) {
	e := rbac.New()
	p := e.Permissions(rbac.RoleViewer)
	// viewer 能读全部 8 类资源，但不应出现 settings/member。
	if len(p) != 8 {
		t.Fatalf("viewer 资源数 = %d，期望 8: %#v", len(p), p)
	}
	if _, ok := p[rbac.ResSettings]; ok {
		t.Fatal("viewer 不应有 settings")
	}
	for _, acts := range p {
		if len(acts) != 1 || acts[0] != rbac.ActRead {
			t.Fatalf("viewer 每类资源应只有 read: %#v", p)
		}
	}
	// owner 的 settings 有 read/write/manage 三个动作。
	owner := e.Permissions(rbac.RoleOwner)
	if len(owner[rbac.ResSettings]) != 3 {
		t.Fatalf("owner.settings 动作数 = %d，期望 3: %#v", len(owner[rbac.ResSettings]), owner[rbac.ResSettings])
	}
	// 未知角色返回空 map 而不是 nil。
	if got := e.Permissions("root"); got == nil || len(got) != 0 {
		t.Fatalf("未知角色应返回空 map: %#v", got)
	}
}

// 守住角色等级比较：未知角色 rank 为 0，不应被当成最高权限。
func TestAtLeast(t *testing.T) {
	cases := []struct {
		role, required string
		want           bool
	}{
		{rbac.RoleOwner, rbac.RoleViewer, true},
		{rbac.RoleOwner, rbac.RoleOwner, true},
		{rbac.RoleAdmin, rbac.RoleOwner, false},
		{rbac.RoleRunner, rbac.RoleRunner, true},
		{rbac.RoleViewer, rbac.RoleRunner, false},
		{"root", rbac.RoleViewer, false},
		{"", "", true}, // 未知对未知：0 >= 0
	}
	for _, c := range cases {
		if got := rbac.AtLeast(c.role, c.required); got != c.want {
			t.Errorf("AtLeast(%q,%q) = %v，期望 %v", c.role, c.required, got, c.want)
		}
	}
}

// 守住历史角色归一：大小写、空白、旧名都要落到五个内置角色之一。
func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"owner":        rbac.RoleOwner,
		" ADMIN ":      rbac.RoleAdmin,
		"Editor":       rbac.RoleEditor,
		"runner":       rbac.RoleRunner,
		"viewer":       rbac.RoleViewer,
		"member":       rbac.RoleViewer,
		"USER":         rbac.RoleViewer,
		"":             rbac.RoleViewer,
		"super-hacker": rbac.RoleViewer,
	}
	for in, want := range cases {
		if got := rbac.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 守住 context 传递：注入时归一化，缺少 key 时返回空串而不是 panic。
func TestContextRole(t *testing.T) {
	ctx := rbac.WithRole(context.Background(), " Admin ")
	if got := rbac.RoleFrom(ctx); got != rbac.RoleAdmin {
		t.Fatalf("context 角色归一失败: %q", got)
	}
	if got := rbac.RoleFrom(context.Background()); got != "" {
		t.Fatalf("空 context 应返回空串，实际 %q", got)
	}
	// 其它包塞入的非字符串值不应被误认。
	ctx2 := context.WithValue(context.Background(), struct{ k int }{1}, 123)
	if got := rbac.RoleFrom(ctx2); got != "" {
		t.Fatalf("无关 key 应返回空串，实际 %q", got)
	}
}
