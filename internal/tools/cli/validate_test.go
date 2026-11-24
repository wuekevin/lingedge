package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC6：参数校验违规 → 退出码 2（组件名枚举、标志枚举/格式、互斥、缺参数）。
func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{"install", "Server"},          // 区分大小写
		{"install", "server", "extra"}, // 多余参数
		{"install"},                    // 非交互缺组件
		{"install", "server", "--channel", "nightly"},
		{"install", "server", "--version", "1.2"},
		{"install", "server", "--version", "v1.2.3"},
		{"install", "server", "--set", "novalue"},
		{"install", "server", "--set", "Bad Key=1"},
		{"install", "node", "--mode", "static"}, // --mode 仅 website
		{"install", "website", "--mode", "cgi"},
		{"install", "node", "--setup-listen", "127.0.0.1:8080"}, // 仅 server
		{"install", "server", "--setup-listen", "no-port"},
		{"install", "server", "--setup-listen", "127.0.0.1:99999"},
		{"update"},
		{"update", "node", "--all"},
		{"update", "server", "--no-backup"},
		{"update", "node", "--apply-staged", "--all"},
		{"update", "server", "--apply-staged"},
		{"update", "check", "--channel", "x"},
		{"service", "start", "nope"},
		{"service", "status", "node", "--all"},
		{"logs", "node", "--since", "abc"},
		{"logs", "node", "-n", "0"},
		{"doctor", "node", "--all"},
		{"config", "validate", "nope"},
		{"node", "enroll", "--token", "a", "--token-file", "b"},
		{"node", "enroll", "--server", "ftp://x"},
		{"node", "enroll", "--server", "https://user" + ":" + "x@host"},
		{"backup", "create", "--component", "dns"},
		{"rollback", "node", "--to", "latest"},
		{"recover", "--resume", "--rollback"},
		{"uninstall", "nope"},
		{"list", "extra"},
		{"version", "--bogus"},
	}
	for _, args := range cases {
		h := newHarness(t, asRoot()) // root：确保失败发生在校验阶段而非 root 检查之后
		if code := h.run(append(args, "--non-interactive")...); code != 2 {
			t.Errorf("%v 退出码 %d，期望 2\n%s", args, code, h.stderr)
		}
	}
	// 合法组合不被误伤（走到「尚未实现」= 1，而不是 2）。Story 1.33 起 install 已实现：其合法组合通过校验后进入清单验证，本构建未内置发行公钥 → 5（见下）。
	for _, args := range [][]string{
		{"update", "node", "--apply-staged"},
		{"node", "enroll", "--server", "https://panel.example.com:8443"},
	} {
		h := newHarness(t, asRoot())
		if code := h.run(append(args, "--non-interactive", "--yes")...); code != 1 {
			t.Errorf("%v 应通过校验落到未实现(1)，得 %d\n%s", args, code, h.stderr)
		}
	}
	// Story 1.43 起 update 已实现：合法组合通过校验后落到命令本身——目标未安装 → 14；--all 在没有任何已装组件时无事可做 → 0。
	for _, c := range []struct {
		args []string
		want int
	}{{[]string{"update", "node", "--no-backup", "--yes"}, 14}, {[]string{"update", "--all", "--plan-only"}, 0}} {
		h := newHarness(t, asRoot())
		if code := h.run(append(c.args, "--non-interactive", "--yes")...); code != c.want {
			t.Errorf("%v 应通过校验落到命令本身（%d），得 %d\n%s", c.args, c.want, code, h.stderr)
		}
	}
}

// logs 的合法标志组合通过校验后落到命令本身：node 未安装 → 14（Story 1.42；此前它落到「尚未实现」1）。
func TestLogsValidCombosPassValidation(t *testing.T) {
	h := newHarness(t, asRoot())
	if code := h.run("logs", "node", "--since", "10m", "-n", "5", "--non-interactive"); code != int(ExitNotInstalled) {
		t.Errorf("应通过校验落到命令本身（14），得 %d\n%s", code, h.stderr)
	}
}

