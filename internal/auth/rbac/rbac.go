// Package rbac 实现资源级权限控制。
//
// 设计取舍：没有引入 Casbin。原因：
//   - Casbin 的 model 文件对「5 类资源 × 5 个角色 × 4 个动作」这种小矩阵是过度设计；
//   - 权限判定必须能在中间件里 O(1) 完成，用内存矩阵比解析策略表达式更快；
//   - 真正的难点在「资源归属校验」而不是策略语言，那部分本就必须写代码。
//
// 但保留了 Casbin 的核心语义：sub, dom(workspace), obj(resource), act。
// 因此未来要换 Casbin，只需替换 Enforce 的实现。
package rbac

import (
	"context"
	"strings"
	"sync"
)

// 内置角色。按权限从高到低。
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleRunner = "runner"
	RoleViewer = "viewer"
)

// Resource 是资源类型。
const (
	ResWorkspace  = "workspace"
	ResDatasource = "datasource"
	ResModel      = "model"
	ResPipeline   = "pipeline"
	ResDashboard  = "dashboard"
	ResDataset    = "dataset"
	ResChart      = "chart"
	ResMember     = "member"
	ResAlert      = "alert"
	ResSettings   = "settings"
)

// Action 是动作。
const (
	ActRead   = "read"
	ActWrite  = "write"
	ActDelete = "delete"
	ActRun    = "run"
	ActManage = "manage"
)

// Roles 返回全部角色，按权限降序。
func Roles() []string { return []string{RoleOwner, RoleAdmin, RoleEditor, RoleRunner, RoleViewer} }

// roleRank 用于「角色继承」：高等级角色自动拥有低等级角色的权限。
var roleRank = map[string]int{
	RoleOwner: 5, RoleAdmin: 4, RoleEditor: 3, RoleRunner: 2, RoleViewer: 1,
}

// grants 是角色 → 资源 → 动作集合。
// 这里只定义「默认授予」，单资源授权通过 resource_grant 表补充。
var grants = map[string]map[string]map[string]bool{
	RoleOwner: {
		ResWorkspace:  {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResDatasource: {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResModel:      {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResPipeline:   {ActRead: true, ActWrite: true, ActDelete: true, ActRun: true, ActManage: true},
		ResDashboard:  {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResDataset:    {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResChart:      {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResMember:     {ActRead: true, ActWrite: true, ActDelete: true, ActManage: true},
		ResAlert:      {ActRead: true, ActWrite: true, ActManage: true},
		ResSettings:   {ActRead: true, ActWrite: true, ActManage: true},
	},
	RoleAdmin: {
		ResWorkspace:  {ActRead: true, ActWrite: true, ActManage: true},
		ResDatasource: {ActRead: true, ActWrite: true, ActDelete: true},
		ResModel:      {ActRead: true, ActWrite: true, ActDelete: true},
		ResPipeline:   {ActRead: true, ActWrite: true, ActDelete: true, ActRun: true},
		ResDashboard:  {ActRead: true, ActWrite: true, ActDelete: true},
		ResDataset:    {ActRead: true, ActWrite: true, ActDelete: true},
		ResChart:      {ActRead: true, ActWrite: true, ActDelete: true},
		ResMember:     {ActRead: true, ActWrite: true},
		ResAlert:      {ActRead: true, ActWrite: true},
		ResSettings:   {ActRead: true, ActWrite: true},
	},
	RoleEditor: {
		ResWorkspace:  {ActRead: true},
		ResDatasource: {ActRead: true},
		ResModel:      {ActRead: true, ActWrite: true},
		ResPipeline:   {ActRead: true, ActWrite: true, ActRun: true},
		ResDashboard:  {ActRead: true, ActWrite: true},
		ResDataset:    {ActRead: true, ActWrite: true},
		ResChart:      {ActRead: true, ActWrite: true},
		ResAlert:      {ActRead: true},
		ResSettings:   {ActRead: true},
	},
	RoleRunner: {
		ResWorkspace:  {ActRead: true},
		ResDatasource: {ActRead: true},
		ResModel:      {ActRead: true},
		ResPipeline:   {ActRead: true, ActRun: true},
		ResDashboard:  {ActRead: true},
		ResDataset:    {ActRead: true},
		ResChart:      {ActRead: true},
		ResAlert:      {ActRead: true},
	},
	RoleViewer: {
		ResWorkspace:  {ActRead: true},
		ResDatasource: {ActRead: true},
		ResModel:      {ActRead: true},
		ResPipeline:   {ActRead: true},
		ResDashboard:  {ActRead: true},
		ResDataset:    {ActRead: true},
		ResChart:      {ActRead: true},
		ResAlert:      {ActRead: true},
	},
}

// Enforcer 判定权限。Grants 提供单资源授权（可选）。
type Enforcer struct {
	mu sync.RWMutex
	// Grants 是「角色 + 资源类型 + 资源 ID + 动作」的额外授权，key 为拼接串。
	Grants map[string]bool
}

// New 创建 enforcer。
func New() *Enforcer { return &Enforcer{Grants: map[string]bool{}} }

func grantKey(role, resource, resourceID, action string) string {
	return role + "\x00" + resource + "\x00" + resourceID + "\x00" + action
}

// Allow 授予单资源权限。
func (e *Enforcer) Allow(role, resource, resourceID, action string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Grants == nil {
		e.Grants = map[string]bool{}
	}
	e.Grants[grantKey(role, resource, resourceID, action)] = true
}

// Revoke 撤销单资源权限。
func (e *Enforcer) Revoke(role, resource, resourceID, action string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.Grants, grantKey(role, resource, resourceID, action))
}

// Enforce 判定是否允许。
// 规则：角色的默认授予优先；未授予时再看单资源授权；都没有则拒绝。
func (e *Enforcer) Enforce(role, resource, action string) bool {
	if action == ActRead && strings.HasPrefix(resource, "public:") {
		// 公开分享的只读资源不鉴权，由 token 本身控制可见性。
		return true
	}
	g, ok := grants[role]
	if !ok {
		return false
	}
	acts, ok := g[resource]
	if !ok {
		return false
	}
	if acts[action] {
		return true
	}
	// manage 蕴含除 delete 外的所有动作
	if acts[ActManage] && action != ActDelete {
		return true
	}
	return false
}

// EnforceResource 带资源 ID 的判定：先看默认授予，再看单资源授权。
func (e *Enforcer) EnforceResource(role, resource, resourceID, action string) bool {
	if e.Enforce(role, resource, action) {
		return true
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Grants[grantKey(role, resource, resourceID, action)]
}

// Permissions 返回某角色的全部权限，供前端渲染菜单与按钮。
func (e *Enforcer) Permissions(role string) map[string][]string {
	out := map[string][]string{}
	for res, acts := range grants[role] {
		list := make([]string, 0, len(acts))
		for a, ok := range acts {
			if ok {
				list = append(list, a)
			}
		}
		out[res] = list
	}
	return out
}

// AtLeast 判断角色等级是否不低于要求。
func AtLeast(role, required string) bool {
	return roleRank[role] >= roleRank[required]
}

// Normalize 把历史角色名归一。老数据里 admin 与 owner 混用，统一到新集合。
func Normalize(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	switch r {
	case "owner", "admin", "editor", "runner", "viewer":
		return r
	case "member", "user", "":
		return RoleViewer
	}
	return RoleViewer
}

// type ctxKey 避免与其它包的 context key 冲突。
type ctxKey struct{}

// WithRole 把角色放进 context，中间件注入。
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, ctxKey{}, Normalize(role))
}

// RoleFrom 从 context 取角色。
func RoleFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
