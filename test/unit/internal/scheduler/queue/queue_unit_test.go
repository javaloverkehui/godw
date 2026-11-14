package queue_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/scheduler/queue"
)

// fakeStore 记录 Promote 的 limit，并可注入错误。
type fakeStore struct {
	running    map[string]int
	promoteN   map[string]int
	limits     map[string]int
	runningErr error
	promoteErr error
	calls      int
}

func (f *fakeStore) RunningCount(ctx context.Context, workspaceID, group string) (int, error) {
	if f.runningErr != nil {
		return 0, f.runningErr
	}
	return f.running[group], nil
}

func (f *fakeStore) Promote(ctx context.Context, workspaceID, group string, limit int) (int, error) {
	f.calls++
	if f.promoteErr != nil {
		return 0, f.promoteErr
	}
	if f.limits == nil {
		f.limits = map[string]int{}
	}
	f.limits[group] = limit
	return f.promoteN[group], nil
}

func (f *fakeStore) Pending(ctx context.Context, workspaceID string, limit int) ([]queue.Item, error) {
	return nil, nil
}

// 守住默认组兜底：未配置 default 时必须自动补 4 并发/1 小时超时。
func TestNewDispatcherDefaults(t *testing.T) {
	d := queue.NewDispatcher(&fakeStore{}, nil)
	def, ok := d.Groups["default"]
	if !ok {
		t.Fatal("应自动补 default 组")
	}
	if def.MaxConcurrency != 4 || def.MaxTimeoutSec != 3600 || def.MaxPool != 10 || def.Name != "default" {
		t.Fatalf("default 组默认值错误: %+v", def)
	}

	d2 := queue.NewDispatcher(&fakeStore{}, []queue.Group{
		{Name: "etl", MaxConcurrency: 0},
		{Name: "default", MaxConcurrency: 2},
	})
	if got := d2.Groups["etl"].MaxConcurrency; got != 4 {
		t.Fatalf("未配置并发应回退 4，实际 %d", got)
	}
	if got := d2.Groups["default"].MaxConcurrency; got != 2 {
		t.Fatalf("显式配置不应被覆盖，实际 %d", got)
	}
}

// 守住分发上限：只按「并发上限 - 运行数」提升，满了就完全跳过。
func TestDispatchRespectsFreeSlots(t *testing.T) {
	store := &fakeStore{
		running:  map[string]int{"default": 1, "etl": 2},
		promoteN: map[string]int{"default": 3},
	}
	d := queue.NewDispatcher(store, []queue.Group{
		{Name: "default", MaxConcurrency: 4},
		{Name: "etl", MaxConcurrency: 2},
	})
	total, err := d.Dispatch(context.Background(), "ws_1")
	if err != nil {
		t.Fatalf("分发失败: %v", err)
	}
	if total != 3 {
		t.Fatalf("提升总数 = %d，期望 3", total)
	}
	if store.limits["default"] != 3 {
		t.Fatalf("default 提升上限 = %d，期望 3", store.limits["default"])
	}
	if _, ok := store.limits["etl"]; ok {
		t.Fatal("etl 已满（2/2）不应调用 Promote")
	}
}

// 守住查询运行数失败：必须立刻返回错误，不能继续盲目提升。
func TestDispatchRunningCountError(t *testing.T) {
	boom := errors.New("db down")
	store := &fakeStore{runningErr: boom}
	d := queue.NewDispatcher(store, []queue.Group{{Name: "default", MaxConcurrency: 4}})
	total, err := d.Dispatch(context.Background(), "ws_1")
	if !errors.Is(err, boom) {
		t.Fatalf("应返回查询错误，实际 %v", err)
	}
	if total != 0 {
		t.Fatalf("失败时不应有提升计数，实际 %d", total)
	}
	if store.calls != 0 {
		t.Fatal("查询失败后不应调用 Promote")
	}
}

