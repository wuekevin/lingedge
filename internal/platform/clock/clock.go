// Package clock 是 Server 内唯一允许读取系统时钟的位置（Story 1.4 AC5）。
//
// 业务代码一律通过注入的 Clock 取得「现在」：生产装配 System，测试注入 Func（或自己的实现），
// 因此时间相关逻辑（过期、租约、窗口、ULID 时间戳）可被确定性地测试。`make lint` 的 forbidigo 规则禁止
// Server 业务代码（internal/<platform|auth|tenant|…>/，即 spec.go 登记的 Server 模块）里除本包之外直接调用 time.Now / time.Since / time.Until；
// Tools（internal/tools，与 Server 无编译依赖）、cmd、pkg 不在约束范围内。time.After/Sleep/NewTimer 等计时器暂未被禁止
// （后续需要可测试的定时逻辑时再引入抽象）。注意 System().Now() 是墙钟读数（已去除单调时钟部分），不应用于度量耗时。
package clock

import "time"

// Clock 提供「当前时刻」。实现必须并发安全。
type Clock interface {
	Now() time.Time
}

// System 返回读取真实系统时钟的 Clock：UTC、截断到毫秒（数据库 DATETIME(3) 与 API 的 RFC 3339 毫秒精度，architecture §9.1）。
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// Func 把函数适配为 Clock（测试中注入固定/可推进的时间）。
type Func func() time.Time

// Now 实现 Clock。
func (f Func) Now() time.Time { return f() }

// Since 返回自 t 起经过的时间（相对 c.Now()）。业务代码用它代替 time.Since。
func Since(c Clock, t time.Time) time.Duration { return c.Now().Sub(t) }

// Until 返回 t 距 c.Now() 的时间。业务代码用它代替 time.Until。
func Until(c Clock, t time.Time) time.Duration { return t.Sub(c.Now()) }
