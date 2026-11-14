//go:build !duckdb

package duckdb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/godw/godw/internal/connector"
	_ "github.com/godw/godw/internal/connector/duckdb"
)

// 默认构建下必须给出「怎么启用」的提示，而不是「不支持的数据源类型」或笼统的打开失败。
func TestStubHintsBuildTag(t *testing.T) {
	_, err := connector.New(context.Background(), connector.Spec{Type: "duckdb", Name: "x"})
	if err == nil {
		t.Fatal("默认构建不应成功构造 DuckDB 连接器")
	}
	if !strings.Contains(err.Error(), "-tags duckdb") {
		t.Fatalf("错误信息没有告诉用户怎么启用: %v", err)
	}
}
