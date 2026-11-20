package schedrt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是触发时刻的计算规则（一律 UTC）。
type Schedule interface {
	// Next 返回严格晚于 after 的下一个触发点；无解时返回零值。
	Next(after time.Time) time.Time
	// Prev 返回不晚于 at 的最近一个触发点；无解时返回零值。
	Prev(at time.Time) time.Time
	String() string
}

// ---- Every：对齐 Unix 纪元的固定间隔 ----

type every struct{ d time.Duration }

// Every 返回固定间隔调度：触发点是 Unix 纪元起 d 的整数倍（因此多个实例的窗口天然一致）。d 必须是 >= 1s 的整秒数。
func Every(d time.Duration) (Schedule, error) {
	if d < time.Second || d%time.Second != 0 {
		return nil, errors.New("schedrt: Every 间隔必须是 >= 1s 的整秒数")
	}
	return every{d}, nil
}

func (e every) Next(after time.Time) time.Time {
	s := int64(e.d / time.Second)
	return time.Unix((after.Unix()/s+1)*s, 0).UTC()
}

func (e every) Prev(at time.Time) time.Time {
	s := int64(e.d / time.Second)
	u := at.Unix()
	if u < 0 { // 纪元前不是合法场景
		return time.Time{}
	}
	return time.Unix(u/s*s, 0).UTC()
}

func (e every) String() string { return "@every " + e.d.String() }

// ---- Cron：标准 5 字段（分 时 日 月 周），UTC ----

type cronSpec struct {
	expr             string
	min              uint64
	hour, dom        uint32
	month            uint16
	dow              uint8
	domStar, dowStar bool
}

// ParseCron 解析 5 字段 cron 表达式（`分 时 日 月 周`，UTC）。支持 `*`、`a`、`a-b`、`*/n`、`a-b/n`、`a/n` 与逗号列表；周 0–7（0 与 7 都是周日）。
// 日与周都被限定时取「或」（Vixie cron 语义）。不支持名称（JAN/MON）与 @ 宏——需要固定间隔请用 Every。
func ParseCron(expr string) (Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("schedrt: cron 表达式必须是 5 个字段（分 时 日 月 周）: %q", expr)
	}
	c := &cronSpec{expr: strings.Join(f, " "), domStar: f[2] == "*" || f[2] == "?", dowStar: f[4] == "*" || f[4] == "?"}
	var err error
	if c.min, err = parseField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("schedrt: cron 分钟字段: %w", err)
	}
	h, err := parseField(f[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("schedrt: cron 小时字段: %w", err)
	}
	c.hour = uint32(h)
	d, err := parseField(f[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("schedrt: cron 日字段: %w", err)
	}
	c.dom = uint32(d)
	m, err := parseField(f[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("schedrt: cron 月字段: %w", err)
	}
	c.month = uint16(m)
	w, err := parseField(f[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("schedrt: cron 周字段: %w", err)
	}
	if w&(1<<7) != 0 { // 7 = 周日
		w |= 1
	}
	c.dow = uint8(w & 0x7f)
	return c, nil
}

func parseField(s string, lo, hi int) (uint64, error) {
	if s == "" {
		return 0, errors.New("为空")
	}
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, step, hasStep := strings.Cut(part, "/")
		stepN := 1
		if hasStep {
			n, err := strconv.Atoi(step)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("步长非法 %q", step)
			}
			stepN = n
		}
		a, b := lo, hi
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("范围非法 %q", rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("取值非法 %q", rng)
			}
			a = n
			if hasStep { // a/n = 从 a 到上界每 n 个
				b = hi
			} else {
				b = n
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("取值越界 %q（允许 %d–%d）", part, lo, hi)
		}
		for v := a; v <= b; v += stepN {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

func (c *cronSpec) String() string { return c.expr }

func (c *cronSpec) dayOK(t time.Time) bool {
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	}
	return domOK || dowOK
}

// searchLimit 是 Next/Prev 的搜索上限（8 年足以覆盖 2 月 29 日等稀有组合）。
const searchLimit = 8 * 366 * 24 * time.Hour

func (c *cronSpec) Next(after time.Time) time.Time {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	end := t.Add(searchLimit)
	for t.Before(end) {
		switch {
		case c.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !c.dayOK(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case c.hour&(1<<uint(t.Hour())) == 0:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case c.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (c *cronSpec) Prev(at time.Time) time.Time {
	t := at.UTC().Truncate(time.Minute)
	end := t.Add(-searchLimit)
	for t.After(end) {
		switch {
		case c.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case !c.dayOK(t):
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case c.hour&(1<<uint(t.Hour())) == 0:
			t = t.Truncate(time.Hour).Add(-time.Minute)
		case c.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(-time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
