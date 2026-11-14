package clickhouse_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/connector/clickhouse"
	"github.com/godw/godw/internal/pkg/errs"
)

// chFixture 用 httptest 模拟 ClickHouse HTTP 接口。
// 真实实例不可得，但 HTTP 协议层（鉴权头、database 参数、错误码、JSON 解析）
// 正是这个连接器最容易出错的全部逻辑，用假服务端能完整守住。
type chFixture struct {
	srv  *httptest.Server
	rows int
	// fail 为 true 时所有请求都返回 500，用于覆盖服务端错误分支。
	fail bool

	gotUser string
	gotKey  string
	gotDB   string
	gotBody string
}

func newCH(t *testing.T, rows int) *chFixture {
	t.Helper()
	f := &chFixture{rows: rows}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *chFixture) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.gotBody = string(body)
	f.gotUser = r.Header.Get("X-ClickHouse-User")
	f.gotKey = r.Header.Get("X-ClickHouse-Key")
	f.gotDB = r.URL.Query().Get("database")

	if f.fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
		return
	}

	switch {
	case strings.Contains(f.gotBody, "FAIL400"):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "syntax error near FAIL400")
	case strings.Contains(f.gotBody, "LONGBODY"):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("x", 600))
	case strings.Contains(f.gotBody, "BADJSON"):
		_, _ = io.WriteString(w, "this is not json")
	case strings.Contains(f.gotBody, "system.databases"):
		writeJSON(w, map[string]any{
			"meta": []map[string]string{{"name": "name", "type": "String"}},
			"data": []map[string]any{{"name": "dw"}, {"name": "analytics"}},
		})
	case strings.Contains(f.gotBody, "primary_key") && strings.Contains(f.gotBody, "FAILPK"):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "primary_key 查询失败")
	case strings.Contains(f.gotBody, "system.tables"):
		// DescribeTable 会额外查排序键；ListTables 不会。
		if strings.Contains(f.gotBody, "primary_key") {
			if strings.Contains(f.gotBody, "name='nopk'") {
				writeJSON(w, map[string]any{"data": []map[string]any{}})
				return
			}
			if strings.Contains(f.gotBody, "name='emptypk'") {
				writeJSON(w, map[string]any{"data": []map[string]any{{"primary_key": "  "}}})
				return
			}
			writeJSON(w, map[string]any{
				"data": []map[string]any{{"primary_key": "id, toDate(ts), name"}},
			})
			return
		}
		writeJSON(w, map[string]any{
			"data": []map[string]any{
				{"name": "t1", "comment": "主表", "total_rows": 12.0, "total_bytes": "2048"},
				{"name": "t2", "comment": nil, "total_rows": "3"},
			},
		})
	case strings.Contains(f.gotBody, "system.columns"):
		if strings.Contains(f.gotBody, "missing") {
			writeJSON(w, map[string]any{"data": []map[string]any{}})
			return
		}
		writeJSON(w, map[string]any{
			"data": []map[string]any{
				{"name": "id", "type": "UInt64", "comment": ""},
				{"name": "name", "type": "String", "comment": ""},
			},
		})
	case strings.HasSuffix(strings.TrimSpace(f.gotBody), "FORMAT JSON"):
		meta := []map[string]string{{"name": "id", "type": "UInt64"}, {"name": "name", "type": "String"}}
		data := make([]map[string]any, 0, f.rows)
		for i := 0; i < f.rows; i++ {
			data = append(data, map[string]any{"id": i + 1, "name": fmt.Sprintf("n%d", i+1)})
		}
		writeJSON(w, map[string]any{"meta": meta, "data": data})
	default:
		// SELECT 1 与 DDL/DML 都只需要 200。
		_, _ = io.WriteString(w, "1")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *chFixture) conn(t *testing.T) connector.Connector {
	t.Helper()
	return connForURL(t, f.srv.URL, "dw", "reader", "s3cret", "")
}

func (f *chFixture) connNoPass(t *testing.T) connector.Connector {
	t.Helper()
	return connForURL(t, f.srv.URL, "", "", "", "")
}

// connForURL 把测试服务端地址拆成 host/port，构造走正常 New 路径。
func connForURL(t *testing.T, raw, database, user, pass, secure string) connector.Connector {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"host": host, "port": port}
	if database != "" {
		cfg["database"] = database
	}
	if user != "" {
		cfg["user"] = user
	}
	spec := connector.Spec{Config: cfg}
	if pass != "" {
		spec.Secret = map[string]string{"password": pass}
	}
	if secure != "" {
		spec.Config["secure"] = secure
	}
	c, err := clickhouse.New(context.Background(), spec)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	return c
}

