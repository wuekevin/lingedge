package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

func okComp() Component {
	return Component{Version: "1.2.3", State: "running", Unit: "lingedge-server.service", RunUser: "lingedge", Ports: []int{8080, 9443},
		ReleaseDir: "/opt/lingedge/server/releases/1.2.3", ConfigDir: "/etc/lingedge/server", DataDir: "/var/lib/lingedge/server",
		InstalledAt: "2026-11-21T12:00:00.000Z", PrevVersion: "1.2.2", LastTx: "01JD5Y3C8Q5G7M0V1N2P3R4S5T"}
}

func kindOfErr(t *testing.T, err error) Kind {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望 *registry.Error，得到 %T: %v", err, err)
	}
	return e.Kind
}

func TestStateRoundTripPermsAndMissing(t *testing.T) {
	r := newReg(t)
	if s, exists, err := r.LoadState(); err != nil || exists || len(s.Components) != 0 || s.SchemaVersion != StateSchemaVersion {
		t.Fatalf("缺失应得空状态: %+v %v %v", s, exists, err)
	}
	st := NewState()
	st.Components["server"] = okComp()
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	// umask 077 下仍应是 0644（权限不依赖 umask）。
	if err := security.WithUmask077(func() error { return r.SaveState(st) }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(r.L.StateFile())
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("state.json 权限 %v err=%v，期望 0644", fi.Mode().Perm(), err)
	}
	got, exists, err := r.LoadState()
	if err != nil || !exists || got.Components["server"].Version != "1.2.3" || got.Components["server"].PrevVersion != "1.2.2" || got.UpdatedAt == "" {
		t.Fatalf("往返不一致: %+v %v %v", got, exists, err)
	}
	// 目录布局权限：Root 0711，其余 0700。
	for p, want := range map[string]os.FileMode{r.L.Root: 0o711, r.L.JournalDir(): 0o700, r.L.LocksDir(): 0o700, r.L.StagingDir(): 0o700, r.L.TrustDir(): 0o700, r.L.ArchiveDir(): 0o700} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s 权限 %v，期望 %v (%v)", p, fi.Mode().Perm(), want, err)
		}
	}
}

func TestParseStateSchemaVersioning(t *testing.T) {
	good := `{"schema_version":1,"components":{}}`
	if _, err := ParseState([]byte(good)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   string
		kind Kind
	}{
		{"缺 schema_version", `{"components":{}}`, KindStateCorrupt},
		{"schema_version 为字符串", `{"schema_version":"1","components":{}}`, KindStateCorrupt},
		{"schema_version=0", `{"schema_version":0,"components":{}}`, KindStateCorrupt},
		{"schema_version 为负", `{"schema_version":-3,"components":{}}`, KindStateCorrupt},
		{"未来版本 3：失败关闭，不尝试解析其余内容", `{"schema_version":3,"components":{"x":{"future":true}},"new_top":1}`, KindStateUnsupported},
		{"巨大版本", `{"schema_version":99999999}`, KindStateUnsupported},
		{"未知顶层字段（同版本内不允许）", `{"schema_version":1,"components":{},"extra":1}`, KindStateCorrupt},
		{"未知组件字段", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"running","ports":[],"extra_field":"x"}}}`, KindStateCorrupt},
		{"尾随数据", good + `{"a":1}`, KindStateCorrupt},
		{"不是 JSON", `not json`, KindStateCorrupt},
		{"空", ``, KindStateCorrupt},
		{"组件名含路径", `{"schema_version":1,"components":{"../x":{"version":"1.0.0","state":"running","ports":[]}}}`, KindStateInvalid},
		{"版本不是 semver", `{"schema_version":1,"components":{"server":{"version":"latest","state":"running","ports":[]}}}`, KindStateInvalid},
		{"未知状态", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"zombie","ports":[]}}}`, KindStateInvalid},
		{"端口重复", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"running","ports":[80,80]}}}`, KindStateInvalid},
		{"端口越界", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"running","ports":[70000]}}}`, KindStateInvalid},
		{"相对路径", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"running","ports":[],"release_dir":"opt/x"}}}`, KindStateInvalid},
		{"非规范路径", `{"schema_version":1,"components":{"server":{"version":"1.0.0","state":"running","ports":[],"release_dir":"/opt/../etc"}}}`, KindStateInvalid},
	}
	for _, c := range cases {
		_, err := ParseState([]byte(c.in))
		if err == nil {
			t.Errorf("%s: 应失败", c.name)
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Kind != c.kind {
			t.Errorf("%s: kind=%v，期望 %v（%v）", c.name, kindOfErrSoft(err), c.kind, err)
		}
	}
}

func kindOfErrSoft(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return "?"
}

func TestUnsupportedFutureVersionIsNeverRewritten(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"schema_version":3,"components":{"server":{"v3_only":1}}}` + "\n")
	if err := os.WriteFile(r.L.StateFile(), future, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.LoadState()
	if kindOfErr(t, err) != KindStateUnsupported {
		t.Fatalf("得到 %v", err)
	}
	err = r.UpdateState(context.Background(), func(s *State) error { s.Components["x"] = okComp(); return nil })
	if kindOfErr(t, err) != KindStateUnsupported {
		t.Fatalf("UpdateState 应同样失败关闭: %v", err)
	}
	if b, _ := os.ReadFile(r.L.StateFile()); !bytes.Equal(b, future) {
		t.Fatalf("未来版本文件被改写: %s", b)
	}
}