// 守住提升失败：错误向上传递，已提升的数量仍要返回，便于上报告。
func TestDispatchPromoteError(t *testing.T) {
	boom := errors.New("promote failed")
	store := &fakeStore{running: map[string]int{}, promoteErr: boom}
	d := queue.NewDispatcher(store, []queue.Group{{Name: "default", MaxConcurrency: 4}})
	if _, err := d.Dispatch(context.Background(), "ws_1"); !errors.Is(err, boom) {
		t.Fatalf("应返回提升错误，实际 %v", err)
	}
}

// 守住资源组校验：名字、并发上下限、超时非负三条规则都要拦。
func TestValidateGroup(t *testing.T) {
	cases := []struct {
		name    string
		g       queue.Group
		wantErr bool
		wantMsg string
	}{
		{"合法", queue.Group{Name: "default", MaxConcurrency: 4}, false, ""},
		{"边界 1", queue.Group{Name: "g", MaxConcurrency: 1}, false, ""},
		{"边界 128", queue.Group{Name: "g", MaxConcurrency: 128}, false, ""},
		{"无名字", queue.Group{MaxConcurrency: 4}, true, "必须有名字"},
		{"并发为 0", queue.Group{Name: "g", MaxConcurrency: 0}, true, "max_concurrency"},
		{"并发为负", queue.Group{Name: "g", MaxConcurrency: -1}, true, "max_concurrency"},
		{"并发超上限", queue.Group{Name: "g", MaxConcurrency: 129}, true, "max_concurrency"},
		{"超时为负", queue.Group{Name: "g", MaxConcurrency: 1, MaxTimeoutSec: -1}, true, "不能为负"},
		{"超时为 0 合法（表示不限）", queue.Group{Name: "g", MaxConcurrency: 1, MaxTimeoutSec: 0}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := queue.ValidateGroup(c.g)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v，期望错误=%v", err, c.wantErr)
			}
			if err != nil && c.wantMsg != "" {
				if !strings.Contains(err.Error(), c.wantMsg) {
					t.Fatalf("错误信息 %q 缺少 %q", err.Error(), c.wantMsg)
				}
				var e *errs.AppError
				if !errors.As(err, &e) || e.Code != errs.CodeInvalidArgument {
					t.Fatalf("错误码应为 invalid_argument: %v", err)
				}
			}
		})
	}
}

// 守住排序契约：优先级升序、同优先级按入队时间升序，且不修改入参。
func TestSortPending(t *testing.T) {
	in := []queue.Item{
		{InstanceID: "a", Priority: 5, QueuedAt: 100},
		{InstanceID: "b", Priority: 1, QueuedAt: 300},
		{InstanceID: "c", Priority: 5, QueuedAt: 50},
		{InstanceID: "d", Priority: 1, QueuedAt: 200},
	}
	snapshot := make([]queue.Item, len(in))
	copy(snapshot, in)

	out := queue.SortPending(in)
	got := []string{out[0].InstanceID, out[1].InstanceID, out[2].InstanceID, out[3].InstanceID}
	want := []string{"d", "b", "c", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("排序 = %v，期望 %v", got, want)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("SortPending 不应修改入参: %+v", in)
	}
	// 空切片不应 panic。
	if out := queue.SortPending(nil); len(out) != 0 {
		t.Fatalf("nil 应返回空切片: %#v", out)
	}
}

// 守住优先级归一：0→5（默认）、负数→1、超上限→9、正常值原样。
func TestClampPriority(t *testing.T) {
	cases := map[int]int{
		0:  5,
		-3: 1,
		1:  1,
		5:  5,
		9:  9,
		10: 9,
		99: 9,
	}
	for in, want := range cases {
		if got := queue.ClampPriority(in); got != want {
			t.Errorf("ClampPriority(%d) = %d，期望 %d", in, got, want)
		}
	}
}

// 守住 DefaultGroup 返回副本：调用方修改返回值不应污染全局默认配置。
func TestDefaultGroupCopy(t *testing.T) {
	g := queue.DefaultGroup()
	g.MaxConcurrency = 99
	if queue.DefaultGroup().MaxConcurrency != 4 {
		t.Fatal("DefaultGroup 每次应返回新值")
	}
}
