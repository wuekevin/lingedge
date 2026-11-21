package installer_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.35：server 的生产 ComponentSpec（installer.ProductionSpecs()["server"]）经 toolsworld 的「伪操作系统」跑完整安装流程。
// 真实：状态机、journal、撤销、冲突判定、registry、downloader、resolver、service、server.yaml/master.key/setup-token 生成、生产 Health（真实 HTTP GET /healthz）。
// 伪的只有「操作系统」（伪 systemctl 等）——它不会真的 fork lingedge-server，所以「健康」要么没有监听者（健康失败 → 回滚），要么由测试起一个真实 HTTP 监听者。
var update = flag.Bool("update", false, "重写 testdata 下的 golden")

func prodWorld(t testing.TB) *toolsworld.World {
	t.Helper()
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{ProductionServerSpec: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func fastHealth(d *installer.Deps) {
	d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "1500ms", "100ms"
}

func freeAddr(t testing.TB) (string, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), l.Addr().(*net.TCPAddr).Port
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("缺少 golden %s（go test ./internal/tools/installer -run %s -update）: %v", p, t.Name(), err)
	}
	if string(want) != string(got) {
		t.Errorf("%s 与 golden 不一致\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// ---- 渲染 golden ----

func TestRenderServerYAMLGolden(t *testing.T) {
	ports := func(over ...resolver.PlannedPort) []resolver.PlannedPort {
		base := []resolver.PlannedPort{
			{Component: "server", Proto: "tcp", Port: 8080, Bind: "127.0.0.1", Purpose: "rest"},
			{Component: "server", Proto: "tcp", Port: 9443, Bind: "0.0.0.0", Purpose: "grpc-mtls"},
			{Component: "server", Proto: "tcp", Port: 9444, Bind: "0.0.0.0", Purpose: "grpc-enroll", Conditional: true},
			{Component: "server", Proto: "tcp", Port: 9180, Bind: "127.0.0.1", Purpose: "metrics"},
		}
		for _, o := range over {
			for i := range base {
				if base[i].Purpose == o.Purpose {
					base[i] = o
				}
			}
		}
		return base
	}
	cases := []struct {
		name   string
		set    map[string]string
		listen string
		cred   service.CredentialMode
		ports  []resolver.PlannedPort
	}{
		{"server.default.yaml", nil, "", service.CredentialSystemd, ports()},
		{"server.degraded.yaml", map[string]string{"env": "staging", "trusted-proxies": "10.0.0.0/8,127.0.0.1"}, "", service.CredentialKeyFile, ports()},
		{"server.ports.yaml", nil, "", service.CredentialSystemd, ports(
			resolver.PlannedPort{Component: "server", Proto: "tcp", Port: 18080, Bind: "127.0.0.1", Purpose: "rest", Overridden: true},
			resolver.PlannedPort{Component: "server", Proto: "tcp", Port: 19443, Bind: "10.0.0.5", Purpose: "grpc-mtls", Overridden: true},
			resolver.PlannedPort{Component: "server", Proto: "tcp", Port: 19180, Bind: "127.0.0.1", Purpose: "metrics", Overridden: true})},
		{"server.exposed.yaml", nil, "0.0.0.0:8080", service.CredentialSystemd, ports()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := installer.ServerYAMLFromPlan(c.set, c.listen, c.cred, c.ports)
			if err != nil {
				t.Fatal(err)
			}
			b, err := installer.RenderServerYAML(p)
			if err != nil {
				t.Fatal(err)
			}
			b2, _ := installer.RenderServerYAML(p)
			if string(b) != string(b2) {
				t.Fatal("渲染必须确定性")
			}
			if strings.Contains(strings.ToLower(string(b)), "password") || strings.Contains(string(b), "k1:") {
				t.Fatalf("server.yaml 不得含口令/密钥:\n%s", b)
			}
			golden(t, c.name, b)
		})
	}
}

func TestServerListensFromPlan(t *testing.T) {
	def := []resolver.PlannedPort{{Component: "server", Proto: "tcp", Port: 8080, Bind: "127.0.0.1", Purpose: "rest"}}
	if l, err := installer.ServerListensFromPlan("", def); err != nil || l.HTTP != "127.0.0.1:8080" || l.Metrics != "127.0.0.1:9180" {
		t.Fatalf("缺省: %+v %v", l, err)
	}
	if l, err := installer.ServerListensFromPlan("0.0.0.0:8080", def); err != nil || l.HTTP != "0.0.0.0:8080" {
		t.Fatalf("--setup-listen: %+v %v", l, err)
	}
	over := []resolver.PlannedPort{{Component: "server", Proto: "tcp", Port: 18080, Bind: "127.0.0.1", Purpose: "rest", Overridden: true}}
	if l, err := installer.ServerListensFromPlan("", over); err != nil || l.HTTP != "127.0.0.1:18080" {
		t.Fatalf("换端口: %+v %v", l, err)
	}
	if _, err := installer.ServerListensFromPlan("0.0.0.0:8080", over); err == nil {
		t.Error("--setup-listen 与换端口矛盾必须报错")
	}
	// 与换端口一致的显式 --setup-listen 不算矛盾。
	if _, err := installer.ServerListensFromPlan("127.0.0.1:18080", over); err != nil {
		t.Errorf("一致的组合不应报错: %v", err)
	}
	// metrics 必须是回环（Server 会拒绝启动）。
	bad := []resolver.PlannedPort{{Component: "server", Proto: "tcp", Port: 9180, Bind: "0.0.0.0", Purpose: "metrics", Overridden: true}}
	p, err := installer.ServerYAMLFromPlan(nil, "", service.CredentialSystemd, bad)
	if err == nil {
		t.Fatalf("metrics 绑定非回环必须被拒绝: %+v", p)
	}
}

func TestServerHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080/healthz",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/healthz",
		":18080":         "http://127.0.0.1:18080/healthz",
		"[::]:8080":      "http://[::1]:8080/healthz",
		"[::1]:8080":     "http://[::1]:8080/healthz",
		"10.1.2.3:9000":  "http://10.1.2.3:9000/healthz",
		"":               "http://127.0.0.1:8080/healthz",
		"garbage":        "http://127.0.0.1:8080/healthz",
	} {
		if got := installer.ServerHealthURL(in); got != want {
			t.Errorf("ServerHealthURL(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestServerSpecBasics(t *testing.T) {
	spec := installer.ProductionSpecs()["server"]
	if spec == nil || spec.Name() != "server" {
		t.Fatal("ProductionSpecs 必须登记 server")
	}
	if ex := spec.Executables("0.1.0", ""); len(ex) != 1 || ex[0] != "payload/bin/lingedge-server" {
		t.Errorf("Executables: %v", ex)
	}
	if err := spec.ValidateSettings(nil); err != nil {
		t.Error(err)
	}
	// 数据库/Redis/口令类的键一律拒绝：Tools 不接触口令（AC6 ①）。
	for _, k := range []string{"database.host", "db-password", "redis.addr", "mysql-user", "migrate-password"} {
		if err := spec.ValidateSettings(map[string]string{k: "x"}); err == nil {
			t.Errorf("server 不应接受配置项 %q", k)
		}
	}
	cf, err := spec.InitConfig(context.Background(), installer.ConfigInput{Component: "server"})
	if err != nil || len(cf) != 0 {
		t.Errorf("server 的 InitConfig 不返回文件（由 installer 内建生成）: %v %v", cf, err)
	}
	// 生产 Health 是真实 HTTP：200 健康，503/连接拒绝不健康。
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	rt := &installer.Runtime{Component: "server", SetupListen: listen}
	if err := spec.Health(context.Background(), rt); err != nil {
		t.Errorf("200 应健康: %v", err)
	}
	code = http.StatusServiceUnavailable
	if err := spec.Health(context.Background(), rt); err == nil {
		t.Error("503 不应健康")
	}
	srv.Close()
	if err := spec.Health(context.Background(), rt); err == nil {
		t.Error("无监听者不应健康")
	}
}

// ---- 安装流程（生产规格）----

// AC5：激活后健康检查失败 → 自动回滚（退出码 10 的来源 KindRolledBack），不残留用户/单元/目录/配置；伪 systemd 里单元已停用/禁用。
func TestServerHealthFailureRollsBackWithoutResidue(t *testing.T) {
	w := prodWorld(t)
	before := sysSnap(t, w)
	o := doInstall(t, w, "server", []func(*installer.Deps){fastHealth})
	if o.err == nil || kindOf(t, o.err) != installer.KindRolledBack {
		t.Fatalf("健康失败应回滚（KindRolledBack）: %v", o.err)
	}
	var ie *installer.Error
	if !errors.As(o.err, &ie) || ie.SystemState != "rolled_back" || !strings.Contains(o.err.Error(), "healthz") {
		t.Errorf("错误应指明 healthz 与已回滚: %+v", ie)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("回滚后系统状态与安装前不同（残留）:\n%s", d)
	}
	for _, rel := range []string{"etc/lingedge/server", "var/lib/lingedge/server", "var/log/lingedge/server", "opt/lingedge/server", "etc/systemd/system/lingedge-server.service"} {
		if w.Has(rel) {
			t.Errorf("回滚后不应残留 %s", rel)
		}
	}
	for _, f := range []string{"users/lingedge", "active-lingedge-server.service", "enabled-lingedge-server.service"} {
		if w.FakeHas(f) {
			t.Errorf("回滚后伪 systemd/用户状态不应残留 %s", f)
		}
	}
}

// AC1/AC6：健康（本机有真实 HTTP 监听者回应 /healthz）→ 完整安装；访问信息含向导地址、SSH 隧道提示、醒目的主密钥提示、只显示一次的 token；
// 落盘的文件权限、server.yaml 随换端口、哈希与 token 对应；MySQL/Redis 未探测到只警告不阻止（AC2）；Tools 不写 migrate.yaml、磁盘上没有任何口令（AC6 ①）。
func TestServerInstallHappyPathWithRealHealthEndpoint(t *testing.T) {
	w := prodWorld(t)
	addr, port := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			rw.WriteHeader(200)
			return
		}
		http.NotFound(rw, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	o := doInstall(t, w, "server", []func(*installer.Deps){fastHealth}, func(r *installer.Request) {
		r.PortOverrides = []resolver.PortOverride{{Component: "server", Proto: "tcp", Port: 8080, To: port}}
	})
	res := must(t, o)
	var url, ssh, warn, token, fp string
	for _, a := range res.Access {
		switch a.Kind {
		case "setup_url":
			url = a.Value
		case "ssh_tunnel":
			ssh = a.Value
		case "master_key_warning":
			warn = a.Label + a.Value
		case "setup_token":
			token = a.Value
			if !a.Secret {
				t.Error("token 必须标 secret")
			}
		case "master_key_fingerprint":
			fp = a.Value
		}
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(port) + "/"; url != want {
		t.Errorf("向导地址 %q，期望 %q（随换端口）", url, want)
	}
	if !strings.Contains(ssh, "ssh -L "+strconv.Itoa(port)+":127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("SSH 隧道提示: %q", ssh)
	}
	if !strings.Contains(warn, "分开保管") || !strings.Contains(warn, "master.key") || fp == "" {
		t.Errorf("主密钥醒目提示/指纹: %q / %q", warn, fp)
	}
	if len(token) != 43 {
		t.Fatalf("token 应是 256 位 base64url（43 字符）: %q", token)
	}
	hash, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/setup-token.hash"))
	if !setuptoken.Verify(token, string(hash)) {
		t.Error("setup-token.hash 与显示的 token 不对应")
	}
	yml, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(yml), "listen: 127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("server.yaml 的 http.listen 应随换端口:\n%s", yml)
	}
	// AC2：MySQL/Redis 未探测到 → 警告（不阻止）。
	var mysqlWarn, redisWarn bool
	for _, wn := range res.Warnings {
		if wn.Code == "external.remote_assumed" && wn.Step == installer.StepDatabases {
			mysqlWarn = mysqlWarn || strings.Contains(wn.Message, "mysql")
			redisWarn = redisWarn || strings.Contains(wn.Message, "redis")
		}
	}
	dep := mysqlWarn && redisWarn
	if !dep {
		t.Errorf("MySQL/Redis 未探测到应给出警告（不阻止）: %+v", res.Warnings)
	}
	// AC6 ①：不写 migrate.yaml，不要求/不接触任何数据库口令。
	assertNoMigrateYAMLAndNoDBSecrets(t, w.Root)
}

func assertNoMigrateYAMLAndNoDBSecrets(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), "migrate.yaml") || strings.Contains(d.Name(), "runtime-credentials") {
			t.Errorf("Tools 不得写入 %s（migrate.yaml 由 lingedge-server migrate credential init 写；运行期凭据由向导写）", p)
		}
		rel, _ := filepath.Rel(root, p)
		if !strings.HasPrefix(rel, "etc/") && !strings.HasPrefix(rel, "var/") { // 只扫配置与状态（bin/ 是伪二进制脚本，opt/ 是包内容）
			return nil
		}
		b, _ := os.ReadFile(p)
		low := strings.ToLower(string(b))
		pw := "pass" + "word" // 运行期拼接：本文件不含「口令键 = 字面量」形态（G-6）
		for _, bad := range []string{pw + ":", "passwd", "migrate_" + pw, "mysql://", "redis://"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s 含数据库/Redis 口令相关内容 %q", p, bad)
			}
		}
		return nil
	})
}

