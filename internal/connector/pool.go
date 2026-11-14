package connector

import (
	"context"
	"time"
)

// Pool 按 datasource_id 复用连接器，避免每个任务新建连接。
// 空闲超过 TTL 的连接会被关闭，防止数据源侧连接泄漏。
type Pool struct {
	ttl   time.Duration
	items map[string]*poolItem
	open  func(ctx context.Context, spec Spec) (Connector, error)
}

type poolItem struct {
	c        Connector
	lastUsed time.Time
}

func NewPool(ttl time.Duration) *Pool {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Pool{ttl: ttl, items: map[string]*poolItem{}, open: Open}
}

// Acquire 返回可复用的连接器；不存在则新建。
func (p *Pool) Acquire(ctx context.Context, spec Spec) (Connector, error) {
	if it, ok := p.items[spec.ID]; ok {
		it.lastUsed = time.Now()
		return it.c, nil
	}
	c, err := p.open(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.items[spec.ID] = &poolItem{c: c, lastUsed: time.Now()}
	return c, nil
}

// Invalidate 在数据源配置变更或连接报错时调用，强制下次重建。
func (p *Pool) Invalidate(id string) {
	if it, ok := p.items[id]; ok {
		_ = it.c.Close()
		delete(p.items, id)
	}
}

// Reap 关闭空闲超时的连接，由后台 goroutine 周期调用。
func (p *Pool) Reap(now time.Time) int {
	n := 0
	for id, it := range p.items {
		if now.Sub(it.lastUsed) > p.ttl {
			_ = it.c.Close()
			delete(p.items, id)
			n++
		}
	}
	return n
}

func (p *Pool) Close() {
	for id := range p.items {
		p.Invalidate(id)
	}
}

func (p *Pool) Len() int { return len(p.items) }

// Registry 只读暴露已注册类型，供 /api/v1/datasource-types 使用。
func Registry() map[string]any {
	out := map[string]any{}
	for _, t := range Types() {
		out[t] = map[string]any{"type": t}
	}
	return out
}
