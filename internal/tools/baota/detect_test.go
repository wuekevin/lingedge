package baota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetectionRequiresTwoEvidenceKinds(t *testing.T) {
	cases := []struct {
		dir        string
		detected   bool
		confidence string
		kinds      []string
		port       int
		note       string
	}{
		{"bt-full", true, "high", []string{"bt_command", "bt_command", "panel_dir", "port_file"}, 8888, ""},
		{"bt-two-evidence", true, "medium", []string{"panel_dir", "port_file"}, 34567, ""},
		{"bt-openresty", true, "medium", []string{"bt_command", "panel_dir", "port_file"}, 8888, ""}, // 三类：panel_dir + bt + port（此处 bt 为 /usr/bin/bt），仍判 high
		{"bt-single-evidence", false, "", []string{"panel_dir"}, 0, "只有一类宝塔证据"},
		{"bt-cmd-only", false, "", []string{"bt_command"}, 0, "只有一类宝塔证据"},
		{"bt-bad-port", false, "", []string{"panel_dir"}, 0, "不是合法端口"},
		{"bt-system-only", false, "", nil, 0, ""},
		{"empty", false, "", nil, 0, ""},
	}
	for _, c := range cases {
		r := probeCase(t, c.dir)
		wantConf := c.confidence
		if c.dir == "bt-openresty" {
			wantConf = "high"
		}
		if r.Detected != c.detected || r.Confidence != wantConf {
			t.Errorf("%s: detected=%v confidence=%q，期望 %v/%q（evidence=%v）", c.dir, r.Detected, r.Confidence, c.detected, wantConf, r.Evidence)
		}
		if c.dir != "bt-openresty" && !reflect.DeepEqual(evidenceKinds(r), orNil(c.kinds)) {
			t.Errorf("%s: 证据类别 %v，期望 %v", c.dir, evidenceKinds(r), c.kinds)
		}
		if r.PanelPort != c.port {
			t.Errorf("%s: panel_port=%d，期望 %d", c.dir, r.PanelPort, c.port)
		}
		if c.note != "" && !hasNote(r, c.note) {
			t.Errorf("%s: 缺少说明 %q，notes=%v", c.dir, c.note, r.Notes)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: 报告不合法: %v", c.dir, err)
		}
	}
}

func orNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestFullBaotaReport(t *testing.T) {
	r := probeCase(t, "bt-full")
	if r.Version != "7.9.10" || r.VersionSource != "/www/server/panel/class/common.py" {
		t.Errorf("版本=%q 来源=%q", r.Version, r.VersionSource)
	}
	if r.ProbedAt != "2026-11-21T01:02:03.000Z" {
		t.Errorf("probed_at=%q", r.ProbedAt)
	}
	// Nginx（宝塔）：版本取自 version.pl（无 Exec）
	if len(r.WebServers) != 1 || r.WebServers[0].Variant != "baota" || r.WebServers[0].Kind != "nginx" || r.WebServers[0].Version != "1.24.0" ||
		r.WebServers[0].VhostDir != "/www/server/panel/vhost/nginx" || r.WebServers[0].Binary != "/www/server/nginx/sbin/nginx" {
		t.Errorf("web=%+v", r.WebServers)
	}
	// MySQL/Redis：仅识别 + 版本（version.pl）
	wantDB := []DBInstall{
		{Variant: "baota", Kind: "mysql", Path: "/www/server/mysql/bin/mysqld", DataDir: "/www/server/data", Version: "5.7.44"},
		{Variant: "baota", Kind: "redis", Path: "/www/server/redis/src/redis-server", Version: "7.0.15"},
	}
	if !reflect.DeepEqual(r.Databases, wantDB) {
		t.Errorf("db=%+v", r.Databases)
	}
	wantPHP := []PHP{{"7.4", "/www/server/php/74"}, {"8.0", "/www/server/php/80"}, {"8.3", "/www/server/php/83"}}
	if !reflect.DeepEqual(r.PHP, wantPHP) {
		t.Errorf("php=%+v", r.PHP)
	}
	if !reflect.DeepEqual(r.WAF, []WAF{{"btwaf", "/www/server/btwaf"}, {"btwaf-plugin", "/www/server/panel/plugin/btwaf"}, {"firewall-plugin", "/www/server/panel/plugin/firewall"}}) {
		t.Errorf("waf=%+v", r.WAF)
	}
	if !reflect.DeepEqual(r.Plugins, []string{"btwaf", "firewall", "mysql"}) { // "bad name" 含空格被忽略
		t.Errorf("plugins=%v", r.Plugins)
	}
	if !reflect.DeepEqual(r.Firewalls, []Firewall{{Kind: "ufw", Active: "yes", Source: "/etc/ufw/ufw.conf"}}) {
		t.Errorf("firewalls=%+v", r.Firewalls)
	}
	// 站点：4 个 conf（notes.txt 不是 .conf）；按文件名排序
	var files []string
	for _, s := range r.Sites {
		files = append(files, filepath.Base(s.File))
	}
	if !reflect.DeepEqual(files, []string{"0.default.conf", "broken.conf", "example.com.conf", "proxy.example.org.conf"}) {
		t.Fatalf("站点文件=%v", files)
	}
	byFile := map[string]Site{}
	for _, s := range r.Sites {
		byFile[filepath.Base(s.File)] = s
	}
	ex := byFile["example.com.conf"]
	if !reflect.DeepEqual(ex.ServerNames, []string{"example.com", "www.example.com"}) || !ex.SSL || ex.Source != "baota" || ex.Malformed {
		t.Errorf("example: %+v", ex)
	}
	wantL := []Listen{{Addr: "*", Port: 80}, {Addr: "*", Port: 443, SSL: true}, {Addr: "::", Port: 80}, {Addr: "::", Port: 443, SSL: true}}
	if !reflect.DeepEqual(ex.Listens, wantL) {
		t.Errorf("example listens=%+v", ex.Listens)
	}
	df := byFile["0.default.conf"]
	if len(df.Listens) != 1 || !df.Listens[0].Default || !reflect.DeepEqual(df.ServerNames, []string{"_"}) {
		t.Errorf("default: %+v", df)
	}
	px := byFile["proxy.example.org.conf"]
	if !reflect.DeepEqual(px.ServerNames, []string{"api.example.org", "*.wild.example.org", `~^(?<sub>.+)\.re\.example\.org$`}) ||
		!reflect.DeepEqual(px.Listens, []Listen{{Addr: "*", Port: 8081}, {Addr: "127.0.0.1", Port: 8082}}) {
		t.Errorf("proxy: %+v", px)
	}
	if !byFile["broken.conf"].Malformed || !hasNote(r, "broken.conf") {
		t.Errorf("畸形配置应标记 malformed 并记入 notes: %+v / %v", byFile["broken.conf"], r.Notes)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSystemOnlyEnvironment(t *testing.T) {
	r := probeCase(t, "bt-system-only")
	if r.Detected || r.PanelPort != 0 || r.Version != "" {
		t.Fatalf("非宝塔环境不应判定宝塔: %+v", r)
	}
	kinds := map[string]string{}
	for _, w := range r.WebServers {
		kinds[w.Variant+"/"+w.Kind] = w.Binary
	}
	if kinds["system/nginx"] != "/usr/sbin/nginx" || kinds["system/openresty"] != "/usr/local/openresty/nginx/sbin/openresty" || len(kinds) != 2 {
		t.Errorf("web=%v", kinds)
	}
	var dbs []string
	for _, d := range r.Databases {
		dbs = append(dbs, d.Variant+"/"+d.Kind)
	}
	if !reflect.DeepEqual(dbs, []string{"system/mysql", "system/redis"}) {
		t.Errorf("db=%v", dbs)
	}
	names := map[string][]string{}
	for _, s := range r.Sites {
		names[filepath.Base(s.File)] = s.ServerNames
		if s.Source != "system" {
			t.Errorf("source=%s", s.Source)
		}
	}
	if !reflect.DeepEqual(names["blog.conf"], []string{"blog.example.net"}) || !reflect.DeepEqual(names["default"], []string{"_"}) || len(names) != 2 {
		t.Errorf("sites=%v（sites-enabled 的符号链接应读取目标；未启用的 sites-available/shop 不应出现）", names)
	}
	if !reflect.DeepEqual(r.Firewalls, []Firewall{{Kind: "firewalld", Active: "unknown", Source: "/etc/firewalld/firewalld.conf"}}) {
		t.Errorf("fw=%+v", r.Firewalls)
	}
}

type stubExec struct{ flavor, ver string }

func (s stubExec) Version(_ context.Context, kind, exe string) (string, string, error) {
	if kind == "nginx" {
		return s.ver, s.flavor, nil
	}
	return "", "", errors.New("无版本")
}

// Exec 只是 Probe 取版本的接口；这里只验证「以 -v 输出为准的 Kind 切换」与「Exec 失败回退到文件证据/记入 notes」，真实 -v 输出的解析在 probe 包用真实 nginx 测试。
func TestExecFlavorAndFailureFallback(t *testing.T) {
	root := OSFS{Root: filepath.Join(fixtureRoot, "baota", "bt-full")}
	r := Probe(context.Background(), Options{FS: root, Exec: stubExec{flavor: "openresty", ver: "1.21.4.1"}})
	if r.WebServers[0].Kind != "openresty" || r.WebServers[0].Version != "1.21.4.1" {
		t.Errorf("宝塔 /www/server/nginx 是 OpenResty 时应以 -v 为准: %+v", r.WebServers[0])
	}
	// MySQL 的 Exec 失败 → 回退 version.pl 并记 notes
	if r.Databases[0].Version != "5.7.44" || !hasNote(r, "无法取得 /www/server/mysql/bin/mysqld 的版本") {
		t.Errorf("db=%+v notes=%v", r.Databases[0], r.Notes)
	}
}

// 不打开任何口令类文件：夹具副本里放入「金丝雀」口令文件，用记录式 FS 包装真实文件系统，探测后断言：
// 没有任何读取指向这些文件、金丝雀串不出现在序列化结果里；并且直接读取它们被 readGuard 拒绝。
func TestNeverOpensPasswordFiles(t *testing.T) {
	root := copyTree(t, "baota/bt-full")
	canary := "CANARY-ROOT-PW-9f8e7d6c"
	secrets := map[string]string{
		"/www/server/panel/data/default.pl":          canary + "-panel",
		"/www/server/panel/data/default.db":          "SQLite format 3\x00 mysql_root=" + canary,
		"/www/server/panel/data/mysql_root.pl":       canary,
		"/www/server/redis/redis.conf":               "requirepass " + canary,
		"/etc/mysql/my.cnf":                          "[client]\npass" + "word=" + canary + "\n",
		"/www/server/panel/vhost/cert/x/privkey.pem": canary,
		"/www/server/data/secret_note.txt":           canary,
		"/www/server/panel/data/other.db":            canary, // 仅被后缀规则覆盖
		"/www/server/data/x/root_pass_hint":          canary, // 仅被子串规则覆盖
	}
	for p, c := range secrets {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordingFS{inner: OSFS{Root: root}}
	r := Probe(context.Background(), Options{FS: rec})
	for _, p := range rec.reads {
		if _, isSecret := secrets[p]; isSecret || readGuard(p) != nil {
			t.Errorf("探测打开了敏感文件 %s", p)
		}
	}
	if strings.Contains(toJSON(t, r), canary) {
		t.Fatal("序列化结果含金丝雀口令")
	}
	if len(rec.reads) == 0 {
		t.Fatal("记录式 FS 没有记录到任何读取：测试本身失效")
	}
	for p := range secrets {
		if _, err := (OSFS{Root: root}).ReadFile(p, 1<<10); !errors.Is(err, ErrForbidden) {
			t.Errorf("readGuard 应拒绝 %s，得到 %v", p, err)
		}
	}
	// 非敏感文件仍可读（防止 guard 误伤全部）
	if _, err := (OSFS{Root: root}).ReadFile("/www/server/panel/data/port.pl", 100); err != nil {
		t.Errorf("port.pl 应可读: %v", err)
	}
}

// 探测不写入任何文件：前后对整棵树做快照（路径 + 大小 + 修改时间 + 内容）对比。
func TestProbeIsReadOnly(t *testing.T) {
	root := copyTree(t, "baota/bt-full")
	snap := func() map[string]string {
		m := map[string]string{}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, _ := d.Info()
			b := []byte{}
			if !d.IsDir() {
				b, _ = os.ReadFile(p)
			}
			m[p] = fi.ModTime().Format(time.RFC3339Nano) + "|" + fi.Mode().String() + "|" + string(b)
			return nil
		})
		return m
	}
	before := snap()
	_ = Probe(context.Background(), Options{FS: OSFS{Root: root}})
	if !reflect.DeepEqual(before, snap()) {
		t.Fatal("探测修改了文件树")
	}
}

// OSFS 不跟随符号链接：根内的符号链接指向根外的文件时不会被读取。
func TestOSFSDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "www/server/panel/data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "www/server/panel/data/port.pl")); err != nil {
		t.Fatal(err)
	}
	if b, err := (OSFS{Root: root}).ReadFile("/www/server/panel/data/port.pl", 100); err == nil {
		t.Fatalf("不应跟随符号链接读取: %q", b)
	}
	if _, err := (OSFS{Root: root}).ReadFile("../escape", 1); err == nil {
		t.Fatal("非规范路径应被拒绝")
	}
}