// 注入式泄漏扫描：把多种形态的密钥塞进每个字符串字段，Save 必须拒绝，且磁盘上的旧文件/不存在状态保持不变。
func TestStateRefusesSecrets(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	base := NewState()
	base.Components["server"] = okComp()
	if err := r.SaveState(base); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.L.StateFile())
	secrets := []string{
		"password=hunter2hunter2", "token=abc123def456ghi789", "le_live_" + strings.Repeat("A1b2", 6), "lenr1." + strings.Repeat("xY9", 8),
		"postgres://admin:" + strings.Repeat("Qw7", 4) + "@db:5432/x", "Bearer " + strings.Repeat("a1B2c3", 8), "api_key: " + strings.Repeat("Zx9", 8),
	}
	setters := map[string]func(*Component, string){
		"version": func(c *Component, v string) { c.Version = "1.0.0+" + v }, "unit": func(c *Component, v string) { c.Unit = v + ".service" },
		"run_user": func(c *Component, v string) { c.RunUser = v }, "release_dir": func(c *Component, v string) { c.ReleaseDir = "/opt/" + v },
		"config_dir": func(c *Component, v string) { c.ConfigDir = "/etc/" + v }, "data_dir": func(c *Component, v string) { c.DataDir = "/var/" + v },
		"prev_version": func(c *Component, v string) { c.PrevVersion = "1.0.0+" + v }, "installed_at": func(c *Component, v string) { c.InstalledAt = v },
		"state": func(c *Component, v string) { c.State = v }, "last_tx": func(c *Component, v string) { c.LastTx = v },
	}
	for field, set := range setters {
		for _, sec := range secrets {
			s := NewState()
			c := okComp()
			set(&c, sec)
			s.Components["server"] = c
			err := r.SaveState(s)
			if err == nil {
				t.Errorf("%s=%q 应被拒绝写入", field, sec)
			}
			if after, _ := os.ReadFile(r.L.StateFile()); !bytes.Equal(after, before) {
				t.Fatalf("拒绝后磁盘内容被改变（%s）", field)
			}
		}
	}
	// 泄漏检查本身有效（而不只是被格式校验挡住）：一个格式合法但含口令的版本号尾缀必须以 KindSecret 被拒。
	c := okComp()
	c.Version = "1.0.0+password=hunter2hunter2"
	s := NewState()
	s.Components["server"] = c
	if k := kindOfErr(t, r.SaveState(s)); k != KindSecret && k != KindStateInvalid {
		t.Fatalf("kind=%v", k)
	}
	// 落盘文件中不含任何注入过的密钥串。
	for _, sec := range secrets {
		if bytes.Contains(before, []byte(sec)) {
			t.Fatalf("state.json 含密钥 %q", sec)
		}
	}
}

