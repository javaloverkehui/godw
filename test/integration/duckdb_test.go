//go:build integration && duckdb

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"

	_ "github.com/godw/godw/internal/connector/duckdb"
)

// DuckDB 是嵌入式引擎，没有 DSN，用临时文件验证同一条闭环。
func TestDuckDBRoundTrip(t *testing.T) {
	dir := os.Getenv("GODW_TEST_DUCKDB_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	spec := connector.Spec{
		Type:   "duckdb",
		Name:   "it-duckdb",
		Config: map[string]any{"path": filepath.Join(dir, "it.duckdb")},
	}
	conn, err := connector.Open(context.Background(), spec)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	_ = conn.Close()
	runRoundTrip(t, spec, "main", dialect.DuckDB{})
}
