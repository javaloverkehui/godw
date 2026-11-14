//go:build !duckdb

package duckdb

// 默认构建不包含 DuckDB 驱动（依赖 CGO 与本地动态库）。
// 这里注册一个「明确报错」的工厂，而不是让用户看到「不支持的数据源类型」——
// 后者会让人以为平台没实现，前者直接告诉他要加 -tags duckdb 重新编译。

import (
	"context"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/pkg/errs"
)

func init() {
	connector.Register("duckdb", newStub)
}

func newStub(ctx context.Context, spec connector.Spec) (connector.Connector, error) {
	return nil, errs.New(errs.CodeUnavailable,
		"当前二进制未启用 DuckDB 支持，请使用 go build -tags duckdb 重新编译").
		WithMeta("hint", "DuckDB 驱动需要 CGO（gcc）；静态库随驱动一起编译，不依赖系统 libduckdb")
}
