// Package uid 生成与校验对外资源标识 uid（ULID，CHAR(26)，architecture §9.1）。
//
// 生成器单调：同一毫秒内连续生成的值严格递增；时钟回拨时沿用已见过的最大时间戳，保证输出序列不降序。
// 时钟与熵源均可注入，业务代码不直接读取系统时间或 crypto/rand。
package uid

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/wuekevin/lingedge/internal/platform/clock"
)

// Len 是 ULID 文本长度。
const Len = ulid.EncodedSize

// Generator 是并发安全的单调 ULID 生成器。
type Generator struct {
	clock clock.Clock

	mu      sync.Mutex
	entropy io.Reader
	lastMs  uint64
}

// NewGenerator 使用 c 作为时间来源、crypto/rand 作为熵源。
func NewGenerator(c clock.Clock) *Generator {
	return NewGeneratorWithEntropy(c, rand.Reader)
}

// NewGeneratorWithEntropy 允许注入熵源（仅用于确定性测试；生产装配使用 NewGenerator）。
func NewGeneratorWithEntropy(c clock.Clock, entropy io.Reader) *Generator {
	return &Generator{clock: c, entropy: ulid.Monotonic(entropy, 0)}
}

// New 返回新的 ULID 文本（26 位大写 Crockford base32）。熵源失败或同毫秒熵溢出时返回错误，不会返回重复值。
func (g *Generator) New() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := ulid.Timestamp(g.clock.Now())
	if ms < g.lastMs { // 时钟回拨：保持时间戳不降，单调熵在同一毫秒内继续递增
		ms = g.lastMs
	}
	id, err := ulid.New(ms, g.entropy)
	if err != nil {
		return "", err
	}
	g.lastMs = ms
	return id.String(), nil
}

// MustNew 与 New 相同，但失败时 panic。crypto/rand 失败属于进程级不可恢复错误，仅适合进程启动期装配与测试；
// 请求路径上用 New 并把错误按 500 处理（requestid 即如此）。
func (g *Generator) MustNew() string {
	s, err := g.New()
	if err != nil {
		panic("uid: 生成 ULID 失败: " + err.Error())
	}
	return s
}

// Valid 报告 s 是否为规范 ULID：恰好 26 位、大写 Crockford base32、未超过 128 位上界。
func Valid(s string) bool {
	if len(s) != Len || s != strings.ToUpper(s) {
		return false
	}
	_, err := ulid.ParseStrict(s)
	return err == nil
}

// Normalize 把合法 ULID（允许小写输入）规整为大写规范形式；非法返回 false。
func Normalize(s string) (string, bool) {
	if len(s) != Len {
		return "", false
	}
	u := strings.ToUpper(s)
	if _, err := ulid.ParseStrict(u); err != nil {
		return "", false
	}
	return u, true
}

// ErrInvalid 表示不是合法 ULID。
var ErrInvalid = errors.New("uid: 不是合法的 ULID")

// Time 取出 ULID 内嵌的毫秒时间戳（UTC）。
func Time(s string) (time.Time, error) {
	u, ok := Normalize(s)
	if !ok {
		return time.Time{}, ErrInvalid
	}
	id, err := ulid.ParseStrict(u)
	if err != nil {
		return time.Time{}, ErrInvalid
	}
	return ulid.Time(id.Time()).UTC(), nil
}
