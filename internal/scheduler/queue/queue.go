// Package queue 实现「分发 + 领取」两段式队列。
//
// 设计取舍：没有引入 Redis 或 MQ。原因有三：
//  1. 数据仓库的任务速率是「每分钟几十个」，PG 的 SKIP LOCKED 完全够用；
//  2. 引入 MQ 意味着多一个必须高可用的组件，运维成本远大于收益；
//  3. 队列状态与实例状态同库，天然一致，不需要「消息投递成功但状态没改」的对账。
//
// 分发（READY→QUEUED）与领取（QUEUED→RUNNING）必须解耦：
// 分发只做「资源组还有没有空位」的判断，领取才真正占用执行槽。
// 这样 worker 崩溃时，QUEUED 的实例可以被任何存活 worker 捡走，不会卡死。
package queue

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/godw/godw/internal/pkg/errs"
)

// Group 是资源组。零值表示不限。
type Group struct {
	Name           string   `json:"name"`
	MaxConcurrency int      `json:"maxConcurrency"`
	MaxTimeoutSec  int      `json:"maxTimeoutSec"`
	MaxPool        int      `json:"maxPool"`
	Engines        []string `json:"engines,omitempty"`
}

// DefaultGroup 是未显式配置时的兜底：4 并发，1 小时超时。
func DefaultGroup() Group {
	return Group{Name: "default", MaxConcurrency: 4, MaxTimeoutSec: 3600, MaxPool: 10}
}

// Item 是队列中的一条待执行记录。
type Item struct {
	InstanceID    string `json:"instanceId"`
	Pipeline      string `json:"pipeline"`
	ResourceGroup string `json:"resourceGroup"`
	Priority      int    `json:"priority"`
	QueuedAt      int64  `json:"queuedAt"`
}

// Store 是队列的存储抽象。生产实现走 PG；测试用内存实现。
type Store interface {
	// RunningCount 返回某资源组当前 running 的实例数。
	RunningCount(ctx context.Context, workspaceID, group string) (int, error)
	// Promote 把满足条件的 READY 实例标记为 QUEUED。返回提升数量。
	Promote(ctx context.Context, workspaceID, group string, limit int) (int, error)
	// Pending 返回按优先级排序的排队实例，用于展示与调试。
	Pending(ctx context.Context, workspaceID string, limit int) ([]Item, error)
}

// Dispatcher 负责把排队实例按资源组空位「提升」到可领取状态。
type Dispatcher struct {
	Store Store
	// Groups 是资源组配置，key 为组名。
	Groups map[string]Group
	mu     sync.Mutex
}

// NewDispatcher 构造分发器；未配置的组自动补默认值。
func NewDispatcher(store Store, groups []Group) *Dispatcher {
	m := map[string]Group{}
	for _, g := range groups {
		if g.MaxConcurrency <= 0 {
			g.MaxConcurrency = 4
		}
		m[g.Name] = g
	}
	if _, ok := m["default"]; !ok {
		m["default"] = DefaultGroup()
	}
	return &Dispatcher{Store: store, Groups: m}
}

// Dispatch 为每个资源组分发任务。返回本次提升的总数。
//
// 关键约束：只提升到「并发上限 - 当前运行数」为止。
// 超过上限时任务留在排队态，而不是先领走再等待——后者会占着 worker 空转。
func (d *Dispatcher) Dispatch(ctx context.Context, workspaceID string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	total := 0
	for name, g := range d.Groups {
		running, err := d.Store.RunningCount(ctx, workspaceID, name)
		if err != nil {
			return total, err
		}
		free := g.MaxConcurrency - running
		if free <= 0 {
			continue
		}
		n, err := d.Store.Promote(ctx, workspaceID, name, free)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// ValidateGroup 校验资源组配置。
func ValidateGroup(g Group) error {
	if g.Name == "" {
		return errs.New(errs.CodeInvalidArgument, "资源组必须有名字")
	}
	if g.MaxConcurrency <= 0 || g.MaxConcurrency > 128 {
		return errs.New(errs.CodeInvalidArgument,
			fmt.Sprintf("资源组 %s 的 max_concurrency 必须在 1..128，实际 %d", g.Name, g.MaxConcurrency))
	}
	if g.MaxTimeoutSec < 0 {
		return errs.New(errs.CodeInvalidArgument, "max_timeout_sec 不能为负")
	}
	return nil
}

// SortPending 按「优先级升序 → 入队时间升序」排序。
// 单独抽出来是为了让前端展示顺序与出队顺序完全一致，避免「看到的和跑的不一样」。
func SortPending(items []Item) []Item {
	out := make([]Item, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].QueuedAt < out[j].QueuedAt
	})
	return out
}

// ClampPriority 把优先级限制在 1..9，1 最高。0 视为默认 5。
func ClampPriority(p int) int {
	if p == 0 {
		return 5
	}
	if p < 1 {
		return 1
	}
	if p > 9 {
		return 9
	}
	return p
}