// 配置分支决定协议与目标库；secure=true 必须走 https，默认值必须落到 default 库。
func TestNew(t *testing.T) {
	ctx := context.Background()
	c, err := clickhouse.New(ctx, connector.Spec{Config: map[string]any{
		"host": "127.0.0.1", "port": 1, "database": "dw", "user": "u", "secure": "true",
	}, Secret: map[string]string{"password": "p"}})
	if err != nil {
		t.Fatalf("构造不应建立连接: %v", err)
	}
	if c.Type() != "clickhouse" || c.Dialect().Name() != "clickhouse" {
		t.Fatalf("类型/方言错误: %s %s", c.Type(), c.Dialect().Name())
	}
	caps := c.Capabilities()
	if !caps.ReplacePartition || caps.InsertOverwrite || caps.TransactionalDDL {
		t.Fatalf("ClickHouse 能力位声明错误: %+v", caps)
	}
	if !caps.WindowFunction || !caps.ApproxCountDistinct || !caps.BulkLoad {
		t.Fatalf("ClickHouse 能力位声明不完整: %+v", caps)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 应为空实现: %v", err)
	}

	// 空配置必须能构造（默认 host/port/database/user 在真正请求时才可见）。
	def, err := clickhouse.New(ctx, connector.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Type() != "clickhouse" {
		t.Fatalf("默认构造类型错误: %s", def.Type())
	}
	_ = def.Close()

	// secure=true 使用 https：对明文 HTTP 服务端握手必然失败；
	// 同一服务端在 secure=false 时则应成功，证明分支确实切换了协议。
	f := newCH(t, 0)
	if err := connForURL(t, f.srv.URL, "", "", "", "true").Test(ctx); err == nil {
		t.Fatal("secure=true 应使用 https，明文服务端不应成功")
	}
	if err := f.conn(t).Test(ctx); err != nil {
		t.Fatalf("secure=false 时明文服务端应可用: %v", err)
	}
}

// 鉴权头与 database 参数漏传会让查询落到 default 库或用匿名身份执行。
func TestRequestHeadersAndDatabase(t *testing.T) {
	f := newCH(t, 1)
	c := f.conn(t)
	if err := c.Test(context.Background()); err != nil {
		t.Fatalf("Test 失败: %v", err)
	}
	if f.gotUser != "reader" || f.gotKey != "s3cret" || f.gotDB != "dw" {
		t.Fatalf("请求头/参数丢失: user=%q key=%q db=%q", f.gotUser, f.gotKey, f.gotDB)
	}
	if strings.TrimSpace(f.gotBody) != "SELECT 1" {
		t.Fatalf("Test 应发送 SELECT 1，实际 %q", f.gotBody)
	}

	// 未配置密码时不能发送空的 X-ClickHouse-Key 头。
	f2 := newCH(t, 0)
	if err := f2.connNoPass(t).Test(context.Background()); err != nil {
		t.Fatalf("无密码连接测试失败: %v", err)
	}
	if f2.gotKey != "" {
		t.Fatalf("未配置密码时不应发送 Key 头，得到 %q", f2.gotKey)
	}
	if f2.gotUser != "default" || f2.gotDB != "default" {
		t.Fatalf("默认用户/库应落到 default: user=%q db=%q", f2.gotUser, f2.gotDB)
	}
}

func TestTestError(t *testing.T) {
	f := newCH(t, 1)
	f.fail = true
	c := f.conn(t)
	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("服务端 500 时 Test 必须失败")
	}
	if !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("错误码应为 CodeExecutionFailed: %v", err)
	}
}

