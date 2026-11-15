package logstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godw/godw/internal/obs/logstore"
)

// fakeSink 记录写入批次，并可注入读写错误。
type fakeSink struct {
	batches  [][]logstore.Line
	read     []logstore.Line
	writeErr error
	readErr  error
	reads    int
}

func (f *fakeSink) WriteBatch(ctx context.Context, instanceID string, lines []logstore.Line) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := make([]logstore.Line, len(lines))
	copy(cp, lines)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeSink) ReadAfter(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]logstore.Line, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.read, nil
}

// 守住脱敏规则：密码、token、Authorization、连接串内的口令四类都要被替换。
func TestMask(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"password 等号", "dsn password=Hunter2 user=x", "dsn password=*** user=x"},
		{"pwd 冒号", "pwd: topsecret;", "pwd=***;"},
		{"token", "token=abc123 other", "token=*** other"},
		{"api key", "api_key: k-999", "api_key=***"},
		{"secret", "client_secret=xyz", "client_secret=***"},
		{"authorization bearer", "Authorization: Bearer abc.def.ghi", "Authorization: ***"},
		{"authorization basic", "authorization: Basic dXNlcjpwdw==", "Authorization: ***"},
		{"连接串口令", "postgres://godw:p@ssw0rd@127.0.0.1:5432/godw", "postgres://godw:***@127.0.0.1:5432/godw"},
		{"无敏感信息", "select 1 from t", "select 1 from t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := logstore.Mask(c.in); got != c.want {
				t.Fatalf("Mask(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// 守住 Append 的规范化：级别归一、去掉尾部换行、消息脱敏、seq 单调递增。
func TestBufferAppendNormalizes(t *testing.T) {
	b := logstore.NewBuffer("ins_1", nil, 100)
	levels := []struct {
		in   string
		want string
	}{
		{"ERROR", "error"},
		{"err", "error"},
		{"Warn", "warn"},
		{"warning", "warn"},
		{"DEBUG", "debug"},
		{"info", "info"},
		{"trace", "info"},
		{"", "info"},
	}
	for i, l := range levels {
		line := b.Append("n1", l.in, "password=hunter2\n")
		if line.Level != l.want {
			t.Fatalf("level %q 归一为 %q，期望 %q", l.in, line.Level, l.want)
		}
		if line.Seq != int64(i+1) {
			t.Fatalf("seq = %d，期望 %d", line.Seq, i+1)
		}
		if line.Message != "password=***" {
			t.Fatalf("消息未脱敏或未去换行: %q", line.Message)
		}
		if line.NodeID != "n1" || line.At.IsZero() {
			t.Fatalf("节点或时间缺失: %+v", line)
		}
	}
}

// 守住环形缓冲上限：超过容量后丢弃最旧的一半，内存不能无限增长。
func TestBufferRingDropsOldest(t *testing.T) {
	b := logstore.NewBuffer("ins_1", nil, 4)
	for i := 0; i < 10; i++ {
		b.Append("n", "info", "line")
	}
	lines, err := b.Since(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 容量 4 时最终保留最近 4 行（7..10），证明旧行被丢弃。
	if len(lines) != 4 {
		t.Fatalf("环形缓冲保留 %d 行，期望 4", len(lines))
	}
	if lines[0].Seq != 7 || lines[3].Seq != 10 {
		t.Fatalf("保留区间错误: %d..%d", lines[0].Seq, lines[3].Seq)
	}
}

// 守住默认容量：capacity<=0 时必须回退到 2000，而不是零容量导致每次都裁剪。
func TestBufferDefaultCapacity(t *testing.T) {
	b := logstore.NewBuffer("ins_1", nil, 0)
	for i := 0; i < 2001; i++ {
		b.Append("n", "info", "x")
	}
	lines, err := b.Since(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 2001 行超过 2000 容量，裁剪后保留最近 1000 行（1002..2001）。
	if len(lines) != 1000 || lines[0].Seq != 1002 {
		t.Fatalf("默认容量行为不符: len=%d first=%d", len(lines), lines[0].Seq)
	}
}

// 守住批量刷盘：累计 200 行必须触发一次 WriteBatch，且批次内容完整有序。
func TestBufferAppendFlushesOnThreshold(t *testing.T) {
	sink := &fakeSink{}
	b := logstore.NewBuffer("ins_1", sink, 500)
	for i := 0; i < 199; i++ {
		b.Append("n", "info", "x")
	}
	if len(sink.batches) != 0 {
		t.Fatalf("199 行不应刷盘，实际 %d 批", len(sink.batches))
	}
	b.Append("n", "info", "x")
	if len(sink.batches) != 1 {
		t.Fatalf("第 200 行应触发刷盘，实际 %d 批", len(sink.batches))
	}
	batch := sink.batches[0]
	if len(batch) != 200 || batch[0].Seq != 1 || batch[199].Seq != 200 {
		t.Fatalf("批次内容错误: len=%d", len(batch))
	}
}

// 守住空刷盘与无 sink：没有待写数据时不能产生空批次；sink 为空时不能 panic。
func TestBufferFlushNoop(t *testing.T) {
	ctx := context.Background()
	sink := &fakeSink{}
	b := logstore.NewBuffer("ins_1", sink, 100)
	if err := b.Flush(ctx); err != nil {
		t.Fatalf("空刷盘应返回 nil: %v", err)
	}
	if len(sink.batches) != 0 {
		t.Fatal("空刷盘不应写入空批次")
	}

	b2 := logstore.NewBuffer("ins_1", nil, 100)
	b2.Append("n", "info", "x")
	if err := b2.Flush(ctx); err != nil {
		t.Fatalf("无 sink 刷盘应返回 nil: %v", err)
	}
}

// 守住刷盘失败：错误必须向上返回，调用方才能重试或告警。
func TestBufferFlushError(t *testing.T) {
	boom := errors.New("db down")
	b := logstore.NewBuffer("ins_1", &fakeSink{writeErr: boom}, 100)
	b.Append("n", "info", "x")
	if err := b.Flush(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("应返回 sink 错误，实际 %v", err)
	}
}

// 守住热数据读取：limit 非法时回退 200，命中足够热数据时不回源 DB。
func TestBufferSinceHotOnly(t *testing.T) {
	sink := &fakeSink{}
	b := logstore.NewBuffer("ins_1", sink, 100)
	for i := 0; i < 5; i++ {
		b.Append("n", "info", "x")
	}
	// limit=0 与 limit>1000 都回退为 200；热数据只有 5 行，不足则回源，但这里先验证热路径。
	lines, err := b.Since(context.Background(), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].Seq != 1 {
		t.Fatalf("热路径截断错误: len=%d", len(lines))
	}
	if sink.reads != 0 {
		t.Fatalf("热数据足够时不应回源，实际读了 %d 次", sink.reads)
	}

	// afterSeq 过滤：只要 seq>3 的行。
	lines, err = b.Since(context.Background(), 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Seq != 4 {
		t.Fatalf("afterSeq 过滤错误: %+v", lines)
	}
}

// 守住断点续传回源：内存不足时查 DB，并按 seq 去重合并。
func TestBufferSinceColdMerge(t *testing.T) {
	sink := &fakeSink{
		// DB 里包含内存中的 2、3，以及更早的 10、11。
		read: []logstore.Line{
			{Seq: 2, Message: "db-2"},
			{Seq: 3, Message: "db-3"},
			{Seq: 10, Message: "db-10"},
		},
	}
	b := logstore.NewBuffer("ins_1", sink, 100)
	b.Append("n", "info", "1")
	b.Append("n", "info", "2")
	b.Append("n", "info", "3")

	lines, err := b.Since(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 去重后应为 DB 的 3 条 + 内存独有的 1 条 = 4 条，且 seq 不重复。
	seen := map[int64]bool{}
	for _, l := range lines {
		if seen[l.Seq] {
			t.Fatalf("seq %d 重复返回，客户端会重复渲染", l.Seq)
		}
		seen[l.Seq] = true
	}
	if len(lines) != 4 {
		t.Fatalf("合并结果 %d 条，期望 4: %+v", len(lines), lines)
	}
	// 合并后按 seq 升序：前端按 seq 游标继续读，乱序会重复或漏行。
	for i := 1; i < len(lines); i++ {
		if lines[i].Seq < lines[i-1].Seq {
			t.Fatalf("合并结果应按 seq 升序: %+v", lines)
		}
	}
	if lines[0].Seq != 1 || lines[len(lines)-1].Seq != 10 {
		t.Fatalf("合并结果的首尾 seq 不对: %+v", lines)
	}
}

// 守住回源结果裁剪：合并后超过 limit 必须截断，避免一次 SSE 拉爆。
func TestBufferSinceColdTruncated(t *testing.T) {
	var cold []logstore.Line
	for i := int64(10); i < 15; i++ {
		cold = append(cold, logstore.Line{Seq: i})
	}
	sink := &fakeSink{read: cold}
	b := logstore.NewBuffer("ins_1", sink, 100)
	b.Append("n", "info", "1")
	b.Append("n", "info", "2")

	lines, err := b.Since(context.Background(), 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 {
		t.Fatalf("应裁剪到 limit=4，实际 %d", len(lines))
	}
}

// 守住回源失败：必须把热数据一并返回，同时把错误交给调用方决策。
func TestBufferSinceColdError(t *testing.T) {
	boom := errors.New("db down")
	sink := &fakeSink{readErr: boom}
	b := logstore.NewBuffer("ins_1", sink, 100)
	b.Append("n", "info", "1")
	b.Append("n", "info", "2")

	lines, err := b.Since(context.Background(), 0, 10)
	if !errors.Is(err, boom) {
		t.Fatalf("应返回 DB 错误，实际 %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("失败时也应返回已有热数据，实际 %d", len(lines))
	}
}

// 守住无 sink 的读取：纯内存实例（如测试环境）不能因为回源而报错。
func TestBufferSinceNoSink(t *testing.T) {
	b := logstore.NewBuffer("ins_1", nil, 100)
	b.Append("n", "info", "x")
	lines, err := b.Since(context.Background(), 0, 0) // limit 非法回退 200
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("应返回 1 行，实际 %d", len(lines))
	}
}

// 守住 Close：停止通道只关一次，重复 Close 不能 panic，且都会尝试刷盘。
func TestBufferCloseIdempotent(t *testing.T) {
	sink := &fakeSink{}
	b := logstore.NewBuffer("ins_1", sink, 100)
	b.Append("n", "info", "x")
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("Close 应刷出 1 行，实际 %#v", sink.batches)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("重复 Close 不应报错: %v", err)
	}
}

// 守住 Manager 生命周期：同实例复用缓冲，Release 后刷盘并释放，重建序号归零。
func TestManagerGetAndRelease(t *testing.T) {
	sink := &fakeSink{}
	m := logstore.NewManager(sink, 100)

	b1 := m.Get("ins_1")
	if b1 != m.Get("ins_1") {
		t.Fatal("同实例应复用同一个 Buffer")
	}
	if m.Get("ins_2") == b1 {
		t.Fatal("不同实例应各自独立")
	}
	b1.Append("n", "info", "x")

	m.Release(context.Background(), "ins_1")
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("Release 应刷盘，实际 %#v", sink.batches)
	}
	b2 := m.Get("ins_1")
	if b2 == b1 {
		t.Fatal("Release 后再次 Get 应创建新缓冲")
	}
	if line := b2.Append("n", "info", "y"); line.Seq != 1 {
		t.Fatalf("新缓冲 seq 应从 1 开始，实际 %d", line.Seq)
	}

	// 释放不存在的实例不应 panic。
	m.Release(context.Background(), "never-created")
}

// 守住时间戳：日志行必须带写入时刻，SSE 前端按此排序与展示。
func TestBufferLineTimestamp(t *testing.T) {
	b := logstore.NewBuffer("ins_1", nil, 10)
	before := time.Now()
	line := b.Append("n", "info", "x")
	after := time.Now()
	if line.At.Before(before) || line.At.After(after) {
		t.Fatalf("时间戳超出写入区间: %v", line.At)
	}
}
