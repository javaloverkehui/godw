package repo_test

import (
	"context"
	"os"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/testsupport"
)

// 仓储层的用例全部对着真实 PostgreSQL 跑：SQL 写错、字段顺序错、约束冲突
// 这些缺陷只有真连库才会暴露。每个用例开始前清空数据，互不影响。
func TestMain(m *testing.M) { os.Exit(testsupport.Main(m)) }

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	db := testsupport.Shared(t)
	testsupport.Reset(t, db)
	return repo.New(db)
}

func TestEnsureWorkspaceIsIdempotent(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	first, err := r.EnsureWorkspace(ctx, "default")
	if err != nil {
		t.Fatalf("首次创建工作空间失败: %v", err)
	}
	if first.ID == "" || first.Name != "default" {
		t.Fatalf("工作空间字段不对: %+v", first)
	}

	second, err := r.EnsureWorkspace(ctx, "default")
	if err != nil {
		t.Fatalf("重复创建工作空间应成功: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("重复创建应返回同一个工作空间: %s != %s", second.ID, first.ID)
	}
}

func TestUserByEmail(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	ws, err := r.EnsureWorkspace(ctx, "default")
	if err != nil {
		t.Fatalf("准备工作空间失败: %v", err)
	}
	u := model.User{
		ID: "usr_1", WorkspaceID: ws.ID, Email: "a@b.c",
		DisplayName: "测试", PasswordHash: "hash", Role: "admin",
	}
	if err := r.CreateUser(ctx, u); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	got, err := r.UserByEmail(ctx, ws.ID, "a@b.c")
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if got.ID != u.ID || got.Role != "admin" || got.DisplayName != "测试" {
		t.Fatalf("用户字段不对: %+v", got)
	}

	// 不存在时必须是 401，而不是把 pgx.ErrNoRows 漏给上层：登录接口依赖这个语义。
	if _, err := r.UserByEmail(ctx, ws.ID, "none@b.c"); err == nil {
		t.Fatal("不存在的用户应返回错误")
	} else if errs.From(err).Code != errs.CodeUnauthorized {
		t.Fatalf("应返回 CodeUnauthorized，实际 %v", errs.From(err).Code)
	}

	// 跨工作空间不可见。
	other, err := r.EnsureWorkspace(ctx, "other")
	if err != nil {
		t.Fatalf("创建第二个工作空间失败: %v", err)
	}
	if _, err := r.UserByEmail(ctx, other.ID, "a@b.c"); err == nil {
		t.Fatal("跨工作空间不应查到用户")
	}
}