// AC3：既有 master.key/server.yaml/同名单元 → 绝不覆盖，新内容写 .new；沿用的 master.key 仍给出（已有文件的）指纹与保管提示。
func TestServerExistingFilesAreNeverOverwritten(t *testing.T) {
	w := prodWorld(t)
	writeFile := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(w.Root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("etc/lingedge/server/master.key", "@current k9\nk9:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", 0o400)
	writeFile("etc/lingedge/server/server.yaml", "# 管理员手工配置\nserver:\n  env: custom\n", 0o640)
	writeFile("etc/systemd/system/lingedge-server.service", "[Service]\n# 手工单元\n", 0o644)
	keyBefore, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/master.key"))
	ymlBefore, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	unitBefore, _ := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-server.service"))

	// 非交互且无 --yes：冲突需确认（KindConfirmRequired），什么都不写。
	o := doInstall(t, w, "server", []func(*installer.Deps){fastHealth}, func(r *installer.Request) { r.SkipStart = true })
	if o.err == nil || kindOf(t, o.err) != installer.KindConfirmRequired {
		t.Fatalf("既有内容不同应要求确认: %v", o.err)
	}
	if w.Has("etc/lingedge/server/server.yaml.new") {
		t.Fatal("确认前不得写 .new")
	}
	res := must(t, doInstall(t, w, "server", []func(*installer.Deps){fastHealth}, func(r *installer.Request) { r.SkipStart, r.Yes = true, true }))
	for rel, before := range map[string][]byte{"etc/lingedge/server/master.key": keyBefore, "etc/lingedge/server/server.yaml": ymlBefore, "etc/systemd/system/lingedge-server.service": unitBefore} {
		got, _ := os.ReadFile(filepath.Join(w.Root, rel))
		if string(got) != string(before) {
			t.Errorf("%s 被覆盖了", rel)
		}
	}
	if !w.Has("etc/lingedge/server/server.yaml.new") || !w.Has("etc/systemd/system/lingedge-server.service.new") {
		t.Error("冲突的新内容应写成 .new")
	}
	if w.Has("etc/lingedge/server/master.key.new") {
		t.Error("master.key 是随机生成物：已存在即沿用，不写 .new")
	}
	var fp string
	for _, a := range res.Access {
		if a.Kind == "master_key_fingerprint" {
			fp = a.Value
		}
	}
	if fp == "" || !strings.HasPrefix(fp, sha16(keyBefore)) {
		t.Errorf("沿用既有 master.key 时应给出既有文件的指纹 %s: %q", sha16(keyBefore), fp)
	}
	kept := false
	for _, a := range res.Access {
		kept = kept || a.Kind == "master_key_warning"
	}
	if !kept {
		t.Error("沿用时同样要有分开保管的提示")
	}
}

// AC4：systemd < 247 → 降级方案并告警（单元无 LoadCredential，master.key 0440，server.yaml 指向密钥文件）；< 235 → 失败。
func TestServerLoadCredentialDegradation(t *testing.T) {
	w := prodWorld(t)
	if err := w.SetSystemdVersion("systemd 245 (245.4-4ubuntu3.22)"); err != nil {
		t.Fatal(err)
	}
	res := must(t, doInstall(t, w, "server", nil, func(r *installer.Request) { r.SkipStart = true }))
	var warned bool
	for _, wn := range res.Warnings {
		warned = warned || wn.Code == "systemd.credentials_degraded"
	}
	if !warned {
		t.Errorf("应有 systemd.credentials_degraded 告警: %+v", res.Warnings)
	}
	unit, _ := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-server.service"))
	if strings.Contains(strings.ReplaceAll(string(unit), "\n#", "\n"), "\nLoadCredential=") {
		t.Errorf("降级模式的单元不得含 LoadCredential=:\n%s", unit)
	}
	fi, err := os.Stat(filepath.Join(w.Root, "etc/lingedge/server/master.key"))
	if err != nil || fi.Mode().Perm() != 0o440 {
		t.Errorf("降级模式 master.key 应为 0440: %v %v", fi, err)
	}
	yml, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(yml), "master_key_file: /etc/lingedge/server/master.key") {
		t.Errorf("降级模式 server.yaml 应指向主密钥文件:\n%s", yml)
	}
	w2 := prodWorld(t)
	_ = w2.SetSystemdVersion("systemd 219")
	o := doInstall(t, w2, "server", nil, func(r *installer.Request) { r.SkipStart = true })
	if o.err == nil || kindOf(t, o.err) != installer.KindStepFailed {
		t.Errorf("systemd < 235 应失败: %v", o.err)
	}
}

