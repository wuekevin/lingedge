package installer_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
)

// AC1：22 步按序执行，每步先 begin（含 undo）后 done，事务以 commit 结束并归档；staging 被清理；信任状态（sequence）在安装成功后才推进。
func TestInstallNodeHappyPath(t *testing.T) {
	w := newW(t)
	var c collector
	mut, stop := withEvents(&c)
	res := must(t, doInstall(t, w, "node", []func(*installer.Deps){mut}))
	stop()
	if res.Outcome != installer.OutcomeInstalled || len(res.Components) != 2 {
		t.Fatalf("结果: %+v", res)
	}
	// journal：open → 1..22 每步 begin/done 严格交替 → commit，已归档。
	if len(activeJournals(t, w)) != 0 || len(archivedJournals(t, w)) != 1 {
		t.Fatalf("journal 应已归档: active=%v archived=%v", activeJournals(t, w), archivedJournals(t, w))
	}
	want := []string{"open"}
	for n := 1; n <= 22; n++ {
		want = append(want, fmt.Sprintf("begin:%d", n), fmt.Sprintf("done:%d", n))
	}
	want = append(want, "commit")
	if got := journalOps(t, archivedJournals(t, w)[0]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("journal 序列不符\n got: %v\nwant: %v", got, want)
	}
	// state.json：两个组件，LastTx 是本事务，端口/用户/目录齐全。
	st, _, err := w.Registry("x").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	n := st.Components["node"]
	if n.Version != "0.1.0" || n.State != "running" || n.Unit != "lingedge-node.service" || n.RunUser != "lingedge-node" || n.LastTx != res.Tx ||
		n.ReleaseDir != "/opt/lingedge/node/releases/0.1.0" || n.ConfigDir != "/etc/lingedge/node" || n.DataDir != "/var/lib/lingedge-node" || len(n.Ports) != 1 || n.Ports[0] != 9100 {
		t.Fatalf("node 记录: %+v", n)
	}
	if o := st.Components["openresty"]; o.State != "running" || o.LastTx != res.Tx {
		t.Fatalf("openresty 记录: %+v", o)
	}
	if st.Host == nil || st.Host.Baota == nil {
		t.Error("host.baota 应随提交点一起写入")
	}
	// 文件系统：release 只读、current 链接、配置 0640、单元 0644、用户已创建、服务已启用并启动。
	for _, f := range []struct {
		rel  string
		mode os.FileMode
	}{
		{"opt/lingedge/node/releases/0.1.0/bin/lingedge-node", 0o755}, {"etc/lingedge/node/node.yaml", 0o640},
		{"etc/systemd/system/lingedge-node.service", 0o644}, {"etc/systemd/system/lingedge-openresty.path", 0o644},
	} {
		fi, err := os.Lstat(filepath.Join(w.Root, f.rel))
		if err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("%s: %v %v，期望 %v", f.rel, fi, err, f.mode)
		}
	}
	if tg, _ := os.Readlink(filepath.Join(w.Root, "opt/lingedge/node/current")); tg != "releases/0.1.0" {
		t.Errorf("current -> %q", tg)
	}
	for _, rel := range []string{"users/lingedge-node", "enabled-lingedge-node.service", "active-lingedge-node.service", "enabled-lingedge-openresty.path", "enabled-lingedge-node-upgrade.path"} {
		if !w.FakeHas(rel) {
			t.Errorf("伪 systemd 状态缺少 %s", rel)
		}
	}
	// staging 已清理，sequence 已推进。
	if ents, _ := os.ReadDir(w.Layout.StagingDir()); len(ents) != 0 {
		t.Errorf("staging 应已清理: %v", ents)
	}
	if seq, ok, err := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); err != nil || !ok || seq != 10 {
		t.Errorf("sequence=%d ok=%v err=%v，期望 10", seq, ok, err)
	}
	// 事件：10..22 每步 step_start 按序出现；预检 1..9 各有结论。
	last := 0
	for _, e := range c.evs {
		if e.Type == installer.EventStepStart {
			if e.Step <= last {
				t.Fatalf("步骤事件乱序: %d 之后出现 %d", last, e.Step)
			}
			last = e.Step
		}
	}
	if last != 22 {
		t.Errorf("最后一个 step_start=%d，期望 22", last)
	}
}

