package sensor

import (
	"context"
	"os"
	"testing"
	"time"
)

// T0509 验收：文件 10s 后到达，两个轮询周期内成功；重复投递不产生新实例（去重逻辑在 service）。

func TestFileSensor(t *testing.T) {
	now := time.Now()
	calls := 0
	c := &Checker{Stat: func(p string) (int64, time.Time, error) {
		calls++
		if calls < 3 {
			return 0, time.Time{}, os.ErrNotExist
		}
		return 1024, now.Add(-30 * time.Second), nil
	}}

	r, err := c.Check(context.Background(), Spec{Kind: KindFile, Path: "/tmp/x.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Satisfied {
		t.Fatalf("文件不存在时应不满足: %+v", r)
	}

	// 第 3 次开始存在
	_, _ = c.Check(context.Background(), Spec{Kind: KindFile, Path: "/tmp/x.csv"})
	r, err = c.Check(context.Background(), Spec{Kind: KindFile, Path: "/tmp/x.csv"})
	if err != nil || !r.Satisfied {
		t.Fatalf("文件就绪应满足: %+v err=%v", r, err)
	}
	if r.Observed != "1024 B" {
		t.Fatalf("观测值异常: %s", r.Observed)
	}
}

func TestFileSensorStillWriting(t *testing.T) {
	c := &Checker{Stat: func(p string) (int64, time.Time, error) {
		return 512, time.Now(), nil // 刚修改
	}}
	r, err := c.Check(context.Background(), Spec{Kind: KindFile, Path: "/tmp/x", StableSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if r.Satisfied {
		t.Fatal("文件刚修改时应判定仍在写入")
	}
}

func TestTableSensor(t *testing.T) {
	c := &Checker{Query: func(ctx context.Context, ds, sqlText string) (string, error) {
		if sqlText != "SELECT COUNT(*) FROM ods.`order` WHERE dt = '2025-11-10'" {
			t.Fatalf("SQL 不符: %s", sqlText)
		}
		return "42", nil
	}}
	r, err := c.Check(context.Background(), Spec{
		Kind: KindTable, Datasource: "pg", Table: "ods.`order`", Partition: "dt = '2025-11-10'",
	})
	if err != nil || !r.Satisfied || r.Observed != "42" {
		t.Fatalf("表传感器结果 %+v err=%v", r, err)
	}

	// 行数为 0 → 不满足
	c.Query = func(ctx context.Context, ds, s string) (string, error) { return "0", nil }
	r, _ = c.Check(context.Background(), Spec{Kind: KindTable, Datasource: "pg", Table: "t"})
	if r.Satisfied {
		t.Fatal("0 行不应满足")
	}
}

func TestHTTPSensor(t *testing.T) {
	c := &Checker{HTTPGet: func(ctx context.Context, url string) (int, error) { return 200, nil }}
	r, err := c.Check(context.Background(), Spec{Kind: KindHTTP, URL: "http://x"})
	if err != nil || !r.Satisfied {
		t.Fatalf("%+v %v", r, err)
	}
	c.HTTPGet = func(ctx context.Context, url string) (int, error) { return 503, nil }
	r, _ = c.Check(context.Background(), Spec{Kind: KindHTTP, URL: "http://x"})
	if r.Satisfied {
		t.Fatal("503 不应满足")
	}
}

func TestWaitTimeout(t *testing.T) {
	c := &Checker{Stat: func(p string) (int64, time.Time, error) { return 0, time.Time{}, os.ErrNotExist }}
	start := time.Now()
	_, err := c.Wait(context.Background(), Spec{
		Kind: KindFile, Path: "/nope", IntervalSec: 1, TimeoutSec: 1,
	})
	if err == nil {
		t.Fatal("应超时")
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("超时时间未生效")
	}
}

func TestFromNodeConfig(t *testing.T) {
	spec, ok := FromNodeConfig(map[string]any{
		"sensor": map[string]any{
			"kind": "file", "path": "/data/x.csv", "intervalSec": float64(15), "timeoutSec": float64(600),
		},
	})
	if !ok || spec.Kind != KindFile || spec.IntervalSec != 15 || spec.TimeoutSec != 600 {
		t.Fatalf("解析错误: %+v ok=%v", spec, ok)
	}
	if _, ok := FromNodeConfig(map[string]any{}); ok {
		t.Fatal("无 sensor 配置应返回 false")
	}
	// 缺 kind
	if _, ok := FromNodeConfig(map[string]any{"sensor": map[string]any{"path": "x"}}); ok {
		t.Fatal("缺 kind 应返回 false")
	}
}
