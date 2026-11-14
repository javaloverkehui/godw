package clock

import "time"

// Clock 抽象时间源，调度与租约逻辑依赖它才能在测试中快进。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Real 返回系统时钟。
func Real() Clock { return realClock{} }

// Fixed 用于测试：返回固定时间。
type Fixed struct{ T time.Time }

func (f Fixed) Now() time.Time { return f.T }

// Travel 用于测试：从 Base 起每次调用推进 Step。
type Travel struct {
	Base time.Time
	Step time.Duration
	n    int
}

func (t *Travel) Now() time.Time {
	cur := t.Base.Add(time.Duration(t.n) * t.Step)
	t.n++
	return cur
}

// UTC 统一返回 UTC 截断到秒，避免各库时区差异导致的实例时间不一致。
func UTC(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }
