package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

// Capability 描述引擎真实支持的能力。上层据此选择写入策略，
// 不允许「猜引擎支持什么」——能力缺失时必须走降级路径。
type Capability struct {
	MergeUpsert         bool `json:"mergeUpsert"`
	InsertOverwrite     bool `json:"insertOverwrite"`
	ReplacePartition    bool `json:"replacePartition"`
	TransactionalDDL    bool `json:"transactionalDdl"`
	MaterializedView    bool `json:"materializedView"`
	WindowFunction      bool `json:"windowFunction"`
	ApproxCountDistinct bool `json:"approxCountDistinct"`
	JSONType            bool `json:"jsonType"`
	BulkLoad            bool `json:"bulkLoad"`
}

type ColumnMeta struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default,omitempty"`
	Comment  string  `json:"comment,omitempty"`
	Ordinal  int     `json:"ordinal"`
}

type TableMeta struct {
	Schema    string       `json:"schema"`
	Name      string       `json:"name"`
	Type      string       `json:"type"`
	Comment   string       `json:"comment,omitempty"`
	RowCount  int64        `json:"rowCount"`
	SizeBytes int64        `json:"sizeBytes"`
	Columns   []ColumnMeta `json:"columns"`
	// PrimaryKey 是源表声明的主键列，按索引内的顺序排列（复合主键顺序有意义）。
	// 采集必须原样带出：建模脚手架据此定主键，而不是拿列名去猜——
	// 源表已经写清楚的事实，不该让用户再回答一次。
	PrimaryKey  []string `json:"primaryKey,omitempty"`
	PartitionBy []string `json:"partitionBy,omitempty"`
}

// Rows 是游标式结果集。调用方必须 Close。
type Rows interface {
	Columns() []ColumnMeta
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

type Result struct {
	RowsAffected int64         `json:"rowsAffected"`
	Duration     time.Duration `json:"duration"`
}

// Spec 是打开连接所需的全部信息。Secret 与 Config 分离，
// 前者只来自加密存储，后者可以出现在日志与 API 响应里。
type Spec struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Config map[string]any    `json:"config"`
	Secret map[string]string `json:"-"`
}

// Get 读取字符串配置，支持默认值。
func (s Spec) Get(key, def string) string {
	if v, ok := s.Config[key]; ok {
		if sv, ok := v.(string); ok && sv != "" {
			return sv
		}
	}
	return def
}

// GetInt 读取整数配置。
func (s Spec) GetInt(key string, def int) int {
	switch v := s.Config[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

// Connector 是数据源唯一入口。实现必须并发安全。
type Connector interface {
	Type() string
	Capabilities() Capability
	Dialect() dialect.Dialect

	Test(ctx context.Context) error
	ListSchemas(ctx context.Context) ([]string, error)
	ListTables(ctx context.Context, schema string) ([]TableMeta, error)
	DescribeTable(ctx context.Context, schema, table string) (*TableMeta, error)

	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (Result, error)
	Stream(ctx context.Context, sql string, batchSize int, fn func(Rows) error) error

	Close() error
}

// Factory 由各引擎在 init() 注册。
type Factory func(ctx context.Context, spec Spec) (Connector, error)

var registry = map[string]Factory{}

// Register 注册连接器工厂。重复注册直接 panic，避免测试中静默覆盖。
func Register(typ string, f Factory) {
	typ = strings.ToLower(typ)
	if _, exists := registry[typ]; exists {
		panic("connector: 重复注册 " + typ)
	}
	registry[typ] = f
}

// New 按类型构造连接器，但不建立连接。
// 查看能力声明、渲染 DSN、校验类型都只需要「构造」，不该因为数据库没启动就失败。
func New(ctx context.Context, spec Spec) (Connector, error) {
	f, ok := registry[strings.ToLower(spec.Type)]
	if !ok {
		return nil, errs.New(errs.CodeInvalidArgument, "不支持的数据源类型: "+spec.Type).
			WithMeta("supported", Types())
	}
	c, err := f(ctx, spec)
	if err != nil {
		// 工厂自己给出业务错误时原样返回：例如 DuckDB 未编译进二进制，
		// 真正有用的是「请用 -tags duckdb 重新编译」，而不是笼统的「打开数据源失败」。
		var ae *errs.AppError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, errs.Wrap(errs.CodeDatasourceUnreachable, err, "打开数据源失败").
			WithMeta("datasource", spec.Name).WithMeta("type", spec.Type)
	}
	return c, nil
}

// Open 按类型构造连接器并验证连通性。
func Open(ctx context.Context, spec Spec) (Connector, error) {
	c, err := New(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := c.Test(ctx); err != nil {
		_ = c.Close()
		return nil, errs.Wrap(errs.CodeDatasourceUnreachable, err, "连接测试失败").
			WithMeta("datasource", spec.Name)
	}
	return c, nil
}

// Types 返回已注册的数据源类型，排序后输出保证提示稳定。
func Types() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildDSN 用于日志展示：只保留 host/db，剔除一切凭证。
func BuildDSN(spec Spec) string {
	var b strings.Builder
	b.WriteString(spec.Type)
	b.WriteString("://")
	if h := spec.Get("host", ""); h != "" {
		b.WriteString(h)
		if p := spec.GetInt("port", 0); p > 0 {
			b.WriteString(fmt.Sprintf(":%d", p))
		}
	}
	if db := spec.Get("database", ""); db != "" {
		b.WriteString("/" + db)
	}
	return b.String()
}

// Transactor 是可选的原子性能力。实现它的连接器可以把多条写入放进一个事务；
// 未实现的引擎（如 ClickHouse）由上层退化为「逐条执行 + 失败标记需重跑」。
type Transactor interface {
	WithTx(ctx context.Context, fn func(exec func(sql string, args ...any) (Result, error)) error) error
}