func TestQuery(t *testing.T) {
	f := newCH(t, 2)
	c := f.conn(t)
	rows, err := c.Query(context.Background(), "SELECT id, name FROM t WHERE id > ?", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := rows.Columns()
	if len(cols) != 2 || cols[0].Name != "id" || cols[0].Type != "UInt64" || cols[0].Ordinal != 1 {
		t.Fatalf("列元信息错误: %+v", cols)
	}

	var got []string
	for rows.Next() {
		var id any
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		if _, ok := id.(float64); !ok {
			t.Fatalf("JSON 数字应解码为 float64，得到 %T", id)
		}
		got = append(got, name)
	}
	if len(got) != 2 || got[0] != "n1" || got[1] != "n2" {
		t.Fatalf("行数据错误: %v", got)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("残留错误: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("重复 Close 不应报错: %v", err)
	}

	// 参数必须渲染进 SQL，否则 ClickHouse 收到的是裸 ? 语法错误。
	if !strings.Contains(f.gotBody, "id > 0 FORMAT JSON") {
		t.Fatalf("参数未渲染: %q", f.gotBody)
	}

	// 多参数按序替换、字符串转义、无占位符时多余参数被忽略。
	if _, err := c.Query(context.Background(), "SELECT id FROM t WHERE a=? AND b=? AND c=?", 1, "x'y", true); err != nil {
		t.Fatal(err)
	}
	want := "a=1 AND b='x''y' AND c=true FORMAT JSON"
	if !strings.Contains(f.gotBody, want) {
		t.Fatalf("多参数渲染错误: %q, want 包含 %q", f.gotBody, want)
	}
	if _, err := c.Query(context.Background(), "SELECT 1", 99); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(f.gotBody) != "SELECT 1 FORMAT JSON" {
		t.Fatalf("多余参数不应修改 SQL: %q", f.gotBody)
	}
}

// 服务端返回超长错误体时必须截断，避免把整页 HTML 塞进错误信息与日志。
func TestErrorBodyTruncated(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)
	_, err := c.Query(context.Background(), "SELECT LONGBODY", nil)
	if err == nil {
		t.Fatal("400 必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "...") {
		t.Fatalf("超长错误体应被截断: %q", msg)
	}
	if strings.Count(msg, "x") > 520 {
		t.Fatalf("错误体未被截断，长度 %d", len(msg))
	}
}

func TestQueryErrors(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)

	if _, err := c.Query(context.Background(), "SELECT FAIL400", nil); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("400 应转成执行失败: %v", err)
	}

	_, err := c.Query(context.Background(), "SELECT BADJSON", nil)
	if err == nil {
		t.Fatal("非 JSON 响应必须报错，不能返回空结果集")
	}
	if !errs.Is(err, errs.CodeExecutionFailed) || !strings.Contains(err.Error(), "非 JSON") {
		t.Fatalf("错误分类错误: %v", err)
	}
	ae := errs.From(err)
	if _, ok := ae.Meta["body"]; !ok {
		t.Fatalf("应保留响应片段便于排查: %+v", ae.Meta)
	}
}

func TestExec(t *testing.T) {
	f := newCH(t, 1)
	c := f.conn(t)
	res, err := c.Exec(context.Background(), "INSERT INTO t VALUES (?, ?)", "O'Reilly", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("Exec 失败: %v", err)
	}
	if res.Duration < 0 {
		t.Fatalf("耗时不应为负: %v", res.Duration)
	}
	if !strings.Contains(f.gotBody, "'O''Reilly'") || !strings.Contains(f.gotBody, "'2026-01-02 03:04:05'") {
		t.Fatalf("参数渲染未做转义/时间格式化: %q", f.gotBody)
	}

	if _, err := c.Exec(context.Background(), "INSERT FAIL400", nil); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("Exec 失败应返回执行错误: %v", err)
	}
}

func TestListSchemas(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)
	got, err := c.ListSchemas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "dw" || got[1] != "analytics" {
		t.Fatalf("库列表错误: %v", got)
	}
}

func TestListTables(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)
	got, err := c.ListTables(context.Background(), "o'brien")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.gotBody, "'o''brien'") {
		t.Fatalf("schema 未转义，存在注入风险: %q", f.gotBody)
	}
	if len(got) != 2 {
		t.Fatalf("表数量 = %d, want 2", len(got))
	}
	if got[0].Name != "t1" || got[0].Schema != "o'brien" || got[0].Type != "table" {
		t.Fatalf("第一张表元信息错误: %+v", got[0])
	}
	if got[0].RowCount != 12 || got[0].SizeBytes != 2048 {
		t.Fatalf("数值列转换错误: %+v", got[0])
	}
	if got[1].RowCount != 3 || got[1].SizeBytes != 0 {
		t.Fatalf("字符串/缺失数值列转换错误: %+v", got[1])
	}
}