// --setup-listen 非回环 → 风险提示（预检警告 + 访问信息的醒目提示）。
func TestServerSetupListenExposedWarns(t *testing.T) {
	w := prodWorld(t)
	pre, err := w.NewInstaller("r").Plan(t.Context(), baseReq(w, "server", func(r *installer.Request) { r.SetupListen = "0.0.0.0:8080" }))
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	for _, wn := range pre.Warnings {
		ok = ok || (wn.Code == "setup.listen_exposed" && strings.Contains(wn.Message, "SSH 隧道") && strings.Contains(wn.Message, "token"))
	}
	if !ok {
		t.Errorf("应有 setup.listen_exposed 风险提示: %+v", pre.Warnings)
	}
	res := must(t, doInstall(t, w, "server", nil, func(r *installer.Request) { r.SetupListen, r.SkipStart = "0.0.0.0:8080", true }))
	var risk, ssh bool
	for _, a := range res.Access {
		risk = risk || (a.Kind == "note" && strings.Contains(a.Label, "风险"))
		ssh = ssh || a.Kind == "ssh_tunnel"
	}
	if !risk || ssh {
		t.Errorf("非回环监听应给出风险提示、不给 SSH 隧道: %+v", res.Access)
	}
	yml, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(yml), "listen: 0.0.0.0:8080") {
		t.Errorf("server.yaml:\n%s", yml)
	}
}

