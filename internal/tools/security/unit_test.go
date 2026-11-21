package security

import "testing"

func TestValidateUnitValue(t *testing.T) {
	good := []string{
		"lingedge", "lingedge-node", "node_1", "1.2.3", "v1.2.3-rc.1+x"[:10], "host.example.com", "127.0.0.1:8080", "user@host",
		"/usr/bin/lingedge", "/opt/lingedge/releases/1.2.3/bin/lingedge-node", "/etc/lingedge/node.yaml", "/", "a.b", "x..y"[:1],
	}
	for _, v := range good {
		if err := ValidateUnitValue(v); err != nil {
			t.Errorf("%q 应通过: %v", v, err)
		}
	}
	bad := []string{
		"", " ", "a b", "a\tb", "a\nb", "a\r", "a\x00b", "a%b", "%n", "%h", "100%", "a;b", "a|b", "a&b", "$HOME", "${X}", "`id`", "$(id)",
		"a\"b", "a'b", "a\\b", "a=b", "a,b", "a*b", "a?b", "a<b", "a>b", "(a)", "{a}", "[a]", "a#b", "a!b", "~", "é", "a b",
		"..", "a..b", "/a/../b", "/a/..", "../a",
		"a/b", "relative/path", "./a", "/a//b", "/a/./b", "/a/", "//a", "/a b", "/a\nExecStart=/bin/sh", "/etc/passwd\nUser=root",
	}
	for _, v := range bad {
		if err := ValidateUnitValue(v); err == nil {
			t.Errorf("%q 应被拒绝", v)
		}
	}
	long := make([]byte, maxUnitValueLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if ValidateUnitValue(string(long)) == nil {
		t.Error("超长应被拒绝")
	}
}

func TestValidateUnitPath(t *testing.T) {
	for _, p := range []string{"/usr/bin/x", "/", "/a-b_c.d/e@f:g"} {
		if err := ValidateUnitPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "x", "a/b", "/a/..", "/a b", "/a\n", "/a%20b", "/a//b", "/a/", "/a/./b"} {
		if ValidateUnitPath(p) == nil {
			t.Errorf("%q 应被拒绝", p)
		}
	}
}

func FuzzValidateUnitValue(f *testing.F) {
	for _, s := range []string{"a", "/a/b", "a b", "%n", "/a/../b", "a\nb", "é", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidateUnitValue(s) != nil {
			return
		}
		// 通过校验的值：只含允许字符、无 `..`，含 `/` 时是规范绝对路径。
		for i := 0; i < len(s); i++ {
			if !unitByteOK(s[i]) {
				t.Fatalf("%q 含不允许字节却通过", s)
			}
		}
		if s == "" || len(s) > maxUnitValueLen {
			t.Fatalf("%q 不应通过", s)
		}
		for i := 0; i+1 < len(s); i++ {
			if s[i] == '.' && s[i+1] == '.' {
				t.Fatalf("%q 含 .. 却通过", s)
			}
		}
		if ValidateUnitPath(s) != nil && s[0] == '/' {
			t.Fatalf("%q 以 / 开头通过了 Value 却没通过 Path", s)
		}
	})
}
