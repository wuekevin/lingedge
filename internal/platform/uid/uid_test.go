package uid

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
)

func fixed(t time.Time) clock.Clock { return clock.Func(func() time.Time { return t }) }

func TestNewProducesValidMonotonicIDs(t *testing.T) {
	now := time.Date(2026, 11, 17, 8, 30, 0, 123_000_000, time.UTC)
	g := NewGenerator(fixed(now)) // 时钟冻结：全部落在同一毫秒，靠单调熵保证严格递增

	const n = 5000
	ids := make([]string, n)
	for i := range ids {
		id, err := g.New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !Valid(id) {
			t.Fatalf("生成值不是合法 ULID: %q", id)
		}
		ids[i] = id
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("同一毫秒内生成的 ULID 必须字典序单调递增")
	}
	for i := 1; i < n; i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("出现重复 ULID: %q", ids[i])
		}
	}
	ts, err := Time(ids[0])
	if err != nil || !ts.Equal(now) {
		t.Fatalf("Time(%q)=%v,%v；期望 %v", ids[0], ts, err, now)
	}
}

func TestTimestampFollowsInjectedClockAndSurvivesBackwardsJump(t *testing.T) {
	cur := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	g := NewGenerator(clock.Func(func() time.Time { return cur }))

	a := g.MustNew()
	cur = cur.Add(5 * time.Second)
	b := g.MustNew()
	cur = cur.Add(-time.Hour) // 时钟回拨
	c := g.MustNew()
	cur = cur.Add(2 * time.Hour)
	d := g.MustNew()

	got := []string{a, b, c, d}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("回拨后序列必须仍不降序: %v", got)
	}
	tb, _ := Time(b)
	tc, _ := Time(c)
	if !tc.Equal(tb) {
		t.Errorf("回拨期间应沿用已见过的最大时间戳 %v，实际 %v", tb, tc)
	}
	td, _ := Time(d)
	if want := time.Date(2026, 11, 17, 9, 0, 5, 0, time.UTC); !td.Equal(want) {
		t.Errorf("恢复后时间戳=%v，期望 %v", td, want)
	}
}

func TestNewConcurrentNoDuplicates(t *testing.T) {
	g := NewGenerator(clock.System())
	const workers, per = 16, 500
	var wg sync.WaitGroup
	out := make(chan string, workers*per)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				out <- g.MustNew()
			}
		}()
	}
	wg.Wait()
	close(out)
	seen := map[string]bool{}
	for id := range out {
		if seen[id] {
			t.Fatalf("并发生成出现重复: %q", id)
		}
		seen[id] = true
	}
	if len(seen) != workers*per {
		t.Fatalf("数量不符: %d", len(seen))
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("熵源故障") }

func TestEntropyFailureIsReportedNotSwallowed(t *testing.T) {
	g := NewGeneratorWithEntropy(fixed(time.Unix(1_700_000_000, 0)), failingReader{})
	if id, err := g.New(); err == nil {
		t.Fatalf("熵源失败必须返回错误，而不是静默产出 %q", id)
	}
	defer func() {
		if recover() == nil {
			t.Error("MustNew 在熵源失败时必须 panic")
		}
	}()
	g.MustNew()
}

func TestEntropyOverflowReturnsErrorNeverDuplicates(t *testing.T) {
	// 全 0xFF 熵源：同一毫秒内第二次递增必然溢出，必须报错而不是回绕产生重复/倒序值。
	g := NewGeneratorWithEntropy(fixed(time.Unix(1_700_000_000, 0)), io.MultiReader(bytes.NewReader(bytes.Repeat([]byte{0xFF}, 10)), failingReader{}))
	first, err := g.New()
	if err != nil {
		t.Fatalf("首次生成应成功: %v", err)
	}
	if second, err := g.New(); err == nil {
		t.Fatalf("同毫秒熵溢出必须返回错误，实际返回 %q（首个 %q）", second, first)
	}
}

func TestValidNormalizeTime(t *testing.T) {
	const good = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	cases := []struct {
		name       string
		in         string
		valid      bool
		normalized string
		normOK     bool
	}{
		{"规范大写", good, true, good, true},
		{"小写可规整但不算规范", strings.ToLower(good), false, good, true},
		{"过短", good[:25], false, "", false},
		{"过长", good + "0", false, "", false},
		{"含非法字符 U", "01ARZ3NDEKTSV4RRFFQ69G5FAU", false, "", false},
		{"含非法字符 I", "01ARZ3NDEKTSV4RRFFQ69G5FAI", false, "", false},
		{"含非法字符 L", "01ARZ3NDEKTSV4RRFFQ69G5FAL", false, "", false},
		{"含非法字符 O", "01ARZ3NDEKTSV4RRFFQ69G5FAO", false, "", false},
		{"首位超过 7（128 位溢出）", "81ARZ3NDEKTSV4RRFFQ69G5FAV", false, "", false},
		{"空串", "", false, "", false},
		{"含空白", " " + good[1:], false, "", false},
		{"含注入字符", "01ARZ3NDEKTSV4RRFFQ69G5F\r\n", false, "", false},
	}
	for _, tc := range cases {
		if got := Valid(tc.in); got != tc.valid {
			t.Errorf("%s: Valid(%q)=%v，期望 %v", tc.name, tc.in, got, tc.valid)
		}
		n, ok := Normalize(tc.in)
		if ok != tc.normOK || n != tc.normalized {
			t.Errorf("%s: Normalize(%q)=(%q,%v)，期望 (%q,%v)", tc.name, tc.in, n, ok, tc.normalized, tc.normOK)
		}
		if _, err := Time(tc.in); (err == nil) != tc.normOK {
			t.Errorf("%s: Time(%q) err=%v，期望成功=%v", tc.name, tc.in, err, tc.normOK)
		}
	}
	// 已知向量：官方示例 ULID 的时间戳 1469922850259 ms。
	ts, err := Time(good)
	if err != nil || ts.UnixMilli() != 1469922850259 {
		t.Errorf("Time(%q)=%v,%v；期望 UnixMilli=1469922850259", good, ts, err)
	}
}
