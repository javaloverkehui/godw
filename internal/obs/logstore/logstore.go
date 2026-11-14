// Package logstore 负责运行日志的采集、脱敏、缓存与归档。
//
// 设计取舍：日志不直接写 DB（每条 INSERT 在万行日志下会成为瓶颈），
// 而是「内存缓冲 + 批量写」，读路径则分两段：热数据在内存环形缓冲，冷数据查 DB。
// 这样 SSE 首行延迟压到 1s 内，同时不牺牲可回溯性。
package logstore

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Line 是一条日志。
type Line struct {
	Seq     int64     `json:"seq"`
	NodeID  string    `json:"nodeId,omitempty"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Sink 是持久化抽象。BatchWriter 周期调用它。
type Sink interface {
	WriteBatch(ctx context.Context, instanceID string, lines []Line) error
	ReadAfter(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]Line, error)
}

// 脱敏：命中即整行替换敏感片段，而不是只遮住值——
// 日志里最常见的泄漏是「连接串里带密码」和「Authorization: Bearer xxx」。
var maskRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[=:]\s*[^\s,;'"]+`), `${1}=***`},
	{regexp.MustCompile(`(?i)(token|secret|api[_-]?key)\s*[=:]\s*[^\s,;'"]+`), `${1}=***`},
	{regexp.MustCompile(`(?i)authorization:\s*\S+(\s+\S+)?`), `Authorization: ***`},
	{regexp.MustCompile(`(?i)(://[^:/\s]+:)[^/\s]*@`), `${1}***@`},
}

// Mask 对单行做脱敏。
func Mask(s string) string {
	for _, r := range maskRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Buffer 是单实例的内存环形缓冲 + 批量写。
type Buffer struct {
	instanceID string
	sink       Sink
	capacity   int
	flushLines int
	flushEvery time.Duration

	mu        sync.Mutex
	ring      []Line
	seq       int64
	pending   []Line
	lastFlush time.Time
	stop      chan struct{}
	once      sync.Once
}

// NewBuffer 创建缓冲。capacity 决定热数据保留行数。
func NewBuffer(instanceID string, sink Sink, capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 2000
	}
	b := &Buffer{
		instanceID: instanceID,
		sink:       sink,
		capacity:   capacity,
		flushLines: 200,
		flushEvery: time.Second,
		lastFlush:  time.Now(),
		stop:       make(chan struct{}),
	}
	return b
}

// Append 追加一行。超过阈值立即刷盘，否则等定时器。
func (b *Buffer) Append(nodeID, level, msg string) Line {
	b.mu.Lock()
	b.seq++
	line := Line{
		Seq: b.seq, NodeID: nodeID, Level: normalizeLevel(level),
		Message: Mask(strings.TrimRight(msg, "\n")), At: time.Now(),
	}
	b.ring = append(b.ring, line)
	if len(b.ring) > b.capacity {
		// 丢弃最旧的一半，避免每行都做切片拷贝。
		b.ring = append([]Line{}, b.ring[len(b.ring)-b.capacity/2:]...)
	}
	b.pending = append(b.pending, line)
	shouldFlush := len(b.pending) >= b.flushLines || time.Since(b.lastFlush) >= b.flushEvery
	b.mu.Unlock()

	if shouldFlush {
		_ = b.Flush(context.Background())
	}
	return line
}

// Flush 把 pending 落库。
func (b *Buffer) Flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return nil
	}
	batch := b.pending
	b.pending = nil
	b.lastFlush = time.Now()
	b.mu.Unlock()

	if b.sink == nil {
		return nil
	}
	return b.sink.WriteBatch(ctx, b.instanceID, batch)
}

// Since 返回 seq 之后的行，优先读内存，不足再补 DB。
// 这是 SSE 断点续传的关键：客户端带 Last-Event-ID 重连时，不丢不重。
func (b *Buffer) Since(ctx context.Context, afterSeq int64, limit int) ([]Line, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	b.mu.Lock()
	var hot []Line
	for _, l := range b.ring {
		if l.Seq > afterSeq {
			hot = append(hot, l)
		}
	}
	b.mu.Unlock()
	if len(hot) >= limit {
		return hot[:limit], nil
	}
	if b.sink == nil {
		return hot, nil
	}
	// 内存里不够，说明客户端落后太多，回源查 DB。
	cold, err := b.sink.ReadAfter(ctx, b.instanceID, afterSeq, limit)
	if err != nil {
		return hot, err
	}
	// 合并去重：DB 可能已经包含了内存里的行。
	seen := map[int64]bool{}
	out := make([]Line, 0, len(cold)+len(hot))
	for _, l := range cold {
		if !seen[l.Seq] {
			seen[l.Seq] = true
			out = append(out, l)
		}
	}
	for _, l := range hot {
		if !seen[l.Seq] {
			seen[l.Seq] = true
			out = append(out, l)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Close 停止定时器并做最后一次刷盘。
func (b *Buffer) Close(ctx context.Context) error {
	var err error
	b.once.Do(func() { close(b.stop) })
	err = b.Flush(ctx)
	return err
}

func normalizeLevel(l string) string {
	switch strings.ToLower(l) {
	case "error", "err":
		return "error"
	case "warn", "warning":
		return "warn"
	case "debug":
		return "debug"
	}
	return "info"
}

// Manager 管理多个实例的 Buffer，实例结束后释放。
type Manager struct {
	mu      sync.Mutex
	buffers map[string]*Buffer
	sink    Sink
	cap     int
}

func NewManager(sink Sink, capacity int) *Manager {
	return &Manager{buffers: map[string]*Buffer{}, sink: sink, cap: capacity}
}

// Get 返回（并按需创建）实例的缓冲。
func (m *Manager) Get(instanceID string) *Buffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.buffers[instanceID]; ok {
		return b
	}
	b := NewBuffer(instanceID, m.sink, m.cap)
	m.buffers[instanceID] = b
	return b
}

// Release 在实例结束后调用：刷盘并从内存移除，避免长期运行后内存增长。
func (m *Manager) Release(ctx context.Context, instanceID string) {
	m.mu.Lock()
	b, ok := m.buffers[instanceID]
	delete(m.buffers, instanceID)
	m.mu.Unlock()
	if ok {
		_ = b.Close(ctx)
	}
}