func TestLoadStateRejectsUnsafeFiles(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"schema_version":1,"components":{}}`)
	// group 可写：拒绝。
	if err := os.WriteFile(r.L.StateFile(), valid, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.L.StateFile(), 0o664); err != nil {
		t.Fatal(err)
	}
	if k := kindOfErr(t, firstErr(r.LoadState())); k != KindPermission {
		t.Fatalf("kind=%v", k)
	}
	// 符号链接：拒绝（O_NOFOLLOW）。
	_ = os.Remove(r.L.StateFile())
	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(real, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, r.L.StateFile()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.LoadState(); err == nil {
		t.Fatal("符号链接 state.json 应被拒绝")
	}
	// 目录：拒绝。
	_ = os.Remove(r.L.StateFile())
	if err := os.Mkdir(r.L.StateFile(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.LoadState(); err == nil {
		t.Fatal("目录应被拒绝")
	}
	// 过大：拒绝。
	_ = os.Remove(r.L.StateFile())
	if err := os.WriteFile(r.L.StateFile(), append([]byte(`{"schema_version":1,"components":{},"x":"`), bytes.Repeat([]byte("a"), maxStateBytes+10)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if k := kindOfErr(t, firstErr(r.LoadState())); k != KindStateCorrupt {
		t.Fatalf("kind=%v", k)
	}
}

func firstErr(_ State, _ bool, err error) error { return err }

func TestUpdateStateFnErrorLeavesFileUntouched(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	if err := r.UpdateState(ctx, func(s *State) error { s.Components["server"] = okComp(); return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.L.StateFile())
	boom := errors.New("boom")
	if err := r.UpdateState(ctx, func(s *State) error { s.Components["node"] = okComp(); return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// 校验失败同样不写。
	if err := r.UpdateState(ctx, func(s *State) error { s.Components["Bad Name"] = okComp(); return nil }); err == nil {
		t.Fatal("非法组件名应被拒绝")
	}
	if after, _ := os.ReadFile(r.L.StateFile()); !bytes.Equal(before, after) {
		t.Fatal("失败的更新改动了文件")
	}
	ents, _ := os.ReadDir(r.L.Root)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("遗留临时文件 %s", e.Name())
		}
	}
}

// ---- state.json 原子性：真实故障注入 ----

// 写失败（真实 write(2) 中途失败：RLIMIT_FSIZE）：SaveState 报错、旧文件字节不变、不遗留临时文件。
func TestStateAtomicWriteFaultRLimit(t *testing.T) {
	r := newReg(t)
	if err := r.UpdateState(context.Background(), func(s *State) error { s.Components["server"] = okComp(); return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.L.StateFile())
	c := startChild(t, r.L.Root, "fsize")
	if l := c.line(); !strings.HasPrefix(l, "FAILED") {
		t.Fatalf("子进程应报告写失败，得到 %q", l)
	}
	_ = c.cmd.Wait()
	after, _ := os.ReadFile(r.L.StateFile())
	if !bytes.Equal(before, after) {
		t.Fatal("写失败后旧文件内容被改变")
	}
	if _, _, err := r.LoadState(); err != nil {
		t.Fatalf("旧文件应仍可读: %v", err)
	}
	ents, _ := os.ReadDir(r.L.Root)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("写失败后遗留临时文件 %s", e.Name())
		}
	}
}

// rename 失败（目标是非空目录）与目录不可写：旧状态保持，且不留临时文件。
func TestStateAtomicRenameAndPermFaults(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.L.StateFile(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := NewState()
	st.Components["server"] = okComp()
	if err := r.SaveState(st); err == nil {
		t.Fatal("目标是目录时应失败")
	}
	_ = os.RemoveAll(r.L.StateFile())
	if err := r.SaveState(st); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.L.StateFile())
	if os.Geteuid() != 0 { // root 不受目录权限约束
		if err := os.Chmod(r.L.Root, 0o500); err != nil {
			t.Fatal(err)
		}
		st.Components["node"] = okComp()
		err := r.SaveState(st)
		_ = os.Chmod(r.L.Root, 0o711)
		if err == nil {
			t.Fatal("目录不可写时应失败")
		}
		if after, _ := os.ReadFile(r.L.StateFile()); !bytes.Equal(before, after) {
			t.Fatal("旧文件被改变")
		}
	}
	ents, _ := os.ReadDir(r.L.Root)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("遗留临时文件 %s", e.Name())
		}
	}
}