// --setup-listen 与 --set 换端口矛盾 → 退出码 2 的 KindSettings，且没有任何副作用。
func TestServerSetupListenConflictsWithPortOverride(t *testing.T) {
	w := prodWorld(t)
	before := sysSnap(t, w)
	o := doInstall(t, w, "server", nil, func(r *installer.Request) {
		r.SetupListen = "0.0.0.0:8080"
		r.PortOverrides = []resolver.PortOverride{{Component: "server", Proto: "tcp", Port: 8080, To: 18080}}
	})
	if o.err == nil || kindOf(t, o.err) != installer.KindSettings {
		t.Fatalf("矛盾应是 KindSettings: %v", o.err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Errorf("预检失败不得有副作用:\n%s", d)
	}
}

// ---- config setup-token --reset ----

func TestResetSetupToken(t *testing.T) {
	w := prodWorld(t)
	in := w.NewInstaller("r")
	// 未安装：前置条件失败。
	if _, err := in.ResetSetupToken(t.Context()); err == nil || kindOf(t, err) != installer.KindPrecondition {
		t.Fatalf("server 未安装应是 KindPrecondition: %v", err)
	}
	inst := must(t, doInstall(t, w, "server", nil, func(r *installer.Request) { r.SkipStart = true }))
	var first string
	for _, a := range inst.Access {
		if a.Kind == "setup_token" {
			first = a.Value
		}
	}
	hp := filepath.Join(w.Root, "etc/lingedge/server/setup-token.hash")
	h1, _ := os.ReadFile(hp)
	w.Own.Calls = nil
	res, err := w.NewInstaller("r2").ResetSetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced || len(res.Token) != 43 || res.Token == first {
		t.Fatalf("结果: replaced=%v token=%q", res.Replaced, res.Token)
	}
	h2, _ := os.ReadFile(hp)
	if string(h1) == string(h2) || !setuptoken.Verify(res.Token, string(h2)) || setuptoken.Verify(first, string(h2)) {
		t.Error("新哈希必须对应新 token，旧 token 必须失效")
	}
	fi, _ := os.Stat(hp)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("哈希文件应为 0600: %v", fi.Mode())
	}
	if len(w.Own.Calls) != 1 || !strings.Contains(w.Own.Calls[0], ".setup-token-") {
		t.Errorf("应在 rename 之前对临时文件 chown 为运行用户（任何时刻属主正确）: %v", w.Own.Calls)
	}
	if ents, _ := os.ReadDir(filepath.Dir(hp)); func() bool {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".setup-token-") {
				return true
			}
		}
		return false
	}() {
		t.Error("不得残留临时文件")
	}
	if strings.Contains(string(h2), res.Token) {
		t.Error("token 明文不得落盘")
	}
	// 向导已完成（install.lock 存在）→ 前置条件失败，哈希文件不动。
	lock := filepath.Join(w.Root, "var/lib/lingedge/server/install.lock")
	if err := os.WriteFile(lock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewInstaller("r3").ResetSetupToken(t.Context()); err == nil || kindOf(t, err) != installer.KindPrecondition {
		t.Fatalf("向导已完成应拒绝: %v", err)
	}
	h3, _ := os.ReadFile(hp)
	if string(h3) != string(h2) {
		t.Error("拒绝时哈希文件必须原样保留")
	}
	// 取消的上下文：不做任何事。
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = os.Remove(lock)
	if _, err := w.NewInstaller("r4").ResetSetupToken(ctx); err == nil {
		t.Error("已取消的上下文应失败")
	}
	_ = time.Second
}

