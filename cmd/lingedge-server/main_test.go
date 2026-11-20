package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCLI(args ...string) (string, string, int) {
	var so, se bytes.Buffer
	code := run(args, &so, &se, []string{"PATH=" + "/usr/bin"})
	return so.String(), se.String(), code
}

// AC2：setup 在 1.24 前不注册（不假装可用）；未知子命令同样 2。
func TestSetupIsNotRegisteredBeforeStory124(t *testing.T) {
	for _, sub := range []string{"setup", "bogus"} {
		_, se, code := runCLI(sub)
		if code != 2 || !strings.Contains(se, "未知子命令") {
			t.Errorf("%s: code=%d stderr=%q", sub, code, se)
		}
	}
	if _, se, _ := runCLI("help"); strings.Contains(se, "setup") {
		t.Error("用法文本不应出现 setup")
	}
	so, _, code := runCLI("help")
	if code != 0 || strings.Contains(so, "setup") {
		t.Errorf("help 输出不得提及 setup: %q", so)
	}
}

func TestVersionAndUsage(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		so, _, code := runCLI(args...)
		if code != 0 || !strings.Contains(so, "lingedge-server") {
			t.Errorf("%v: %d %q", args, code, so)
		}
	}
	if _, _, code := runCLI(); code != 2 {
		t.Errorf("无参数应 2: %d", code)
	}
}

// 非法角色：在连接任何依赖之前就以退出码 2 报错。
func TestInvalidRolesExitNonZeroBeforeAnyIO(t *testing.T) {
	_, se, code := runCLI("--config", "/nonexistent.yaml", "--roles=api,nope")
	if code != 2 || !strings.Contains(se, "未知角色") {
		t.Fatalf("code=%d stderr=%q", code, se)
	}
}

func TestLineageUsageErrors(t *testing.T) {
	if _, _, code := runCLI("lineage"); code != 2 {
		t.Errorf("lineage 缺子命令应 2: %d", code)
	}
	if _, _, code := runCLI("lineage", "nuke"); code != 2 {
		t.Errorf("未知 lineage 子命令应 2: %d", code)
	}
	if _, se, code := runCLI("lineage", "bump", "--config", "/nonexistent.yaml"); code != 1 || !strings.Contains(se, "配置") {
		t.Errorf("配置不存在应 1: %d %q", code, se)
	}
}
