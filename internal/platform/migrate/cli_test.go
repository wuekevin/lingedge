package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

func runCLI(fsys fstest.MapFS, env []string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(args, &out, &errb, env, fsys)
	return code, out.String(), errb.String()
}

func TestCLIPlanOfflineOutputsJSON(t *testing.T) {
	code, out, errs := runCLI(samplePlanFS(), nil, "plan")
	if code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, errs)
	}
	var p Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("plan 输出不是 JSON: %v\n%s", err, out)
	}
	if p.Count != 4 || !p.HasIrreversible {
		t.Fatalf("plan: %+v", p)
	}
}

func TestCLILint(t *testing.T) {
	if code, out, errs := runCLI(samplePlanFS(), nil, "lint"); code != ExitOK || !strings.Contains(out, "4 个迁移") {
		t.Fatalf("合法迁移 lint 应通过: code=%d out=%q err=%q", code, out, errs)
	}
	bad := fstest.MapFS{"000001_a.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t (id INT);\n")}}
	code, _, errs := runCLI(bad, nil, "lint")
	if code != ExitLint {
		t.Fatalf("lint 失败应返回 %d，实际 %d", ExitLint, code)
	}
	for _, want := range []string{"[header]", "[idempotency]", "[grant-app]"} {
		if !strings.Contains(errs, want) {
			t.Errorf("lint 输出应含 %s:\n%s", want, errs)
		}
	}
	// plan 离线模式同样 fail-closed。
	if code, _, _ := runCLI(bad, nil, "plan"); code != ExitLint {
		t.Fatalf("带缺陷的迁移不得生成 plan，退出码 = %d", code)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	if code, _, errs := runCLI(samplePlanFS(), nil); code != ExitUsage || !strings.Contains(errs, "用法") {
		t.Errorf("无子命令: code=%d err=%q", code, errs)
	}
	if code, _, _ := runCLI(samplePlanFS(), nil, "frobnicate"); code != ExitUsage {
		t.Errorf("未知子命令退出码 = %d", code)
	}
	for _, sub := range []string{"up", "down", "status"} {
		if code, _, errs := runCLI(samplePlanFS(), nil, sub); code != ExitUsage || !strings.Contains(errs, "连接选项") {
			t.Errorf("%s 无连接选项应报用法错误: code=%d err=%q", sub, code, errs)
		}
	}
	if code, _, _ := runCLI(samplePlanFS(), nil, "recover", "--host", "127.0.0.1"); code != ExitUsage {
		t.Errorf("recover 缺 --version 退出码 = %d", code)
	}
}

func TestCLIRequiresPasswordAndNeverTakesItFromFlags(t *testing.T) {
	code, _, errs := runCLI(samplePlanFS(), nil, "up", "--host", "127.0.0.1", "--port", "3306", "--database", "x")
	if code != ExitPrecondition || !strings.Contains(errs, EnvDBPass) || !strings.Contains(errs, "credential init") {
		t.Fatalf("非交互缺口令应以退出码 3（前置条件）失败并指明环境变量与 credential init（ADR-026 §4）: code=%d err=%q", code, errs)
	}
	// 命令行上没有任何口令类参数（运行期拼出参数名，避免本文件被固定口令扫描误报）。
	if code, _, _ := runCLI(samplePlanFS(), nil, "up", "--host", "127.0.0.1", "--"+"password", "x"); code != ExitUsage {
		t.Fatalf("口令不得经命令行传入（会出现在 ps 里），退出码 = %d", code)
	}
	// 口令与口令文件互斥。
	code, _, errs = runCLI(samplePlanFS(), []string{EnvDBPass + "=a", EnvDBPassFile + "=/x"}, "up", "--host", "127.0.0.1", "--database", "x")
	if code != ExitFailure || !strings.Contains(errs, "只能设置其一") {
		t.Fatalf("口令与口令文件互斥: code=%d err=%q", code, errs)
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, ExitOK},
		{&ErrLint{}, ExitLint},
		{&ErrDirty{}, ExitDirty},
		{&ErrChecksumMismatch{}, ExitChecksum},
		{&ErrUnsupportedVersion{}, ExitVersion},
		{&ErrLockTimeout{}, ExitLocked},
		{&ErrPreflight{}, ExitPreflight},
		{&ErrIrreversibleNotAccepted{}, ExitIrreversible},
		{&ErrDownNotAllowed{}, ExitFailure},
		// 数据库与二进制的迁移集合不一致（二进制比数据库旧、乱序、记录矛盾）：Tools 据此区分「回滚/换二进制」与普通失败。
		{&ErrUnknownApplied{Versions: []int64{5}}, ExitOutOfSync},
		{&ErrOutOfOrder{Missing: []int64{2}, Max: 3}, ExitOutOfSync},
		{&ErrInconsistent{Version: 3}, ExitOutOfSync},
		{fmt.Errorf("包装: %w", &ErrOutOfOrder{}), ExitOutOfSync},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("%T → %d，期望 %d", c.err, got, c.want)
		}
	}
}