func TestDescribeTable(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)
	meta, err := c.DescribeTable(context.Background(), "dw", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Schema != "dw" || meta.Name != "t1" || len(meta.Columns) != 2 {
		t.Fatalf("表元信息错误: %+v", meta)
	}
	if meta.Columns[0].Name != "id" || meta.Columns[1].Ordinal != 2 || meta.Columns[1].Type != "String" {
		t.Fatalf("列元信息错误: %+v", meta.Columns)
	}
	// 排序键里的表达式（toDate(ts)）不是列名，不能当主键带出去。
	if len(meta.PrimaryKey) != 2 || meta.PrimaryKey[0] != "id" || meta.PrimaryKey[1] != "name" {
		t.Fatalf("排序键解析错误: %v", meta.PrimaryKey)
	}

	// 查不到排序键时返回空主键，而不是把 "<nil>" 之类的占位符当列名。
	noPK, err := c.DescribeTable(context.Background(), "dw", "nopk")
	if err != nil {
		t.Fatal(err)
	}
	if len(noPK.PrimaryKey) != 0 {
		t.Fatalf("无排序键表不应带出主键: %v", noPK.PrimaryKey)
	}

	// 排序键为空串时同样返回空主键。
	emptyPK, err := c.DescribeTable(context.Background(), "dw", "emptypk")
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyPK.PrimaryKey) != 0 {
		t.Fatalf("空排序键不应带出主键: %v", emptyPK.PrimaryKey)
	}

	// 排序键查询失败不影响列元数据采集（主键是增强信息）。
	despiteErr, err := c.DescribeTable(context.Background(), "dw", "FAILPK")
	if err != nil {
		t.Fatalf("排序键查询失败不应让整表采集失败: %v", err)
	}
	if len(despiteErr.PrimaryKey) != 0 {
		t.Fatalf("查询失败时应返回空主键: %v", despiteErr.PrimaryKey)
	}

	if _, err := c.DescribeTable(context.Background(), "dw", "missing"); err == nil {
		t.Fatal("空列集应报错")
	} else if !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("表不存在应为 CodeNotFound: %v", err)
	}
}

// Stream 的分批边界错一位就会丢最后一批或少一次回调。
func TestStream(t *testing.T) {
	f := newCH(t, 5)
	c := f.conn(t)

	var sizes []int
	var first, last int64
	err := c.Stream(context.Background(), "SELECT id, name FROM t", 2, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			if first == 0 {
				first = id
			}
			last = id
			n++
		}
		sizes = append(sizes, n)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("分批错误: %v", sizes)
	}
	if first != 1 || last != 5 {
		t.Fatalf("跨批首尾值错误: %d..%d", first, last)
	}

	// batchSize=0 表示不分批，一次给全量。
	sizes = nil
	if err := c.Stream(context.Background(), "SELECT id, name FROM t", 0, func(rows connector.Rows) error {
		n := 0
		for rows.Next() {
			n++
		}
		sizes = append(sizes, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[0] != 5 {
		t.Fatalf("不分批时应一次回调: %v", sizes)
	}

	// 行数刚好是批大小的整数倍时，末尾不能多回调一个空批次。
	f2 := newCH(t, 4)
	c2 := f2.conn(t)
	calls := 0
	if err := c2.Stream(context.Background(), "SELECT id, name FROM t", 2, func(rows connector.Rows) error {
		calls++
		for rows.Next() {
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("整数倍批次回调次数 = %d, want 2", calls)
	}

	// 回调报错必须中断并原样返回，不能继续消费后续批次。
	sentinel := fmt.Errorf("consumer stopped")
	err = c.Stream(context.Background(), "SELECT id, name FROM t", 2, func(connector.Rows) error { return sentinel })
	if err != sentinel {
		t.Fatalf("回调错误应原样返回，得到 %v", err)
	}
}

func TestStreamQueryError(t *testing.T) {
	f := newCH(t, 0)
	c := f.conn(t)
	if err := c.Stream(context.Background(), "SELECT FAIL400", 10, func(connector.Rows) error { return nil }); err == nil {
		t.Fatal("查询失败时 Stream 必须返回错误")
	}
}

// 数据源不可达与 SQL 执行失败是两类错误，前端重试策略不同。
func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	srv.Close() // 端口已释放，后续请求必然连接失败

	c, err := clickhouse.New(context.Background(), connector.Spec{Config: map[string]any{
		"host": host, "port": port,
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Test(context.Background())
	if err == nil {
		t.Fatal("不可达地址必须报错")
	}
	if !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("连接失败应归为 CodeDatasourceUnreachable: %v", err)
	}

	if _, err := c.ListSchemas(context.Background()); !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("ListSchemas 应透出连接错误: %v", err)
	}
	if _, err := c.Query(context.Background(), "SELECT 1", nil); !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("Query 应透出连接错误: %v", err)
	}
	if _, err := c.Exec(context.Background(), "SELECT 1", nil); !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("Exec 应透出连接错误: %v", err)
	}
	if err := c.Stream(context.Background(), "SELECT 1", 1, func(connector.Rows) error { return nil }); !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("Stream 应透出连接错误: %v", err)
	}
	if _, err := c.ListTables(context.Background(), "dw"); !errs.Is(err, errs.CodeDatasourceUnreachable) {
		t.Fatalf("ListTables 应透出连接错误: %v", err)
	}
}

// 字面量替换必须转义单引号：这是 ClickHouse HTTP 接口唯一的注入面。
