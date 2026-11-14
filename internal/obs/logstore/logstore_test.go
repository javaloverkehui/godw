package logstore

import (
	"context"
	"strings"
	"testing"
)

type memSink struct {
	lines []Line
}

func (m *memSink) WriteBatch(ctx context.Context, instanceID string, lines []Line) error {
	m.lines = append(m.lines, lines...)
	return nil
}

func (m *memSink) ReadAfter(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]Line, error) {
	out := []Line{}
	for _, l := range m.lines {
		if l.Seq > afterSeq {
			out = append(out, l)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// T0511 验收：断线重连不丢不重（Last-Event-ID 语义）；敏感信息脱敏。
func TestBufferResume(t *testing.T) {
	sink := &memSink{}
	b := NewBuffer("ins_1", sink, 100)
	for i := 0; i < 5; i++ {
		b.Append("n1", "info", "line")
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 客户端已收到 seq=2，重连后应从 3 开始
	lines, err := b.Since(context.Background(), 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].Seq != 3 {
		t.Fatalf("续传结果异常: %+v", lines)
	}
	// 无重复
	seen := map[int64]bool{}
	for _, l := range lines {
		if seen[l.Seq] {
			t.Fatalf("重复行 %d", l.Seq)
		}
		seen[l.Seq] = true
	}
}

func TestBufferFallsBackToSink(t *testing.T) {
	sink := &memSink{lines: []Line{{Seq: 1, Message: "old"}, {Seq: 2, Message: "old2"}}}
	b := NewBuffer("ins_2", sink, 10)
	b.Append("n", "info", "new") // seq=1 冲突，但 Since(0) 应合并去重
	lines, err := b.Since(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("应包含冷热两段: %+v", lines)
	}
}

func TestMask(t *testing.T) {
	cases := []struct{ in, out string }{
		{"password=abc123 host=x", "password=*** host=x"},
		{"Authorization: Bearer abcdef", "Authorization: ***"},
		{"dsn=postgres://user:p@ss@db:5432/x", "dsn=postgres://user:***@db:5432/x"},
		{"api_key: sk-123456", "api_key=***"},
		{"normal log line", "normal log line"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.out {
			t.Fatalf("Mask(%q)=%q，期望 %q", c.in, got, c.out)
		}
	}
}

func TestRingCapacity(t *testing.T) {
	b := NewBuffer("i", nil, 10)
	for i := 0; i < 100; i++ {
		b.Append("n", "info", "x")
	}
	lines, _ := b.Since(context.Background(), 0, 1000)
	if len(lines) > 10 {
		t.Fatalf("环形缓冲应限制在 10 行，实际 %d", len(lines))
	}
	// 最新的行必须在
	last := lines[len(lines)-1]
	if last.Seq != 100 {
		t.Fatalf("最后一行 seq 应为 100，实际 %d", last.Seq)
	}
}

func TestManagerRelease(t *testing.T) {
	sink := &memSink{}
	m := NewManager(sink, 10)
	b := m.Get("ins")
	b.Append("n", "info", "hello")
	m.Release(context.Background(), "ins")
	if len(sink.lines) != 1 {
		t.Fatalf("Release 应刷盘，实际 %d", len(sink.lines))
	}
	if !strings.Contains(sink.lines[0].Message, "hello") {
		t.Fatal("内容不符")
	}
}