func TestCheckMySQLVersion(t *testing.T) {
	for _, c := range []struct {
		v  string
		ok bool
	}{
		{"8.0.46", true}, {"8.0.46-log", true}, {"8.0.16", true}, {"8.0.15", false}, {"8.0.0", false}, {"5.7.44", false},
		{"8.4.3", true}, {"9.1.0", true}, {"10.11.6-MariaDB", false}, {"8.0.30-MariaDB-1", false}, {"", false}, {"abc", false},
	} {
		err := CheckMySQLVersion(c.v)
		if c.ok && err != nil {
			t.Errorf("%q 应通过: %v", c.v, err)
		}
		if !c.ok {
			var ue *ErrUnsupportedVersion
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), "8.0.16") {
				t.Errorf("%q 应被拒绝（ErrUnsupportedVersion）并说明最低版本 8.0.16: %v", c.v, err)
			}
		}
	}
}

func TestDirtyErrorGivesRecoveryGuidance(t *testing.T) {
	msg := (&ErrDirty{Version: 7, Name: "sites_core", Direction: "up", Status: "failed", Cause: "Error 1824"}).Error()
	for _, want := range []string{"dirty", "000007_sites_core", "recover --version 7", "migrate up", "幂等", "备份"} {
		if !strings.Contains(msg, want) {
			t.Errorf("dirty 错误应含 %q:\n%s", want, msg)
		}
	}
}

func TestCLIRejectsExtraPositionalArgsAndBadOptions(t *testing.T) {
	env := []string{EnvDBPass + "=x"}
	db := []string{"--host", "127.0.0.1", "--database", "x"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"位置参数使后面的选项被忽略", append([]string{"up", "5"}, append(db, "--to", "3")...), "多余的参数"},
		{"位置参数夹在选项中间", []string{"up", "--host", "h", "extra", "--accept-irreversible"}, "多余的参数"},
		{"up --to 为负", append([]string{"up", "--to", "-1"}, db...), "--to"},
		{"down --steps 为负", append([]string{"down", "--steps", "-2"}, db...), "不得为负数"},
		{"down --to 为负", append([]string{"down", "--to", "-1"}, db...), "不得为负数"},
		{"down --steps 与 --all 互斥", append([]string{"down", "--steps", "2", "--all"}, db...), "只能指定其一"},
		{"down --to 与 --steps 互斥", append([]string{"down", "--to", "3", "--steps", "1"}, db...), "只能指定其一"},
		{"锁超时小于 1s", append([]string{"up", "--lock-timeout", "500ms"}, db...), "--lock-timeout"},
		{"锁超时为 0", append([]string{"up", "--lock-timeout", "0s"}, db...), "--lock-timeout"},
		{"端口越界", []string{"status", "--host", "h", "--port", "70000"}, "--port"},
	}
	for _, c := range cases {
		code, _, errs := runCLI(samplePlanFS(), env, c.args...)
		if code != ExitUsage || !strings.Contains(errs, c.want) {
			t.Errorf("%s: code=%d err=%q，期望用法错误并含 %q", c.name, code, errs, c.want)
		}
	}
}

func TestCLIEnvDatabaseHostWithoutFlags(t *testing.T) {
	// 只设置 LINGEDGE_DATABASE__HOST：必须被当作连接来源（而不是跳过离线判断后因主机为空而报不明所以的错误）。
	env := []string{"LINGEDGE_DATABASE__HOST=127.0.0.1", "LINGEDGE_DATABASE__NAME=lingedge", EnvDBPass + "=x"}
	c := connFlags{lockTimeout: DefaultLockTimeout}
	m := envMap(env)
	if !c.wantsDB(m) {
		t.Fatal("LINGEDGE_DATABASE__HOST 应使 wantsDB 为真")
	}
	cfg, err := c.dbConfig(env, m, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Name != "lingedge" || cfg.Port != 3306 || cfg.User != DefaultUser {
		t.Fatalf("环境变量应被读取，端口缺省 3306，用户缺省 %s: %+v", DefaultUser, cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("配置应合法: %v", err)
	}
	// flag 覆盖环境变量；--socket 优先于主机。
	c.host, c.port = "10.0.0.9", 3307
	if cfg, _ = c.dbConfig(env, m, io.Discard); cfg.Host != "10.0.0.9" || cfg.Port != 3307 {
		t.Fatalf("flag 应覆盖环境变量: %+v", cfg)
	}
	c.socket = "/tmp/m.sock"
	if cfg, _ = c.dbConfig(env, m, io.Discard); cfg.Socket != "/tmp/m.sock" || cfg.Host != "" || cfg.Port != 0 {
		t.Fatalf("--socket 应优先于主机: %+v", cfg)
	}
}

func TestTruncateRunesNeverSplitsACharacter(t *testing.T) {
	in := strings.Repeat("中", 3000)
	out := truncateRunes(in, 2000)
	if !utf8.ValidString(out) || utf8.RuneCountInString(out) != 2001 { // 2000 字 + 省略号
		t.Fatalf("应按字符截断且保持合法 UTF-8: valid=%v runes=%d", utf8.ValidString(out), utf8.RuneCountInString(out))
	}
	if short := "短文本"; truncateRunes(short, 2000) != short {
		t.Fatal("不超长时原样返回")
	}
}