// kill -9 打断持续写入：读者在任何时刻读到的都是某个完整版本（所有组件版本一致、可解析、版本号单调不减）。
func TestStateAtomicUnderKill9(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	last := 0
	for round := 0; round < 25; round++ {
		c := startChild(t, r.L.Root, "state-loop")
		c.expect("STARTED")
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, exists, err := r.LoadState()
				if err != nil {
					t.Errorf("读到损坏/半个文件: %v", err)
					return
				}
				if !exists {
					continue
				}
				v := -1
				for name, comp := range s.Components {
					n, perr := strconv.Atoi(strings.TrimPrefix(comp.Version, "1.0."))
					if perr != nil || (v != -1 && n != v) {
						t.Errorf("读到混合版本的状态（组件 %s=%s，其他=%d）", name, comp.Version, v)
						return
					}
					v = n
				}
				if len(s.Components) != 60 || v < last {
					t.Errorf("组件数 %d 版本 %d（上次 %d）", len(s.Components), v, last)
					return
				}
				last = v
			}
		}()
		sleepRand(round)
		c.kill9()
		close(stop)
		<-done
		if t.Failed() {
			return
		}
	}
	// 被杀后：文件完整；遗留的临时文件可被回收，且不影响读取。
	if _, exists, err := r.LoadState(); err != nil || !exists {
		t.Fatalf("kill 后状态应可读: %v", err)
	}
	if _, err := r.CleanStaleTemps(context.Background()); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(r.L.Root)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".state.json.tmp-") {
			t.Errorf("CleanStaleTemps 后仍有 %s", e.Name())
		}
	}
}

func TestMarshalStateIsStableJSON(t *testing.T) {
	st := NewState()
	st.Components["server"] = okComp()
	b, err := MarshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil || b[len(b)-1] != '\n' {
		t.Fatalf("输出不是以换行结尾的 JSON: %v", err)
	}
	if _, ok := generic["schema_version"]; !ok {
		t.Fatal("缺 schema_version")
	}
}

// ---- Story 1.31：schema 版本 2（host.baota）与对版本 1 的兼容读取 ----

const v1Doc = `{"schema_version":1,"updated_at":"2026-11-01T00:00:00.000Z","components":{"server":{"version":"1.2.3","state":"running","unit":"lingedge-server.service","run_user":"lingedge","ports":[8080],"release_dir":"/opt/lingedge/server/releases/1.2.3","config_dir":"","data_dir":"","installed_at":"2026-11-01T00:00:00.000Z"}}}`

func sampleBaota() *baota.Report {
	return &baota.Report{ProbedAt: "2026-11-21T01:02:03.000Z", Detected: true, Confidence: "medium", PanelPort: 8888,
		Evidence:   []baota.Evidence{{Kind: "panel_dir", Path: "/www/server/panel"}, {Kind: "port_file", Path: "/www/server/panel/data/port.pl", Detail: "port=8888"}},
		WebServers: []baota.WebServer{{Variant: "baota", Kind: "nginx", Binary: "/www/server/nginx/sbin/nginx", Version: "1.24.0"}},
		Databases:  []baota.DBInstall{}, PHP: []baota.PHP{}, Firewalls: []baota.Firewall{}, WAF: []baota.WAF{}, Plugins: []string{}, Services: []baota.Service{}, Notes: []string{},
		Sites: []baota.Site{{File: "/www/server/panel/vhost/nginx/a.conf", Source: "baota", ServerNames: []string{"a.example.com"}, Listens: []baota.Listen{{Addr: "*", Port: 80}}}}}
}