func TestReportValidateAndScrub(t *testing.T) {
	r := probeCase(t, "bt-full")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := func(name string, mut func(*Report)) {
		c := r
		c.Sites = append([]Site(nil), r.Sites...)
		c.Evidence = append([]Evidence(nil), r.Evidence...)
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: 应校验失败", name)
		}
	}
	bad("confidence 空但 detected", func(c *Report) { c.Confidence = "" })
	bad("端口越界", func(c *Report) { c.PanelPort = 70000 })
	bad("相对路径", func(c *Report) { c.Evidence[0].Path = "www/server" })
	bad("非规范路径", func(c *Report) { c.Evidence[0].Path = "/www/../etc" })
	bad("未知证据类别", func(c *Report) { c.Evidence[0].Kind = "x" })
	bad("站点来源", func(c *Report) { c.Sites[0].Source = "x" })
	bad("疑似密钥", func(c *Report) {
		c.Notes = append([]string(nil), c.Notes...)
		c.Notes = append(c.Notes, "password=hunter2hunter2")
	})
	// Scrub 后合法，且不丢其他字段
	c := r
	c.Notes = append([]string(nil), c.Notes...)
	c.Notes = append(c.Notes, "password=hunter2hunter2")
	c.Sites = append([]Site(nil), c.Sites...)
	c.Sites[0].ServerNames = []string{"a.example.com", "Bearer abcdefghijklmnopqrstuvwxyz0123456789"}
	s := c.Scrub()
	if err := s.Validate(); err != nil {
		t.Fatalf("Scrub 后应合法: %v", err)
	}
	if strings.Contains(toJSON(t, s), "hunter2hunter2") || s.Sites[0].ServerNames[0] != "a.example.com" || len(s.Evidence) != len(r.Evidence) {
		t.Errorf("Scrub 结果不对: %v", s.Sites[0].ServerNames)
	}
	// Scrub 不修改接收者的原切片
	if c.Sites[0].ServerNames[1] != "Bearer abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Error("Scrub 修改了原值")
	}
	// Scrub 幂等
	if !reflect.DeepEqual(s, s.Scrub()) {
		t.Error("Scrub 应幂等")
	}
}

// sites-enabled 里指向 /etc/nginx 之外的符号链接不被跟随（只读探测不读取范围外的文件）。
func TestVhostSymlinkOutsideNginxIsNotFollowed(t *testing.T) {
	root := copyTree(t, "baota/bt-system-only")
	// 目标在探测根之内（所以「若被跟随就一定读得到」），但不在 /etc/nginx 之下
	if err := os.MkdirAll(filepath.Join(root, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "srv/evil.conf"), []byte("server { listen 1234; server_name leaked.example.com; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/srv/evil.conf", filepath.Join(root, "etc/nginx/sites-enabled/evil")); err != nil {
		t.Fatal(err)
	}
	r := Probe(context.Background(), Options{FS: OSFS{Root: root}})
	for _, s := range r.Sites {
		for _, n := range s.ServerNames {
			if n == "leaked.example.com" {
				t.Fatal("跟随了 /etc/nginx 之外的符号链接")
			}
		}
	}
}