// install 的合法标志组合不被用法校验误伤：通过校验后进入清单验证，本构建未内置发行公钥 → 退出码 5（而不是 2）。
func TestInstallValidCombosPassValidation(t *testing.T) {
	for _, args := range [][]string{
		{"install", "server", "--channel", "beta", "--version", "1.2.3-rc.1+b5", "--set", "env=staging", "--setup-listen", ":8080"},
		{"install", "website", "--mode", "ssr", "--set", "host=www.example.com"}, // Story 1.40：website 是静态前端规格，host 为必填项
		{"install", "node", "--set", "port.node.tcp.9100=19100@127.0.0.1"},
	} {
		h := newHarness(t, asRoot())
		if code := h.run(append(args, "--non-interactive", "--yes")...); code != 5 {
			t.Errorf("%v 应通过校验进入清单验证（5），得 %d\n%s", args, code, h.stderr)
		}
	}
}

// --plan-only 等价 --dry-run（envelope.dry_run=true）。
func TestPlanOnlyIsDryRun(t *testing.T) {
	h := newHarness(t, asRoot())
	h.run("update", "--all", "--plan-only", "--json")
	if h.envelope()["dry_run"] != true {
		t.Errorf("--plan-only 应使 dry_run=true: %s", h.stdout)
	}
	h = newHarness(t, asRoot())
	h.run("install", "server", "--dry-run", "--json")
	if h.envelope()["dry_run"] != true {
		t.Errorf("--dry-run 应使 dry_run=true: %s", h.stdout)
	}
}

// 参数注入向量：shell 元字符只会被当作字面值——不执行、不创建文件、被校验拒绝时原样（经脱敏）回显而不被解释。
func TestShellMetacharactersAreLiteral(t *testing.T) {
	h := newHarness(t, asRoot())
	sentinel := filepath.Join(h.dir, "PWNED")
	logf := filepath.Join(h.dir, "t.log")
	vectors := []string{
		"server; touch " + sentinel,
		"server && touch " + sentinel,
		"server | touch " + sentinel,
		"$(touch " + sentinel + ")",
		"`touch " + sentinel + "`",
		"server\ntouch " + sentinel,
		"server' ; touch " + sentinel + " ; '",
		"${IFS}touch${IFS}" + sentinel,
		"../../etc/passwd",
		"-rf",
	}
	for _, v := range vectors {
		// 作为组件名：违规 → 2，且没有任何副作用。
		if code := h.run("install", v, "--json", "--log-file", logf); code != 2 && code != 8 {
			t.Errorf("组件名 %q 退出码 %d，期望 2", v, code)
		}
		env := h.envelope() // 输出仍是合法的单个 JSON（元字符被正确转义）
		if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
			t.Errorf("%q: %v", v, err)
		}
		// 作为 --set 的值：server 的规格（Story 1.35）不认识键 k → 在任何副作用之前被 schema 拒绝（2）；含控制字符的值被 ParseSet 拒绝（2）。
		// 值只当作数据，绝不被执行（哨兵文件断言见下）。
		if code := h.run("install", "server", "--set", "k="+v, "--json", "--log-file", logf); code != 2 {
			t.Errorf("--set 值 %q 退出码 %d，期望 2", v, code)
		}
		// 合法键 env 的非法值：同样是 schema 拒绝（2），值不会被执行；合法值走到清单验证（本构建无内置公钥 → 5）见循环外。
		// 作为 --version：不是 semver → 2。
		if code := h.run("install", "server", "--version", v, "--json", "--log-file", logf); code != 2 {
			t.Errorf("--version %q 退出码 %d，期望 2", v, code)
		}
	}
	if code := h.run("install", "server", "--set", "env=prod", "--json", "--log-file", logf); code != 5 {
		t.Errorf("合法的 --set 应通过 schema 并走到清单验证（5），实际 %d", code)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("元字符被当作命令执行了：哨兵文件被创建")
	}
	logb, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	// 日志每行都是合法 JSON，换行/引号被转义，没有被拆成伪造的日志行。
	for i, line := range strings.Split(strings.TrimRight(string(logb), "\n"), "\n") {
		decodeAny(t, []byte(line))
		_ = i
	}
}
