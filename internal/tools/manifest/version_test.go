package manifest

import "testing"

func TestParseVersionStrict(t *testing.T) {
	ok := []string{"0.0.0", "0.1.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0+build.5", "1.0.0-rc.1+build.5", "999999.999999.999999"}
	for _, s := range ok {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("%q 应合法: %v", s, err)
		}
	}
	bad := []string{"", "1", "1.2", "v1.2.3", "V1.2.3", "01.2.3", "1.02.3", "1.2.03", "1.2.3.4", "1.2.3-", "1.2.3-01", "1.2.3+", "1.2.3-rc..1", " 1.2.3", "1.2.3 ", "1.2.3\n", "a.b.c", "-1.2.3", "1.2.3-é", "1.2.x", "*"}
	for _, s := range bad {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("%q 应非法", s)
		}
	}
}

func TestVersionCompareSemverPrecedence(t *testing.T) {
	// SemVer 2.0.0 §11 的优先级链（严格递增）。
	chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0"}
	for i := range chain {
		for j := range chain {
			a, b := mustVer(t, chain[i]), mustVer(t, chain[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s,%s)=%d 期望 %d", chain[i], chain[j], got, want)
			}
		}
	}
	// 数值比较而非字典序；构建元数据不参与优先级。
	if mustVer(t, "1.10.0").Compare(mustVer(t, "1.9.0")) != 1 {
		t.Error("1.10.0 应大于 1.9.0")
	}
	if mustVer(t, "1.0.0+a").Compare(mustVer(t, "1.0.0+b")) != 0 {
		t.Error("构建元数据应被忽略")
	}
	if !mustVer(t, "1.0.0-rc.1").Prerelease() || mustVer(t, "1.0.0").Prerelease() {
		t.Error("Prerelease 判定错误")
	}
}

func TestConstraintBoundaries(t *testing.T) {
	type c struct {
		cons string
		ver  string
		want bool
	}
	cases := []c{
		// tools-design §9.2 / §10 示例约束。
		{">=8.0.16 <9", "8.0.16", true}, {">=8.0.16 <9", "8.0.15", false}, {">=8.0.16 <9", "8.0.17", true},
		{">=8.0.16 <9", "8.99.99", true}, {">=8.0.16 <9", "9.0.0", false}, {">=8.0.16 <9", "9.0.0-rc.1", false},
		{">=8", "8.0.0", true}, {">=8", "7.99.99", false}, {">=1 <2", "1.999.999", true}, {">=1 <2", "2.0.0", false}, {">=1 <2", "0.9.9", false},
		// 比较算符边界。
		{">1.2.3", "1.2.3", false}, {">1.2.3", "1.2.4", true},
		{"<1.2.3", "1.2.3", false}, {"<1.2.3", "1.2.2", true},
		{"<=1.2.3", "1.2.3", true}, {"<=1.2.3", "1.2.4", false},
		{"=1.2.3", "1.2.3", true}, {"=1.2.3", "1.2.4", false},
		{"1.2.3", "1.2.3", true}, {"1.2.3", "1.2.2", false},
		// ^：保持最左非零段不变。
		{"^1.2.3", "1.2.3", true}, {"^1.2.3", "1.9.9", true}, {"^1.2.3", "2.0.0", false}, {"^1.2.3", "1.2.2", false},
		{"^0.2.3", "0.2.9", true}, {"^0.2.3", "0.3.0", false}, {"^0.0.3", "0.0.3", true}, {"^0.0.3", "0.0.4", false},
		// ~：只允许 patch 升级。
		{"~1.2.3", "1.2.3", true}, {"~1.2.3", "1.2.99", true}, {"~1.2.3", "1.3.0", false}, {"~1.2.3", "1.2.2", false},
		// 空格 = AND，|| = OR。
		{">=1.0.0 <1.5.0 || >=2.0.0 <2.5.0", "1.4.9", true}, {">=1.0.0 <1.5.0 || >=2.0.0 <2.5.0", "1.5.0", false},
		{">=1.0.0 <1.5.0 || >=2.0.0 <2.5.0", "2.0.0", true}, {">=1.0.0 <1.5.0 || >=2.0.0 <2.5.0", "2.5.0", false},
		{">=1.0.0 <1.5.0 || >=2.0.0 <2.5.0", "1.7.0", false},
		// 预发布语义（库行为，被固定）：约束不含预发布段 → 预发布版本不满足。
		{">=1.0.0", "1.1.0-rc.1", false}, {">=1.0.0 <2.0.0", "1.5.0-beta", false},
		{">=1.1.0-rc.1", "1.1.0-rc.2", true}, {">=1.1.0-rc.1", "1.1.0", true}, {">=1.1.0-rc.1", "1.1.0-rc.0", false},
		{"=1.0.0-rc.1", "1.0.0-rc.1", true}, {"=1.0.0-rc.1", "1.0.0", false},
		// 0 与大数。
		{">=0.0.0", "0.0.0", true}, {"<0.0.1", "0.0.0", true}, {">=18446744073709", "18446744073709.0.0", true},
		// 构建元数据不参与比较。
		{"=1.0.0", "1.0.0+build.9", true},
	}
	for _, tc := range cases {
		cons, err := ParseConstraint(tc.cons)
		if err != nil {
			t.Errorf("约束 %q 应合法: %v", tc.cons, err)
			continue
		}
		if got := cons.Check(mustVer(t, tc.ver)); got != tc.want {
			t.Errorf("%q 对 %s = %v，期望 %v", tc.cons, tc.ver, got, tc.want)
		}
	}
}

