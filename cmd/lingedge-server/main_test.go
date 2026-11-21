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

// Story 1.24：setup 子命令已注册（无头安装，--from-file）；未知子命令仍然 2；help 提及 setup 与 db。
func TestSetupSubcommandRegisteredAndUnknownStillRejected(t *testing.T) {
	_, se, code := runCLI("bogus")
	if code != 2 || !strings.Contains(se, "未知子命令") {
		t.Errorf("bogus: code=%d stderr=%q", code, se)
	}
	// setup 缺 --from-file：用法错误 2，且不是「未知子命令」。
	_, se, code = runCLI("setup")
	if code != 2 || strings.Contains(se, "未知子命令") || !strings.Contains(se, "--from-file") {
		t.Errorf("setup 无参数应为用法错误: code=%d stderr=%q", code, se)
	}
	so, _, code := runCLI("help")
	if code != 0 || !strings.Contains(so, "setup") {
		t.Errorf("help 应列出 setup: %q", so)
	}
	if _, _, code := runCLI("db"); code != 2 {
		t.Errorf("db 缺子命令应 2: %d", code)
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
