package testsupport

import (
	"context"
	"testing"
)

// 夹具本身也要有测试：它一旦坏掉，所有依赖真实库的用例都会静默跳过，
// 覆盖率看起来只是「变低了」，很难定位。这里直接验证建库、迁移、清理三步。
func TestProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60e9)
	defer cancel()
	if err := Probe(ctx); err != nil {
		t.Fatalf("夹具不可用: %v", err)
	}
}
