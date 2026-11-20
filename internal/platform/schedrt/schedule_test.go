package schedrt

import (
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestEveryAlignsToEpoch(t *testing.T) {
	s, err := Every(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(ts("2026-11-21 10:00:07")); !got.Equal(ts("2026-11-21 10:00:10")) {
		t.Fatalf("Next: %v", got)
	}
	if got := s.Next(ts("2026-11-21 10:00:10")); !got.Equal(ts("2026-11-21 10:00:20")) {
		t.Fatalf("Next 严格晚于: %v", got)
	}
	if got := s.Prev(ts("2026-11-21 10:00:19")); !got.Equal(ts("2026-11-21 10:00:10")) {
		t.Fatalf("Prev: %v", got)
	}
	if got := s.Prev(ts("2026-11-21 10:00:10")); !got.Equal(ts("2026-11-21 10:00:10")) {
		t.Fatalf("Prev 含当前: %v", got)
	}
	for _, bad := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, -time.Second} {
		if _, err := Every(bad); err == nil {
			t.Errorf("Every(%v) 应被拒绝", bad)
		}
	}
}

func TestCronNextPrev(t *testing.T) {
	cases := []struct {
		expr, from, next, prev string
	}{
		{"17 3 * * *", "2026-11-21 10:00:00", "2026-11-22 03:17:00", "2026-11-21 03:17:00"},
		{"17 3 * * *", "2026-11-21 03:17:00", "2026-11-22 03:17:00", "2026-11-21 03:17:00"},
		{"*/15 * * * *", "2026-11-21 10:07:30", "2026-11-21 10:15:00", "2026-11-21 10:00:00"},
		{"0 0 1 * *", "2026-11-21 10:00:00", "2026-12-01 00:00:00", "2026-11-01 00:00:00"},
		{"30 2 29 2 *", "2026-11-21 10:00:00", "2028-02-29 02:30:00", "2024-02-29 02:30:00"},
		{"0 9 * * 1", "2026-11-21 10:00:00", "2026-11-23 09:00:00", "2026-11-16 09:00:00"}, // 2026-11-21 是周六
		{"0 9 * * 7", "2026-11-21 10:00:00", "2026-11-22 09:00:00", "2026-11-15 09:00:00"}, // 7 = 周日
		{"0,30 8-9 * * *", "2026-11-21 09:30:00", "2026-11-22 08:00:00", "2026-11-21 09:30:00"},
		{"0 0 13 * 5", "2026-11-21 10:00:00", "2026-11-27 00:00:00", "2026-11-20 00:00:00"}, // 日与周同时限定取「或」
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(ts(c.from)); !got.Equal(ts(c.next)) {
			t.Errorf("%s Next(%s) = %v，期望 %s", c.expr, c.from, got, c.next)
		}
		if got := s.Prev(ts(c.from)); !got.Equal(ts(c.prev)) {
			t.Errorf("%s Prev(%s) = %v，期望 %s", c.expr, c.from, got, c.prev)
		}
	}
}

func TestCronRejectsInvalid(t *testing.T) {
	for _, e := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "a * * * *", "*/0 * * * *", "5-1 * * * *", "@daily", "1-2-3 * * * *"} {
		if _, err := ParseCron(e); err == nil {
			t.Errorf("ParseCron(%q) 应被拒绝", e)
		}
	}
}