func TestStateV1ReadsAndUpgradesToV2(t *testing.T) {
	s, err := ParseState([]byte(v1Doc))
	if err != nil {
		t.Fatalf("版本 1 的文件必须仍可读取: %v", err)
	}
	if s.SchemaVersion != 2 || s.Host != nil || s.Components["server"].Version != "1.2.3" || s.UpdatedAt != "2026-11-01T00:00:00.000Z" {
		t.Fatalf("迁移结果: %+v", s)
	}
	// 经真实文件：读旧文件 → 记录 host.baota → 落盘为版本 2，组件记录原样保留
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.L.StateFile(), []byte(v1Doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordHostBaota(context.Background(), *sampleBaota()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(r.L.StateFile())
	if !bytes.Contains(raw, []byte(`"schema_version": 2`)) || !bytes.Contains(raw, []byte(`"host"`)) {
		t.Fatalf("应落盘为版本 2: %s", raw)
	}
	got, _, err := r.LoadState()
	if err != nil || got.Components["server"].RunUser != "lingedge" || got.Host == nil || got.Host.Baota == nil || got.Host.Baota.PanelPort != 8888 {
		t.Fatalf("读回: %+v %v", got, err)
	}
	// 版本 1 的文件里不应有 host（该字段自版本 2 起）
	bad := strings.Replace(v1Doc, `"components"`, `"host":{},"components"`, 1)
	if _, err := ParseState([]byte(bad)); err == nil || kindOfErrSoft(err) != KindStateCorrupt {
		t.Errorf("v1 含 host 应为损坏: %v", err)
	}
	// 未知顶层字段在版本 1 与 2 都是损坏
	for _, v := range []string{"1", "2"} {
		if _, err := ParseState([]byte(`{"schema_version":` + v + `,"components":{},"zzz":1}`)); kindOfErrSoft(err) != KindStateCorrupt {
			t.Errorf("v%s 未知字段: %v", v, err)
		}
	}
	// 版本 3 仍失败关闭
	if _, err := ParseState([]byte(`{"schema_version":3,"components":{},"host":{"x":1}}`)); kindOfErrSoft(err) != KindStateUnsupported {
		t.Errorf("v3: %v", err)
	}
}

func TestHostBaotaValidationAndStrictness(t *testing.T) {
	mk := func(mut func(*baota.Report)) State {
		s := NewState()
		b := *sampleBaota()
		mut(&b)
		s.Host = &Host{Baota: &b}
		return s
	}
	if _, err := MarshalState(mk(func(*baota.Report) {})); err != nil {
		t.Fatalf("合法报告: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*baota.Report)
		kind Kind
	}{
		{"confidence 与 detected 不一致", func(b *baota.Report) { b.Confidence = "" }, KindStateInvalid},
		{"端口越界", func(b *baota.Report) { b.PanelPort = 99999 }, KindStateInvalid},
		{"相对路径", func(b *baota.Report) { b.Evidence[0].Path = "www/server/panel" }, KindStateInvalid},
		{"站点来源非法", func(b *baota.Report) { b.Sites[0].Source = "nowhere" }, KindStateInvalid},
		{"疑似密钥", func(b *baota.Report) { b.Notes = []string{"password=hunter2hunter2"} }, KindSecret},
		{"令牌形态", func(b *baota.Report) {
			b.Sites[0].ServerNames = []string{"Bearer abcdefghijklmnopqrstuvwxyz0123456789"}
		}, KindSecret},
	}
	for _, c := range cases {
		_, err := MarshalState(mk(c.mut))
		if err == nil || kindOfErrSoft(err) != c.kind {
			t.Errorf("%s: kind=%v err=%v，期望 %v", c.name, kindOfErrSoft(err), err, c.kind)
		}
	}
	// 经 RecordHostBaota 写入时，形似令牌的站点名先被 Scrub 遮蔽，写入成功而不是整个 state.json 写失败
	r := newReg(t)
	b := *sampleBaota()
	b.Sites = []baota.Site{{File: "/a.conf", Source: "baota", ServerNames: []string{"ok.example.com", "Bearer abcdefghijklmnopqrstuvwxyz0123456789"}, Listens: []baota.Listen{}}}
	if err := r.RecordHostBaota(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	st, _, _ := r.LoadState()
	if st.Host.Baota.Sites[0].ServerNames[0] != "ok.example.com" || strings.Contains(st.Host.Baota.Sites[0].ServerNames[1], "abcdefghij") {
		t.Errorf("Scrub 结果: %v", st.Host.Baota.Sites[0].ServerNames)
	}
	// host.baota 内的未知字段（严格解析）是损坏
	doc := `{"schema_version":2,"components":{},"host":{"baota":{"detected":false,"evidence":[],"web_servers":[],"databases":[],"php":[],"sites":[],"firewalls":[],"waf":[],"plugins":[],"services":[],"notes":[],"surprise":1}}}`
	if _, err := ParseState([]byte(doc)); kindOfErrSoft(err) != KindStateCorrupt {
		t.Errorf("host.baota 未知字段: %v", err)
	}
	// 并发：RecordHostBaota 与 UpdateState 互不丢更新
	r2 := newReg(t)
	done := make(chan error, 8)
	for i := 0; i < 4; i++ {
		go func() { done <- r2.RecordHostBaota(context.Background(), *sampleBaota()) }()
		go func(i int) {
			done <- r2.UpdateState(context.Background(), func(s *State) error { s.Components["c"+string(rune('a'+i))] = okComp(); return nil })
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	final, _, _ := r2.LoadState()
	if len(final.Components) != 4 || final.Host == nil {
		t.Errorf("并发后: %d 个组件，host=%v", len(final.Components), final.Host)
	}
}
