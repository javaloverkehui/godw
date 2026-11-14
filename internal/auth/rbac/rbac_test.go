package rbac

import (
	"context"
	"testing"
)

// T0801 验收：viewer 调用发布接口返回 403；跨 workspace 访问返回 404。
func TestViewerCannotWrite(t *testing.T) {
	e := New()
	if e.Enforce(RoleViewer, ResModel, ActWrite) {
		t.Fatal("viewer 不应能写模型")
	}
	if e.Enforce(RoleViewer, ResPipeline, ActRun) {
		t.Fatal("viewer 不应能触发管道")
	}
	if !e.Enforce(RoleViewer, ResModel, ActRead) {
		t.Fatal("viewer 应能读模型")
	}
}

func TestEditorCannotManageMembers(t *testing.T) {
	e := New()
	if e.Enforce(RoleEditor, ResMember, ActWrite) {
		t.Fatal("editor 不应能改成员")
	}
	if !e.Enforce(RoleEditor, ResModel, ActWrite) {
		t.Fatal("editor 应能写模型")
	}
}

func TestAdminAndOwner(t *testing.T) {
	e := New()
	if !e.Enforce(RoleAdmin, ResSettings, ActWrite) {
		t.Fatal("admin 应能改设置")
	}
	if e.Enforce(RoleAdmin, ResWorkspace, ActDelete) {
		t.Fatal("admin 不应能删工作空间（只有 owner 可以）")
	}
	if !e.Enforce(RoleOwner, ResWorkspace, ActDelete) {
		t.Fatal("owner 应能删工作空间")
	}
}

func TestManageImpliesWrite(t *testing.T) {
	e := New()
	// owner 的 settings 只有 manage + write，manage 蕴含 read/write
	if !e.Enforce(RoleOwner, ResSettings, ActRead) {
		t.Fatal("manage 应蕴含 read")
	}
}

func TestUnknownRole(t *testing.T) {
	e := New()
	if e.Enforce("hacker", ResModel, ActRead) {
		t.Fatal("未知角色应拒绝")
	}
	if got := Normalize("hacker"); got != RoleViewer {
		t.Fatalf("未知角色应归一为 viewer，实际 %s", got)
	}
}

func TestResourceGrant(t *testing.T) {
	e := New()
	// viewer 默认不能写 dashboard，但可以单独授权某个看板
	if e.EnforceResource(RoleViewer, ResDashboard, "dash_1", ActWrite) {
		t.Fatal("未授权前应拒绝")
	}
	e.Allow(RoleViewer, ResDashboard, "dash_1", ActWrite)
	if !e.EnforceResource(RoleViewer, ResDashboard, "dash_1", ActWrite) {
		t.Fatal("单资源授权后应允许")
	}
	if e.EnforceResource(RoleViewer, ResDashboard, "dash_2", ActWrite) {
		t.Fatal("授权不应扩散到其它资源")
	}
	e.Revoke(RoleViewer, ResDashboard, "dash_1", ActWrite)
	if e.EnforceResource(RoleViewer, ResDashboard, "dash_1", ActWrite) {
		t.Fatal("撤销后应拒绝")
	}
}

func TestPublicShare(t *testing.T) {
	e := New()
	if !e.Enforce("", "public:share", ActRead) {
		t.Fatal("公开分享应免鉴权读")
	}
	if e.Enforce("", "public:share", ActWrite) {
		t.Fatal("公开分享不应允许写")
	}
}

func TestAtLeast(t *testing.T) {
	if !AtLeast(RoleAdmin, RoleEditor) {
		t.Fatal("admin 应不低于 editor")
	}
	if AtLeast(RoleViewer, RoleRunner) {
		t.Fatal("viewer 低于 runner")
	}
}

func TestPermissionsListing(t *testing.T) {
	e := New()
	perms := e.Permissions(RoleViewer)
	if len(perms[ResModel]) == 0 {
		t.Fatal("viewer 应有 model 读权限")
	}
	if _, ok := perms[ResSettings]; ok {
		t.Fatal("viewer 不应有 settings 权限")
	}
}

func TestContextRole(t *testing.T) {
	ctx := WithRole(context.Background(), "Admin")
	if got := RoleFrom(ctx); got != RoleAdmin {
		t.Fatalf("context 角色归一失败: %s", got)
	}
	if got := RoleFrom(context.Background()); got != "" {
		t.Fatalf("空 context 应返回空，实际 %q", got)
	}
}
