package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/pkg/errs"
)

func init() {
	connector.Register("clickhouse", New)
}

// Conn 通过 HTTP 协议访问 ClickHouse：无需 CGO 驱动，部署简单，
// 且便于在受限网络里走代理。
type Conn struct {
	base     string
	database string
	user     string
	pass     string
	client   *http.Client
}

func New(ctx context.Context, spec connector.Spec) (connector.Connector, error) {
	scheme := "http"
	if spec.Get("secure", "") == "true" {
		scheme = "https"
	}
	host := spec.Get("host", "127.0.0.1")
	port := spec.GetInt("port", 8123)
	return &Conn{
		base:     fmt.Sprintf("%s://%s:%d/", scheme, host, port),
		database: spec.Get("database", "default"),
		user:     spec.Get("user", "default"),
		pass:     spec.Secret["password"],
		client:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *Conn) Type() string { return "clickhouse" }

func (c *Conn) Capabilities() connector.Capability {
	return connector.Capability{
		// ClickHouse 没有 INSERT OVERWRITE（那是 Hive 语法），分区覆写只能走 REPLACE PARTITION。
		// 早先这里误声明为 true，生成出的 INSERT OVERWRITE 在真实集群上直接语法错误。
		ReplacePartition: true, WindowFunction: true,
		ApproxCountDistinct: true, BulkLoad: true,
	}
}

func (c *Conn) Dialect() dialect.Dialect { return dialect.ClickHouse{} }

func (c *Conn) Test(ctx context.Context) error {
	_, err := c.do(ctx, "SELECT 1", nil)
	return err
}

func (c *Conn) ListSchemas(ctx context.Context) ([]string, error) {
	rows, err := c.query(ctx, "SELECT name FROM system.databases WHERE name NOT IN ('system','INFORMATION_SCHEMA','information_schema') ORDER BY name")
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rows.Data {
		out = append(out, fmt.Sprint(r["name"]))
	}
	return out, nil
}

func (c *Conn) ListTables(ctx context.Context, schema string) ([]connector.TableMeta, error) {
	q := fmt.Sprintf(`SELECT name, engine, comment, total_rows, total_bytes
		FROM system.tables WHERE database = '%s' ORDER BY name`, esc(schema))
	rows, err := c.query(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []connector.TableMeta{}
	for _, r := range rows.Data {
		out = append(out, connector.TableMeta{
			Schema:    schema,
			Name:      fmt.Sprint(r["name"]),
			Type:      "table",
			Comment:   fmt.Sprint(r["comment"]),
			RowCount:  toInt(r["total_rows"]),
			SizeBytes: toInt(r["total_bytes"]),
		})
	}
	return out, nil
}

func (c *Conn) DescribeTable(ctx context.Context, schema, table string) (*connector.TableMeta, error) {
	q := fmt.Sprintf(`SELECT name, type, comment FROM system.columns
		WHERE database='%s' AND table='%s' ORDER BY position`, esc(schema), esc(table))
	rows, err := c.query(ctx, q)
	if err != nil {
		return nil, err
	}
	t := &connector.TableMeta{Schema: schema, Name: table, Type: "table"}
	for i, r := range rows.Data {
		t.Columns = append(t.Columns, connector.ColumnMeta{
			Name:    fmt.Sprint(r["name"]),
			Type:    fmt.Sprint(r["type"]),
			Ordinal: i + 1,
		})
	}
	if len(t.Columns) == 0 {
		return nil, errs.New(errs.CodeNotFound, "表不存在: "+schema+"."+table)
	}
	// ClickHouse 没有约束式主键，「主键」是排序键的前缀，system.tables 里是逗号分隔串。
	t.PrimaryKey = c.primaryKey(ctx, schema, table)
	return t, nil
}

func (c *Conn) primaryKey(ctx context.Context, schema, table string) []string {
	q := fmt.Sprintf(`SELECT primary_key FROM system.tables WHERE database='%s' AND name='%s'`, esc(schema), esc(table))
	res, err := c.query(ctx, q)
	if err != nil || len(res.Data) == 0 {
		return nil
	}
	raw := strings.TrimSpace(fmt.Sprint(res.Data[0]["primary_key"]))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		// 排序键允许写表达式，例如 toDate(ts)；只有裸列名才可能是模型主键。
		if p = strings.TrimSpace(p); p != "" && !strings.ContainsAny(p, "()") {
			out = append(out, p)
		}
	}
	return out
}

func (c *Conn) Query(ctx context.Context, sqlText string, args ...any) (connector.Rows, error) {
	res, err := c.query(ctx, renderArgs(sqlText, args))
	if err != nil {
		return nil, err
	}
	cols := make([]connector.ColumnMeta, len(res.Meta))
	for i, m := range res.Meta {
		cols[i] = connector.ColumnMeta{Name: m.Name, Type: m.Type, Ordinal: i + 1}
	}
	data := make([][]any, 0, len(res.Data))
	for _, row := range res.Data {
		vals := make([]any, len(cols))
		for i, col := range cols {
			vals[i] = row[col.Name]
		}
		data = append(data, vals)
	}
	return connector.NewMemRows(cols, data), nil
}

func (c *Conn) Exec(ctx context.Context, sqlText string, args ...any) (connector.Result, error) {
	start := time.Now()
	_, err := c.do(ctx, renderArgs(sqlText, args), nil)
	return connector.Result{Duration: time.Since(start)}, err
}

func (c *Conn) Stream(ctx context.Context, sqlText string, batchSize int, fn func(connector.Rows) error) error {
	rows, err := c.Query(ctx, sqlText)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols := rows.Columns()
	var batch [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		batch = append(batch, vals)
		if batchSize > 0 && len(batch) >= batchSize {
			if err := fn(connector.NewMemRows(cols, batch)); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		return fn(connector.NewMemRows(cols, batch))
	}
	return rows.Err()
}

func (c *Conn) Close() error { return nil }

type chMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type chResult struct {
	Meta []chMeta         `json:"meta"`
	Data []map[string]any `json:"data"`
	Rows int              `json:"rows"`
}

func (c *Conn) query(ctx context.Context, sqlText string) (*chResult, error) {
	body, err := c.do(ctx, sqlText+" FORMAT JSON", nil)
	if err != nil {
		return nil, err
	}
	var res chResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, errs.Wrap(errs.CodeExecutionFailed, err, "ClickHouse 返回非 JSON").
			WithMeta("body", truncate(string(body), 300))
	}
	return &res, nil
}

func (c *Conn) do(ctx context.Context, sqlText string, q url.Values) ([]byte, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("database", c.database)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"?"+q.Encode(), strings.NewReader(sqlText))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ClickHouse-User", c.user)
	if c.pass != "" {
		req.Header.Set("X-ClickHouse-Key", c.pass)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDatasourceUnreachable, err, "ClickHouse 请求失败")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, errs.New(errs.CodeExecutionFailed, "ClickHouse: "+truncate(string(body), 500)).
			WithMeta("status", resp.StatusCode)
	}
	return body, nil
}

func renderArgs(sqlText string, args []any) string {
	// ClickHouse HTTP 接口不支持绑定参数，这里做字面量替换。
	// 只处理由系统生成的 SQL，用户输入必须经过 API 层校验与鉴权。
	for _, a := range args {
		var lit string
		switch v := a.(type) {
		case string:
			lit = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case time.Time:
			lit = "'" + v.Format("2006-01-02 15:04:05") + "'"
		default:
			lit = fmt.Sprint(v)
		}
		sqlText = strings.Replace(sqlText, "?", lit, 1)
	}
	return sqlText
}

func esc(s string) string { return strings.ReplaceAll(s, "'", "''") }

func toInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		var out int64
		fmt.Sscan(n, &out)
		return out
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
