package service

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
)

// Story 1.22：认证模块读取的设置键、默认值与取值范围的唯一来源是 sysconfig 键注册表。本测试核对它们没有漂移：
// 键都已登记（类型、分组、可 PATCH）、默认值逐项相等、读取范围取自注册表，且 repository.ReadSettings 查询的键集合恰是这些键。
var authKeys = map[string]struct {
	def  time.Duration // 秒类键的默认值；0 表示非秒类
	n    int64         // 计数类键的默认值
	kind sysconfig.ValueType
}{
	KeySessionIdleTTL:     {def: DefaultSessionIdleTTL, kind: sysconfig.TInt},
	KeySessionAbsoluteTTL: {def: DefaultSessionAbsoluteTTL, kind: sysconfig.TInt},
	KeyThrottleWindow:     {def: DefaultThrottleWindow, kind: sysconfig.TInt},
	KeyThrottleAccountMax: {n: DefaultThrottleAccountMax, kind: sysconfig.TInt},
	KeyThrottleIPMax:      {n: DefaultThrottleIPMax, kind: sysconfig.TInt},
	KeyReauthWindow:       {def: DefaultReauthWindow, kind: sysconfig.TInt},
	KeyMFAWindow:          {def: DefaultMFAWindow, kind: sysconfig.TInt},
	KeyAPITokenDefaultTTL: {def: DefaultAPITokenTTL, kind: sysconfig.TInt},
	KeyAPITokenMaxTTL:     {def: DefaultAPITokenMaxTTL, kind: sysconfig.TInt},
	KeyTOTPRequiredAdmin:  {kind: sysconfig.TBool},
	KeyRunMode:            {kind: sysconfig.TEnum},
}

func TestAuthKeysMatchRegistry(t *testing.T) {
	for key, want := range authKeys {
		d, ok := sysconfig.Lookup(key)
		if !ok {
			t.Errorf("认证模块使用的键 %s 未在 sysconfig 注册表登记", key)
			continue
		}
		if d.Type != want.kind {
			t.Errorf("%s: 类型 %s != %s", key, d.Type, want.kind)
		}
		if want.def != 0 {
			if got, _ := d.Default.(int64); got != int64(want.def/time.Second) {
				t.Errorf("%s: 注册表默认 %v != 认证模块默认 %s", key, d.Default, want.def)
			}
			lo, hi := bounds(key)
			if lo != time.Duration(d.Min)*time.Second || hi != time.Duration(d.Max)*time.Second {
				t.Errorf("%s: 读取范围与注册表不一致", key)
			}
			if def := want.def; def < lo || def > hi {
				t.Errorf("%s: 默认值超出自身范围", key)
			}
		}
		if want.n != 0 {
			if got, _ := d.Default.(int64); got != want.n {
				t.Errorf("%s: 注册表默认 %v != 认证模块默认 %d", key, d.Default, want.n)
			}
		}
		if key != KeyRunMode && !d.Patchable {
			t.Errorf("%s 应可经 PATCH /admin/settings 修改", key)
		}
	}
	if DefaultRunMode != sysconfig.DefaultRunMode || DefaultReauthWindow != 300*time.Second {
		t.Error("运行模式默认值/UD-29 再认证窗口 300 秒漂移")
	}
}

// 经注册表校验写入的任何合法边界值，认证模块读取时都会被接受（不会「写得进、读时被当作无效而回退默认」）。
func TestAuthReadAcceptsRegistryBoundaryValues(t *testing.T) {
	for key := range authKeys {
		d, _ := sysconfig.Lookup(key)
		if d.Type != sysconfig.TInt || d.Min == 0 && d.Max == 0 {
			continue
		}
		for _, v := range []int64{d.Min, d.Max} {
			raw := map[string]json.RawMessage{key: json.RawMessage(itoa(v))}
			if _, err := d.Normalize(raw[key]); err != nil {
				t.Fatalf("%s=%d 应通过注册表校验: %v", key, v, err)
			}
			if d.Default != nil {
				if _, isTTL := durationKeys[key]; isTTL {
					got := secs(testLog(), raw, key, time.Second)
					if got != time.Duration(v)*time.Second {
						t.Errorf("%s=%d: 认证模块读取得到 %s（被当作无效回退了）", key, v, got)
					}
				} else if got := count(testLog(), raw, key, 1); int64(got) != v {
					t.Errorf("%s=%d: 认证模块读取得到 %d（被当作无效回退了）", key, v, got)
				}
			}
		}
	}
}

var durationKeys = map[string]bool{KeySessionIdleTTL: true, KeySessionAbsoluteTTL: true, KeyThrottleWindow: true, KeyReauthWindow: true, KeyMFAWindow: true,
	KeyAPITokenDefaultTTL: true, KeyAPITokenMaxTTL: true}

// repository.ReadSettings 的查询键集合 == 认证模块的键集合（门禁式核对源码，防止新增键忘记加进查询）。
func TestReadSettingsQueriesExactlyTheAuthKeys(t *testing.T) {
	src, err := os.ReadFile("../repository/repository.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)FROM system_settings\s+WHERE setting_key IN \((.*?)\)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("找不到 ReadSettings 的 IN 列表")
	}
	var got []string
	for _, k := range regexp.MustCompile(`'([a-z_.]+)'`).FindAllSubmatch(m[1], -1) {
		got = append(got, string(k[1]))
	}
	var want []string
	for k := range authKeys {
		want = append(want, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ReadSettings 键集合 %v != 认证模块键集合 %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ReadSettings 键集合 %v != 认证模块键集合 %v", got, want)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func testLog() *zap.Logger { return zap.NewNop() }
