package version

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestCLIVersionFlag(t *testing.T) {
	for _, flag := range []string{"--version", "-version"} {
		var out, errb bytes.Buffer
		code := CLI("lingedge", []string{flag}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s: 退出码 = %d，期望 0", flag, code)
		}
		got := out.String()
		for _, want := range []string{"lingedge ", Version, Commit, runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: 输出 %q 缺少 %q", flag, got, want)
			}
		}
		if !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
			t.Errorf("%s: 输出应为单行: %q", flag, got)
		}
		if errb.Len() != 0 {
			t.Errorf("%s: stderr 应为空: %q", flag, errb.String())
		}
	}
}

func TestCLIRejectsOtherArgs(t *testing.T) {
	cases := [][]string{nil, {}, {"serve"}, {"--version", "extra"}, {"--help"}}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := CLI("lingedge-node", args, &out, &errb); code != 2 {
			t.Errorf("args=%v: 退出码 = %d，期望 2", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("args=%v: stdout 应为空: %q", args, out.String())
		}
		if !strings.Contains(errb.String(), "lingedge-node") {
			t.Errorf("args=%v: stderr 应含程序名: %q", args, errb.String())
		}
	}
}

func TestInfoReflectsInjectedValues(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
	Version, Commit = "1.2.3", "abc123"
	got := Info("x")
	if !strings.Contains(got, "x 1.2.3 (commit abc123,") {
		t.Errorf("Info = %q", got)
	}
}
