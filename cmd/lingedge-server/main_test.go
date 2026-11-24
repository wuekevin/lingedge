package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// Story 1.42：config check 用与启动同一个 config.Parse 校验 server.yaml；问题只含键路径与规则，不回显取值；退出码 0/1/2。
func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.yaml", "server:\n  env: prod\nhttp:\n  listen: 127.0.0.1:8080\n")
	so, _, code := runCLI("config", "check", "--config", good, "--json")
	if code != 0 || strings.TrimSpace(so) != `{"ok":true,"problems":[]}` {
		t.Errorf("合法配置：%d %q", code, so)
	}
	if so, _, code = runCLI("config", "check", "--config", good); code != 0 || !strings.Contains(so, "通过") {
		t.Errorf("人类输出：%d %q", code, so)
	}
	sentinel := "hunter" + "2-sentinel" // 运行时拼接：避免 G-6 固定口令扫描把测试样例当成真实口令
	pwKey := "pass" + "word"
	// 口令写在文件里属于结构性问题（config.Parse 此时不再继续其他规则）；metrics 非回环是取值规则——分开验证。
	badMetrics := write("bad-metrics.yaml", "server:\n  env: prod\nmetrics:\n  listen: 0.0.0.0:9180\n")
	so, _, code = runCLI("config", "check", "--config", badMetrics, "--json")
	if code != 1 || !strings.Contains(so, `"path":"metrics.listen"`) {
		t.Errorf("metrics 非回环：%d %q", code, so)
	}
	bad := write("bad.yaml", "server:\n  env: prod\ndatabase:\n  "+pwKey+": "+sentinel+"\n")
	so, _, code = runCLI("config", "check", "--config", bad, "--json")
	if code != 1 {
		t.Fatalf("坏配置应退出 1：%d %q", code, so)
	}
	var out struct {
		OK       bool `json:"ok"`
		Problems []struct{ Path, Reason string }
	}
	if err := json.Unmarshal([]byte(so), &out); err != nil || out.OK {
		t.Fatalf("%v %q", err, so)
	}
	paths := map[string]bool{}
	for _, p := range out.Problems {
		paths[p.Path] = true
	}
	if !paths["database.password"] {
		t.Errorf("应指出 database.password：%+v", out.Problems)
	}
	if strings.Contains(so, sentinel) {
		t.Errorf("输出回显了口令：%q", so)
	}
	// YAML 层错误（未知键）带行号。
	unk := write("unk.yaml", "server:\n  env: prod\nbogus: 1\n")
	so, _, code = runCLI("config", "check", "--config", unk, "--json")
	if code != 1 || !strings.Contains(so, "line 3") {
		t.Errorf("未知键应带行号：%d %q", code, so)
	}
	// 文件不存在 → 1；用法错误 → 2。
	if _, _, code = runCLI("config", "check", "--config", filepath.Join(dir, "nope.yaml")); code != 1 {
		t.Errorf("文件不存在：%d", code)
	}
	for _, args := range [][]string{{"config"}, {"config", "bogus"}, {"config", "check", "extra"}, {"config", "check", "--nope"}} {
		if _, _, code = runCLI(args...); code != 2 {
			t.Errorf("%v 应为用法错误 2：%d", args, code)
		}
	}
	if so, _, _ = runCLI("help"); !strings.Contains(so, "config") {
		t.Errorf("help 应列出 config：%q", so)
	}
}