func sha16(b []byte) string {
	h := sha256Hex(b)
	return h[:16]
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// 单元的 ExecStart 必须指向安装后真实存在且可执行的文件：包布局（payload/bin/lingedge-server，Story 1.34）→ releases/<v>/bin/lingedge-server → current/bin/lingedge-server。
// （Story 1.35 的联调发现：1.32 的单元模板曾写成 current/lingedge-server，与发行流水线的包布局不一致，真实启动会 ENOENT——此测试防止再次漂移。）
func TestServerUnitExecStartMatchesInstalledLayout(t *testing.T) {
	w := prodWorld(t)
	must(t, doInstall(t, w, "server", nil, func(r *installer.Request) { r.SkipStart = true }))
	unit, err := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-server.service"))
	if err != nil {
		t.Fatal(err)
	}
	var exec string
	for _, ln := range strings.Split(string(unit), "\n") {
		if rest, ok := strings.CutPrefix(ln, "ExecStart="); ok {
			exec = strings.Fields(rest)[0]
		}
	}
	if !strings.HasPrefix(exec, "/opt/lingedge/server/current/") {
		t.Fatalf("ExecStart=%q", exec)
	}
	fi, err := os.Stat(filepath.Join(w.Root, exec)) // 经 current 符号链接解析
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("单元的 ExecStart %s 在安装后的 release 里不存在或不可执行: %v %v", exec, fi, err)
	}
	// 与规格声明的可执行文件一致：payload/<rel> ⇒ releases/<v>/<rel>。
	want := "/opt/lingedge/server/current/" + strings.TrimPrefix(installer.ServerExecutable, "payload/")
	if exec != want {
		t.Errorf("ExecStart=%q，规格的 Executables 推出 %q", exec, want)
	}
}