func TestConstraintRejectsOutsideSubset(t *testing.T) {
	bad := []string{
		"", " ", "||", ">=1.0.0 ||", "|| >=1.0.0", ">=1.0.0 || || <2",
		"*", "x", "1.x", "1.2.x", "1.*", ">=1.x",
		"!=1.2.3", "1.2.3 - 2.0.0", ">=1.0.0, <2.0.0", ">= 1.0.0", ">=", "^", "~", "latest", "v1.0.0", ">=v1.0.0",
		">=01.0.0", ">=1.0.0\n<2.0.0 ;", "<9 && >=8", ">=1.0.0 <2.0.0 @",
	}
	for _, s := range bad {
		if _, err := ParseConstraint(s); err == nil {
			t.Errorf("%q 应被拒绝", s)
		}
	}
	long := ">=1.0.0"
	for len(long) <= maxConstraintLen {
		long += " <99.0.0"
	}
	if _, err := ParseConstraint(long); err == nil {
		t.Error("超长约束应被拒绝")
	}
	// 零值 Constraint 不满足任何版本（失败关闭）。
	if (Constraint{}).Check(mustVer(t, "1.0.0")) {
		t.Error("零值约束不得放行")
	}
	// 部分版本约束（>=8 / <9 / ^1.2）按库语义补零，边界固定。
	for cons, vs := range map[string]map[string]bool{
		"<9":   {"8.99.99": true, "9.0.0": false},
		">=8":  {"8.0.0": true, "7.9.9": false},
		"^1.2": {"1.2.0": true, "1.99.0": true, "2.0.0": false, "1.1.9": false},
		"~1.2": {"1.2.9": true, "1.3.0": false},
	} {
		c, err := ParseConstraint(cons)
		if err != nil {
			t.Fatal(err)
		}
		for v, want := range vs {
			if c.Check(mustVer(t, v)) != want {
				t.Errorf("%q 对 %s 应为 %v", cons, v, want)
			}
		}
	}
}

func FuzzParseConstraint(f *testing.F) {
	for _, s := range []string{">=8.0.16 <9", "^1.2.3", "~1", ">=1 <2 || >=3", "", "||", "1.x", ">=1.0.0-rc.1"} {
		f.Add(s)
	}
	v := Version{}
	if pv, err := ParseVersion("1.2.3"); err == nil {
		v = pv
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseConstraint(s) // 不得 panic
		if err == nil {
			_ = c.Check(v)
			_ = c.String()
		}
	})
}

func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"1.2.3", "1.0.0-rc.1+b", "", "v1", "01.0.0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err == nil {
			// 往返：规范形式必须能再次解析且相等。
			v2, err2 := ParseVersion(v.String())
			if err2 != nil || v2.Compare(v) != 0 {
				t.Fatalf("往返失败: %q → %q", s, v.String())
			}
		}
	})
}

func FuzzParseManifest(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"channel":"stable","sequence":1,"generated_at":"2026-01-01T00:00:00Z","valid_until":"2026-04-01T00:00:00Z","revoked_versions":[],"components":{}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseManifest(b) // 不得 panic；成功时不变量成立
		if err == nil && (m.Sequence < 1 || !m.ValidUntil.After(m.GeneratedAt)) {
			t.Fatalf("通过校验的清单违反不变量: %+v", m)
		}
	})
}

func FuzzParseSig(f *testing.F) {
	f.Add([]byte(`{"key_id":"a","alg":"ed25519","sig":"AAAA"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if s, err := ParseSig(b); err == nil && len(s.sig) != 64 {
			t.Fatal("通过解析的签名必须是 64 字节")
		}
	})
}