// server：server.yaml/master.key/setup-token.hash 由 installer 内建生成；token 只在结果里出现一次，绝不落盘、不进 journal/日志/事件。
func TestInstallServerSecretsNeverLeak(t *testing.T) {
	w := newW(t)
	var c collector
	mut, stop := withEvents(&c)
	res := must(t, doInstall(t, w, "server", []func(*installer.Deps){mut}, func(r *installer.Request) { r.SetupListen = "127.0.0.1:18080" }))
	stop()
	var token string
	for _, a := range res.Access {
		if a.Kind == "setup_token" {
			token = a.Value
			if !a.Secret {
				t.Error("setup_token 必须标记 secret")
			}
		}
	}
	if len(token) != 43 {
		t.Fatalf("应交付一个 43 字符的 setup token: %+v", res.Access)
	}
	hash, err := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/setup-token.hash"))
	if err != nil || !setuptoken.Verify(token, string(hash)) {
		t.Fatalf("哈希文件与 token 不匹配: %v", err)
	}
	for _, f := range []struct {
		rel  string
		mode os.FileMode
	}{{"etc/lingedge/server/master.key", 0o400}, {"etc/lingedge/server/server.yaml", 0o640}, {"etc/lingedge/server/setup-token.hash", 0o600}} {
		fi, err := os.Stat(filepath.Join(w.Root, f.rel))
		if err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("%s: %v %v，期望 %v", f.rel, fi, err, f.mode)
		}
	}
	// F-7：主密钥只给短指纹（不是密钥），并提示与数据库备份分开保管。
	fp, warn := "", ""
	for _, a := range res.Access {
		switch a.Kind {
		case "master_key_fingerprint":
			fp = a.Value
		case "master_key_warning":
			warn = a.Label + a.Value
		}
	}
	if len(strings.Fields(fp)) == 0 || len(strings.Fields(fp)[0]) < 16 || !strings.Contains(warn, "分开保管") {
		t.Errorf("应给出主密钥短指纹（%q）与分开保管的醒目提示（%q）", fp, warn)
	}
	yml, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(yml), "listen: 127.0.0.1:18080") || !strings.Contains(string(yml), "master_key_file: ''") {
		t.Errorf("server.yaml:\n%s", yml)
	}
	key, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/master.key"))
	keyLine := strings.Split(strings.TrimSpace(string(key)), "\n")[2][3:] // k1:<b64>
	// token、主密钥内容不得出现在任何文件（journal、状态、配置除自身外）与事件里。
	err = filepath.Walk(w.Root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || strings.HasSuffix(p, "etc/lingedge/server/master.key") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), token) {
			t.Errorf("setup token 出现在 %s", p)
		}
		if strings.Contains(string(b), keyLine) {
			t.Errorf("主密钥内容出现在 %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.evs {
		if s := fmt.Sprintf("%+v", e); strings.Contains(s, token) || strings.Contains(s, keyLine) {
			t.Errorf("事件泄漏秘密: %s", s)
		}
	}
}

// 已装且版本相同 → 无需安装（keep），不写任何东西。
func TestInstallAlreadyInstalledIsNoop(t *testing.T) {
	w := newW(t)
	must(t, doInstall(t, w, "node", nil))
	before := fullSnap(t, w)
	res := must(t, doInstall(t, w, "node", nil))
	if res.Outcome != installer.OutcomeNoop {
		t.Fatalf("outcome=%s", res.Outcome)
	}
	// 只允许 Tools 簿记里的锁文件 mtime 之外的零变化：内容快照相同。
	if d := diffSnap(before, fullSnap(t, w)); d != "" {
		t.Fatalf("noop 不应改动任何东西:\n%s", d)
	}
}

// ADR-027：systemd < 247（无 LoadCredential=）→ 降级：单元不含 LoadCredential、master.key 为 root:<运行组> 0440、server.yaml 的 secrets.master_key_file 填路径、给出降级警告；
// systemd < 235 → 明确失败（退出码 3 的 service 错误），不安装任何东西。
func TestServerInstallDegradesWithoutLoadCredential(t *testing.T) {
	w := newW(t)
	if err := w.SetSystemdVersion("systemd 245 (245.4-4ubuntu3)"); err != nil {
		t.Fatal(err)
	}
	res := must(t, doInstall(t, w, "server", nil))
	unit, _ := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-server.service"))
	for _, ln := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(ln, "LoadCredential=") { // 注释里提到该指令名不算
			t.Errorf("降级变体不应含 LoadCredential= 指令: %q", ln)
		}
	}
	yml, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(yml), "master_key_file: /etc/lingedge/server/master.key") {
		t.Errorf("降级模式 server.yaml 必须指向主密钥文件:\n%s", yml)
	}
	if fi, err := os.Stat(filepath.Join(w.Root, "etc/lingedge/server/master.key")); err != nil || fi.Mode().Perm() != 0o440 {
		t.Errorf("降级模式 master.key 应为 0440: %v %v", fi, err)
	}
	if res.Pre.CredentialMode != "keyfile" {
		t.Errorf("credential_mode=%s", res.Pre.CredentialMode)
	}
	warned := false
	for _, x := range res.Warnings {
		warned = warned || x.Code == "systemd.credentials_degraded"
	}
	if !warned {
		t.Error("应给出降级警告")
	}

	w2 := newW(t)
	_ = w2.SetSystemdVersion("systemd 234 (234)")
	before := sysSnap(t, w2)
	o := doInstall(t, w2, "server", nil)
	if o.err == nil || diffSnap(before, sysSnap(t, w2)) != "" {
		t.Fatalf("systemd 过旧应在任何副作用之前失败: %v", o.err)
	}
}

// --setup-listen 非回环：给出风险提示（向导在完成前对网络可达，仅凭 setup token 保护）。
func TestSetupListenExposureWarning(t *testing.T) {
	w := newW(t)
	pre, err := plan(t, w, "server", nil, func(r *installer.Request) { r.SetupListen = "0.0.0.0:8080" })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range pre.Warnings {
		found = found || x.Code == "setup.listen_exposed"
	}
	if !found {
		t.Fatal("非回环监听应告警")
	}
	pre, _ = plan(t, w, "server", nil)
	for _, x := range pre.Warnings {
		if x.Code == "setup.listen_exposed" {
			t.Fatal("默认回环监听不应告警")
		}
	}
}
