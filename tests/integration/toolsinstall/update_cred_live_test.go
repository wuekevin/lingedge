package toolsinstall

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// AC9：迁移凭据按「环境变量 ＞ 口令文件 ＞ migrate.yaml ＞ 交互输入（仅 TTY，无回显）」取得；非交互且无凭据 → 退出码 3 且尚未改动任何东西；
// 口令不写入 journal/state.json/日志/输出/systemd 单元。四个来源各自真实地完成一次升级；优先级用「高优先级来源给错口令 → 失败」证明。
func TestUpdateMigrationCredentialSources(t *testing.T) {
	s := startStack(t)
	wrap := s.useSysd()
	wrongA, wrongB, wrongC := "wrong-"+rnd(6), "wrong-"+rnd(6), "wrong-"+rnd(6)
	mkOpt := func(env map[string]string, noPW bool) runOpt {
		e := s.dbEnv()
		for k, v := range env {
			e[k] = v
		}
		return runOpt{wrapRunnerFn: wrap, env: e, noPassword: noPW}
	}
	logFile := filepath.Join(t.TempDir(), "tools.log")
	b2 := s.serverBundle("0.2.0", serverBin(t), 11, nil)
	run := func(opt runOpt, bundle string, extra ...string) (int, string, string) {
		return s.runB(0, opt, append(s.updArgs(bundle, "30s", "--log-file", logFile), extra...)...)
	}
	passFile := func(pw string) string {
		p := filepath.Join(t.TempDir(), "migpass")
		if err := os.WriteFile(p, []byte(pw+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	writeYAML := func(pw string) {
		p := s.host("etc/lingedge/server/migrate.yaml")
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte("host: "+s.be.inst.Host()+"\nport: "+itoa(s.be.inst.Port)+"\ndatabase: "+s.be.dbName+"\nuser: lingedge_migrate\npassword: \""+pw+"\"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	untouched := func(what string) {
		t.Helper()
		if s.currentTarget() != "releases/0.1.0" || s.schemaVersion() != 18 || !s.serverRunning() || journalsLeft(s) != 0 || s.w.Has("opt/lingedge/server/releases/0.2.0") {
			t.Fatalf("%s：系统被改动（current=%s schema=%d）", what, s.currentTarget(), s.schemaVersion())
		}
	}
	// (0) 非交互且没有任何凭据来源 → 3，尚未改动任何东西（journal 都没开、没有备份）。
	before := tree(t, s.w)
	code, so, se := run(mkOpt(nil, true), b2)
	env := envelopeOf(t, so)
	if code != 3 || errCodeOf(env) != "precondition.db_credentials" {
		t.Fatalf("无凭据应 3: %d\n%s\n%s", code, so, se)
	}
	if d := diffTrees(before, tree(t, s.w)); d != "" || s.backupCount() != 0 {
		t.Fatalf("退出码 3 时尚未改动任何东西:\n%s", d)
	}
	untouched("无凭据")
	// (1) 环境变量优先于口令文件与 migrate.yaml：环境变量给错口令 → 失败（证明它被采用而不是后两者）。
	writeYAML(s.be.migPW)
	if code, so, _ := run(mkOpt(map[string]string{backup.EnvDBPass: wrongA, backup.EnvPassFile: ""}, false), b2); code == 0 {
		t.Fatalf("环境变量（错）应优先于口令文件/migrate.yaml（对）而导致失败\n%s", so)
	} else if strings.Contains(so, wrongA) {
		t.Fatalf("错误输出不得回显口令: %s", so)
	}
	untouched("环境变量优先")
	// (2) 口令文件优先于 migrate.yaml：文件给错 → 失败。
	if code, _, _ := run(mkOpt(map[string]string{backup.EnvPassFile: passFile(wrongB)}, true), b2); code == 0 {
		t.Fatal("口令文件（错）应优先于 migrate.yaml（对）")
	}
	untouched("口令文件优先")
	// (3) migrate.yaml 优先于交互输入：migrate.yaml 错、交互对 → 失败。
	writeYAML(wrongC)
	secretCalled := false
	interactiveArgs := withoutFlags(s.updArgs(b2, "30s", "--log-file", logFile), "--json", "--non-interactive")
	if code, _, _ := s.runB(0, runOpt{wrapRunnerFn: wrap, env: s.dbEnv(), noPassword: true, tty: true, secret: func(string) (string, error) { secretCalled = true; return s.be.migPW, nil }},
		interactiveArgs...); code == 0 {
		t.Fatal("migrate.yaml（错）应优先于交互输入（对）")
	}
	if secretCalled {
		t.Fatal("已有 migrate.yaml 时不应提示交互输入")
	}
	untouched("migrate.yaml 优先")
	_ = os.Remove(s.host("etc/lingedge/server/migrate.yaml"))

	// 四个来源各自真实地完成一次升级（从低到高：交互 → migrate.yaml → 口令文件 → 环境变量）。
	ver := []string{"0.2.0", "0.3.0", "0.4.0", "0.5.0"}
	seq := int64(20)
	step := func(i int, opt runOpt, args []string) {
		t.Helper()
		b := s.serverBundle(ver[i], serverBin(t), seq+int64(i), nil)
		a := append([]string{}, args...)
		a = append([]string{"update", "server", "--offline", b, "--config", s.toolsCfg("60s"), "--log-file", logFile}, a...)
		a = append(a, s.dbFlags()...)
		if code, so, se := s.runB(0, opt, a...); code != 0 {
			t.Fatalf("来源 %d 升级失败: %d\n%s\n%s\n%s", i, code, so, se, s.spec.logs.String())
		}
		if got := s.currentTarget(); got != "releases/"+ver[i] {
			t.Fatalf("来源 %d：current=%s", i, got)
		}
		s.waitReady()
	}
	var mu sync.Mutex
	prompts := 0
	step(0, runOpt{wrapRunnerFn: wrap, env: s.dbEnv(), noPassword: true, tty: true, secret: func(label string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts++
		if strings.Contains(label, s.be.migPW) {
			t.Error("提示语不得含口令")
		}
		return s.be.migPW, nil
	}}, nil) // 交互（TTY，无回显）：一次升级只提示一次
	if prompts != 1 {
		t.Errorf("同一次升级只应提示一次口令: %d", prompts)
	}
	writeYAML(s.be.migPW)
	step(1, runOpt{wrapRunnerFn: wrap, env: s.dbEnv(), noPassword: true}, []string{"--json", "--non-interactive"}) // migrate.yaml
	_ = os.Remove(s.host("etc/lingedge/server/migrate.yaml"))
	step(2, mkOpt(map[string]string{backup.EnvPassFile: passFile(s.be.migPW)}, true), []string{"--json", "--non-interactive"}) // 口令文件
	step(3, mkOpt(nil, false), []string{"--json", "--non-interactive"})                                                        // 环境变量

	// 口令不入 journal/state.json/日志/输出/systemd 单元/暂存：全树扫描（含已归档的 journal 与 Tools 日志）。
	secrets := append(s.be.secrets(), wrongA, wrongB, wrongC)
	_ = filepath.WalkDir(s.w.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(s.w.Root, p)
		if strings.HasPrefix(rel, "opt/") || strings.HasPrefix(rel, "bin/") || strings.HasPrefix(rel, "run/") { // 包内二进制/伪二进制/运行期文件不扫
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, sec := range secrets {
			if strings.Contains(string(b), sec) {
				// 运行期凭据密文/备份里的数据库内容不含明文口令；向导写入 server.yaml 的也不含。出现即泄漏。
				t.Errorf("口令明文出现在 %s", rel)
			}
		}
		return nil
	})
	if lb, err := os.ReadFile(logFile); err != nil || len(lb) == 0 {
		t.Errorf("Tools 日志应存在: %v", err)
	}
	for _, f := range []string{"state.json"} {
		b, _ := os.ReadFile(filepath.Join(s.w.Layout.Root, f))
		for _, sec := range secrets {
			if strings.Contains(string(b), sec) {
				t.Errorf("%s 含口令", f)
			}
		}
	}
	s.assertNoCredentialResidue()
	_ = installer.TotalSteps
}

func withoutFlags(args []string, drop ...string) []string {
	var out []string
	for _, a := range args {
		skip := false
		for _, d := range drop {
			skip = skip || a == d
		}
		if !skip {
			out = append(out, a)
		}
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
