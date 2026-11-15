// Package logstore 负责运行日志的采集、脱敏、缓存与归档。
//
// 设计取舍：日志不直接写 DB（每条 INSERT 在万行日志下会成为瓶颈），
// 而是「内存缓冲 + 批量写」，读路径则分两段：热数据在内存环形缓冲，冷数据查 DB。
// 这样 SSE 首行延迟压到 1s 内，同时不牺牲可回溯性。
package logstore

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Line 是一条日志。
//
// InstanceID / TaskID 是「捞出来还能对上号」的关键：日志会被导出、被贴进工单、
// 被拿去和 task_instance 对耗时。只靠「在哪个页面看到」记不住归属，
// 所以这两个标识跟着每一行走，而不是只写在页面标题上。
type Line struct {
	Seq        int64     `json:"seq"`
	InstanceID string    `json:"instanceId,omitempty"`
	TaskID     string    `json:"taskId,omitempty"`
	NodeID     string    `json:"nodeId,omitempty"`
	Level      string    `json:"level"`
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
}

// Sink 是持久化抽象。BatchWriter 周期调用它。
type Sink interface {
	WriteBatch(ctx context.Context, instanceID string, lines []Line) error
	ReadAfter(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]Line, error)
}

// SeqReader 是可选的「续号」能力。
//
// 实例重跑（节点重跑 / 质量放行）会复用同一个 instance_id，而日志缓冲在上一轮
// 结束时已经释放。新缓冲如果从 1 重新编号，就会和库里已有的 seq 撞车，
// 前端按 seq 游标读日志时会漏行。实现这个接口即可在上电时从 MAX(seq) 续号。
type SeqReader interface {
	MaxSeq(ctx context.Context, instanceID string) (int64, error)
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
	seqOnce   sync.Once
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

// Append 追加一行不带任务归属的日志（实例级日志或旧调用点）。
func (b *Buffer) Append(nodeID, level, msg string) Line {
	return b.AppendTask(nodeID, "", level, msg)
}

// AppendTask 追加一行。超过阈值立即刷盘，否则等定时器。
// taskID 是该日志所属的 task_instance.id（实例级日志为空）。
func (b *Buffer) AppendTask(nodeID, taskID, level, msg string) Line {
	// 首行之前先确认库里已有的最大 seq：重跑的实例不能从 1 重新编号。
	b.loadSeq()
	b.mu.Lock()
	b.seq++
	line := Line{
		Seq: b.seq, InstanceID: b.instanceID, TaskID: taskID, NodeID: nodeID,
		Level:   normalizeLevel(level),
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

// loadSeq 在首次写入前把 seq 对齐到持久层的最大值。
// 只做一次；查询失败就退回从 1 开始——续号失败不该阻塞执行。
func (b *Buffer) loadSeq() {
	b.seqOnce.Do(func() {
		sr, ok := b.sink.(SeqReader)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		n, err := sr.MaxSeq(ctx, b.instanceID)
		if err != nil || n <= 0 {
			return
		}
		b.mu.Lock()
		if n > b.seq {
			b.seq = n
		}
		b.mu.Unlock()
	})
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

// MaxReadLimit 是一次读取允许的最大行数。
// 详情页要「从前往后完整捞一遍」，200 行的旧上限会让 ERROR 落在窗口外，
// 于是筛选 ERROR 反而显示「暂无日志」——上限必须留给调用方决定。
const MaxReadLimit = 20000

// Since 返回 seq 之后的行，优先读内存，不足再补 DB。
// 这是 SSE 断点续传的关键：客户端带 Last-Event-ID 重连时，不丢不重。
func (b *Buffer) Since(ctx context.Context, afterSeq int64, limit int) ([]Line, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > MaxReadLimit {
		limit = MaxReadLimit
	}
	b.mu.Lock()
	var hot []Line
	for _, l := range b.ring {
		if l.Seq > afterSeq {
			hot = append(hot, l)
		}
	}
	b.mu.Unlock()

	// 热区够一次交付、且「从 afterSeq+1 开始连续」时，才可以不回源。
	// 只判断 len(hot) >= limit 是错的：环形缓冲丢弃前半段后，热区仍可能有
	// 几千行，但开头的日志已经在 DB 里，直接返回会让详情页永远看不到启动阶段。
	if len(hot) >= limit && hot[0].Seq <= afterSeq+1 {
		return hot[:limit], nil
	}
	if b.sink == nil {
		if len(hot) > limit {
			hot = hot[:limit]
		}
		return hot, nil
	}
	// 内存里不连续（或为空），回源补齐缺口。
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
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
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
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "error", "err", "fatal", "critical", "panic":
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

// Get 返回（并按需创建）实例的缓冲。写入路径用它——先有缓冲，日志才有地方落。
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

// Existing 只返回已存在的缓冲，不创建。
//
// 读路径必须用它：看一条三天前的实例不该在内存里留下一个空缓冲，
// 更不该让「读日志」这个动作制造出新的状态。
func (m *Manager) Existing(instanceID string) *Buffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buffers[instanceID]
}

// Cold 直接从持久层读取，用于缓冲已释放的历史实例。
func (m *Manager) Cold(ctx context.Context, instanceID string, afterSeq int64, limit int) ([]Line, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > MaxReadLimit {
		limit = MaxReadLimit
	}
	if m.sink == nil {
		return []Line{}, nil
	}
	return m.sink.ReadAfter(ctx, instanceID, afterSeq, limit)
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
