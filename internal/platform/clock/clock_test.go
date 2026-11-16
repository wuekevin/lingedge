package clock

import (
	"testing"
	"time"
)

func TestSystemClockIsUTCMillisecondPrecision(t *testing.T) {
	for i := 0; i < 50; i++ {
		now := System().Now()
		if now.Location() != time.UTC {
			t.Fatalf("System().Now() 必须为 UTC，实际 %v", now.Location())
		}
		if now.Nanosecond()%int(time.Millisecond) != 0 {
			t.Fatalf("System().Now() 必须截断到毫秒，实际 ns=%d", now.Nanosecond())
		}
	}
	// 与真实时间同量级（防止误返回零值）。
	if d := time.Since(System().Now()); d < 0 || d > time.Minute {
		t.Fatalf("System().Now() 偏离真实时间过大: %v", d)
	}
}

func TestFuncAdaptsAndSinceUntilUseInjectedTime(t *testing.T) {
	base := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	cur := base
	var c Clock = Func(func() time.Time { return cur })

	cases := []struct {
		name      string
		advance   time.Duration
		since     time.Duration // Since(c, base)
		untilNext time.Duration // Until(c, base+1h)
	}{
		{"零推进", 0, 0, time.Hour},
		{"推进 90 秒", 90 * time.Second, 90 * time.Second, time.Hour - 90*time.Second},
		{"越过目标时刻", 2 * time.Hour, 2 * time.Hour, -time.Hour},
	}
	for _, tc := range cases {
		cur = base.Add(tc.advance)
		if got := c.Now(); !got.Equal(cur) {
			t.Errorf("%s: Now()=%v，期望 %v", tc.name, got, cur)
		}
		if got := Since(c, base); got != tc.since {
			t.Errorf("%s: Since=%v，期望 %v", tc.name, got, tc.since)
		}
		if got := Until(c, base.Add(time.Hour)); got != tc.untilNext {
			t.Errorf("%s: Until=%v，期望 %v", tc.name, got, tc.untilNext)
		}
	}
}
