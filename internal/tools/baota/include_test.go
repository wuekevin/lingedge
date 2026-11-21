package baota

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// incTree 是一个临时根目录；w 在其下按逻辑路径写文件（自动建目录）。
type incTree struct {
	t    *testing.T
	root string
}

func newIncTree(t *testing.T) *incTree { return &incTree{t: t, root: t.TempDir()} }

func (x *incTree) w(logical, content string) {
	x.t.Helper()
	p := filepath.Join(x.root, filepath.FromSlash(logical))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		x.t.Fatal(err)
	}
}

func (x *incTree) link(logical, target string) {
	x.t.Helper()
	p := filepath.Join(x.root, filepath.FromSlash(logical))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		x.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		x.t.Fatal(err)
	}
}

func (x *incTree) inc(o IncludeOptions) *Includer { return NewIncluder(OSFS{Root: x.root}, o) }

func srv(name string, port int) string {
	return fmt.Sprintf("server { listen %d; server_name %s; }\n", port, name)
}

func namesOf(ss []Site) []string {
	var n []string
	for _, s := range ss {
		n = append(n, strings.Join(s.ServerNames, ","))
	}
	sort.Strings(n)
	return n
}

func warnHas(in *Includer, sub string) bool {
	for _, w := range in.Warnings() {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func eq(a []string, b ...string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

const btConf = "/www/server/nginx/conf/nginx.conf"

func TestIncludeGlobAbsoluteAndRelativeToPrefix(t *testing.T) {
	x := newIncTree(t)
	x.w(btConf, "http {\n include mime.types;\n include /www/server/panel/vhost/nginx/*.conf;\n include extra/*.conf;\n}\n")
	x.w("/www/server/nginx/conf/mime.types", "types { text/html html; }\n")
	x.w("/www/server/panel/vhost/nginx/a.conf", srv("a.com", 80))
	x.w("/www/server/panel/vhost/nginx/b.conf", srv("b.com", 80))
	x.w("/www/server/panel/vhost/nginx/c.txt", srv("notconf.com", 80)) // 不匹配 *.conf
	x.w("/www/server/panel/vhost/nginx/.hidden.conf", srv("hidden.com", 80))
	// 相对路径相对 nginx 前缀（主配置所在目录），而不是相对包含者所在目录
	x.w("/www/server/nginx/conf/extra/p.conf", "include sub/q.conf;\n")
	x.w("/www/server/nginx/conf/sub/q.conf", srv("viaprefix.com", 8080))
	x.w("/www/server/nginx/conf/extra/sub/q.conf", srv("WRONG-relative-to-includer.com", 8081))
	in := x.inc(IncludeOptions{})
	sites, err := in.ParseConf(btConf, "baota")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(namesOf(sites), "a.com", "b.com", "viaprefix.com") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	for _, s := range sites {
		if s.ServerNames[0] == "a.com" && s.File != "/www/server/panel/vhost/nginx/a.conf" {
			t.Errorf("Site.File 应是 server 块所在文件，得到 %s", s.File)
		}
	}
}

func TestIncludeInsideServerBlockContributesDirectives(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "http { include conf.d/*.conf; }\n")
	x.w("/etc/nginx/conf.d/site.conf", "server { listen 80; include snippets/ssl.conf; include snippets/names.conf; }\n")
	x.w("/etc/nginx/snippets/ssl.conf", "listen 443 ssl; ssl_certificate /etc/nginx/x.crt;\n")
	x.w("/etc/nginx/snippets/names.conf", "server_name s.example.com;\n")
	in := x.inc(IncludeOptions{})
	sites, err := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if err != nil || len(sites) != 1 {
		t.Fatalf("%v %v", sites, err)
	}
	s := sites[0]
	if !s.SSL || len(s.Listens) != 2 || !eq(s.ServerNames, "s.example.com") || s.File != "/etc/nginx/conf.d/site.conf" {
		t.Fatalf("%+v", s)
	}
}

func TestIncludeSharedFragmentIsNotACycle(t *testing.T) {
	// 宝塔每个站点都 include enable-php-74.conf：重复 include 同一文件是正常的
	x := newIncTree(t)
	x.w(btConf, "http { include /www/server/panel/vhost/nginx/*.conf; }\n")
	x.w("/www/server/nginx/conf/enable-php.conf", "index index.php;\n")
	for i := 0; i < 5; i++ {
		x.w(fmt.Sprintf("/www/server/panel/vhost/nginx/s%d.conf", i), fmt.Sprintf("server { listen 80; server_name s%d.com; include enable-php.conf; }\n", i))
	}
	in := x.inc(IncludeOptions{})
	sites, _ := in.ParseConf(btConf, "baota")
	if len(sites) != 5 || warnHas(in, "循环") {
		t.Fatalf("sites=%d warnings=%v", len(sites), in.Warnings())
	}
	if files, _ := in.Stats(); files > 7 { // nginx.conf + 5 vhost + 1 共享片段（缓存，只读一次）
		t.Errorf("共享片段不应被重复读取，读取了 %d 个文件", files)
	}
}

func TestIncludeCycles(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "http { include a.conf; }\n")
	x.w("/etc/nginx/a.conf", srv("a.com", 80)+"include b.conf;\n")
	x.w("/etc/nginx/b.conf", srv("b.com", 81)+"include a.conf;\ninclude b.conf;\n") // a→b→a，且 b 自包含
	in := x.inc(IncludeOptions{})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if !eq(namesOf(sites), "a.com", "b.com") || !warnHas(in, "循环") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	// 自己包含自己的主配置（经 glob 包含自身所在目录）
	x.w("/etc/nginx/conf.d/self.conf", "include /etc/nginx/conf.d/*.conf;\n"+srv("self.com", 82))
	in = x.inc(IncludeOptions{})
	sites, _ = in.ParseConf("/etc/nginx/conf.d/self.conf", "system")
	if len(sites) != 1 || !warnHas(in, "循环") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	// 经符号链接绕回自己也算环（按真实路径判定）
	x.link("/etc/nginx/alias.conf", "/etc/nginx/c.conf")
	x.w("/etc/nginx/c.conf", "include alias.conf;\n"+srv("c.com", 83))
	in = x.inc(IncludeOptions{})
	sites, _ = in.ParseConf("/etc/nginx/c.conf", "system")
	if len(sites) != 1 || !warnHas(in, "循环") {
		t.Fatalf("符号链接环: sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
}

func TestIncludeDepthLimit(t *testing.T) {
	x := newIncTree(t)
	// 链：n0 → n1 → … → n30，每个文件一个 server
	for i := 0; i < 30; i++ {
		x.w(fmt.Sprintf("/etc/nginx/n%d.conf", i), srv(fmt.Sprintf("n%d.com", i), 80)+fmt.Sprintf("include n%d.conf;\n", i+1))
	}
	x.w("/etc/nginx/n30.conf", srv("n30.com", 80))
	in := x.inc(IncludeOptions{MaxDepth: 5})
	sites, _ := in.ParseConf("/etc/nginx/n0.conf", "system")
	if len(sites) != 6 || !warnHas(in, "深度") { // n0（根，深度 0）+ 5 层被包含
		t.Fatalf("sites=%d warnings=%v", len(sites), in.Warnings())
	}
	// 默认深度 16
	in = x.inc(IncludeOptions{})
	sites, _ = in.ParseConf("/etc/nginx/n0.conf", "system")
	if len(sites) != 17 {
		t.Fatalf("默认深度应展开 16 层，得到 %d 个站点", len(sites))
	}
}

func TestIncludeFileCountAndByteAndGlobLimits(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /etc/nginx/conf.d/*.conf;\n")
	for i := 0; i < 50; i++ {
		x.w(fmt.Sprintf("/etc/nginx/conf.d/f%02d.conf", i), srv(fmt.Sprintf("f%02d.com", i), 80))
	}
	in := x.inc(IncludeOptions{MaxFiles: 10})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if len(sites) != 9 || !warnHas(in, "文件数超过上限") { // 主配置占 1 个文件额度
		t.Fatalf("文件数上限: sites=%d warnings=%v", len(sites), in.Warnings())
	}
	if files, _ := in.Stats(); files > 10 {
		t.Fatalf("读取了 %d 个文件，超过上限 10", files)
	}

	in = x.inc(IncludeOptions{MaxGlob: 7})
	sites, _ = in.ParseConf("/etc/nginx/nginx.conf", "system")
	if len(sites) != 7 || !warnHas(in, "匹配过多") {
		t.Fatalf("glob 上限: sites=%d warnings=%v", len(sites), in.Warnings())
	}

	in = x.inc(IncludeOptions{MaxBytes: 200})
	sites, _ = in.ParseConf("/etc/nginx/nginx.conf", "system")
	if _, b := in.Stats(); len(sites) >= 50 || !warnHas(in, "字节数超过上限") || b > 200+int64(len(srv("f00.com", 80))) {
		t.Fatalf("字节上限: sites=%d bytes=%d warnings=%v", len(sites), b, in.Warnings())
	}
}

func TestIncludeNodeLimitStopsExponentialExpansion(t *testing.T) {
	// 每层把下一层 include 两次（DAG，不是环）：展开规模 2^n，必须被节点上限截断
	x := newIncTree(t)
	for i := 0; i < 15; i++ {
		x.w(fmt.Sprintf("/etc/nginx/l%d.conf", i), fmt.Sprintf("include l%d.conf; include l%d.conf; x 1;\n", i+1, i+1))
	}
	x.w("/etc/nginx/l15.conf", srv("leaf.com", 80))
	in := x.inc(IncludeOptions{MaxNodes: 2000})
	sites, _ := in.ParseConf("/etc/nginx/l0.conf", "system")
	if !warnHas(in, "指令数超过上限") || len(sites) > maxServers {
		t.Fatalf("sites=%d warnings=%v", len(sites), in.Warnings())
	}
}

func TestIncludeOutsideWhitelistIsRefused(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /srv/other.conf;\ninclude /etc/nginx/../srv/other.conf;\ninclude /etc/*/x.conf;\ninclude /etc/nginx-evil/x.conf;\n")
	x.w("/srv/other.conf", srv("leak.com", 80))
	x.w("/etc/nginx-evil/x.conf", srv("evil-prefix.com", 80)) // 前缀相同但不是子目录
	in := x.inc(IncludeOptions{})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if len(sites) != 0 || !warnHas(in, "白名单") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	// 自定义白名单根（经 FS 抽象可测）
	x.w("/data/conf/nginx.conf", "include /data/conf/v/*.conf;\n")
	x.w("/data/conf/v/a.conf", srv("custom.com", 80))
	sites, _ = x.inc(IncludeOptions{Roots: []string{"/data/conf"}}).ParseConf("/data/conf/nginx.conf", "system")
	if !eq(namesOf(sites), "custom.com") {
		t.Fatalf("自定义根: %v", namesOf(sites))
	}
	// 主配置本身不在白名单内：拒绝读取
	if _, err := x.inc(IncludeOptions{}).ParseConf("/data/conf/nginx.conf", "system"); err == nil {
		t.Fatal("白名单外的主配置应被拒绝")
	}
	// 根 "/" 无效（等于没有白名单）
	if sites, err := x.inc(IncludeOptions{Roots: []string{"/"}}).ParseConf("/srv/other.conf", "system"); err == nil || len(sites) != 0 {
		t.Fatalf("根目录 / 不应被当作白名单根: %v %v", sites, err)
	}
}

func TestIncludeSymlinkEscapeIsRefused(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /etc/nginx/conf.d/*.conf;\ninclude /etc/nginx/linkdir/*.conf;\ninclude /etc/nginx/ok.conf;\ninclude /etc/nginx/chain.conf;\n")
	x.w("/srv/evil.conf", srv("leak.com", 80))
	x.w("/srv/dir/in.conf", srv("leakdir.com", 80))
	x.link("/etc/nginx/conf.d/evil.conf", "/srv/evil.conf")            // 文件级越界
	x.link("/etc/nginx/linkdir", "/srv/dir")                           // 目录级越界（glob 与逐级解析都要拦）
	x.link("/etc/nginx/chain.conf", "/etc/nginx/hop.conf")             // 两跳：第二跳越界
	x.link("/etc/nginx/hop.conf", "/srv/evil.conf")                    //
	x.w("/etc/nginx/real.conf", srv("inside.com", 80))                 //
	x.link("/etc/nginx/ok.conf", "real.conf")                          // 相对链接指向白名单内：允许
	x.link("/etc/nginx/conf.d/rel-escape.conf", "../../srv/evil.conf") // 相对链接越界（..）
	in := x.inc(IncludeOptions{})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if !eq(namesOf(sites), "inside.com") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	if !warnHas(in, "越出") {
		t.Fatalf("应有越界警告: %v", in.Warnings())
	}
	// 符号链接环：层数上限，不死循环
	x.link("/etc/nginx/l1", "/etc/nginx/l2")
	x.link("/etc/nginx/l2", "/etc/nginx/l1")
	x.w("/etc/nginx/m.conf", "include /etc/nginx/l1;\n")
	in = x.inc(IncludeOptions{})
	if sites, _ := in.ParseConf("/etc/nginx/m.conf", "system"); len(sites) != 0 || len(in.Warnings()) == 0 {
		t.Fatalf("符号链接环: %v %v", sites, in.Warnings())
	}
}

func TestIncludeNeverReadsGuardedFiles(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /etc/nginx/conf.d/*.conf;\ninclude /etc/nginx/my.cnf;\ninclude /etc/nginx/secret.pem;\ninclude /etc/nginx/redis.conf;\n")
	x.w("/etc/nginx/conf.d/ok.conf", srv("ok.com", 80))
	x.w("/etc/nginx/conf.d/mysql_root.pl", srv("canary1.com", 80))
	x.w("/etc/nginx/my.cnf", srv("canary2.com", 80))
	x.w("/etc/nginx/secret.pem", srv("canary3.com", 80))
	x.w("/etc/nginx/redis.conf", srv("canary4.com", 80))
	x.w("/etc/nginx/passwords/a.conf", srv("canary5.com", 80))
	x.link("/etc/nginx/innocent.conf", "/etc/nginx/my.cnf") // 无害名字的链接指向敏感文件
	x.w("/etc/nginx/n2.conf", "include /etc/nginx/innocent.conf;\ninclude /etc/nginx/passwords/*.conf;\ninclude /etc/nginx/conf.d/ok.conf;\n")
	rec := &recordingFS{inner: OSFS{Root: x.root}}
	in := NewIncluder(rec, IncludeOptions{})
	s1, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	s2, _ := in.ParseConf("/etc/nginx/n2.conf", "system")
	for _, s := range append(s1, s2...) {
		if strings.HasPrefix(s.ServerNames[0], "canary") {
			t.Fatalf("读取了被 readGuard 禁止的文件: %+v", s)
		}
	}
	for _, r := range rec.reads {
		if readGuard(r) != nil {
			t.Fatalf("对被禁止的路径发起了读取: %s", r)
		}
	}
	if !warnHas(in, "敏感") {
		t.Fatalf("应有敏感文件警告: %v", in.Warnings())
	}
}

func TestIncludeMalformedAndMissingNeverPanic(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", strings.Join([]string{
		"include;", "include a b;", "include $host.conf;", "include /etc/nginx/missing.conf;", "include /etc/nginx/nodir/*.conf;",
		"include /etc/nginx/[.conf;", "include /etc/nginx/conf.d;", "include \"/etc/nginx/bad.conf\";", "include rel.conf;", "include",
	}, "\n"))
	x.w("/etc/nginx/conf.d/x.conf", "")
	x.w("/etc/nginx/bad.conf", "server { listen 80; server_name \"unterminated; }")
	in := x.inc(IncludeOptions{})
	sites, err := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if err == nil {
		t.Error("主配置末尾缺少 ; 应报告 error")
	}
	_ = sites
	for _, want := range []string{"参数个数", "变量", "不存在", "不是普通文件", "解析不完整", "没有 nginx 前缀"} {
		if want == "没有 nginx 前缀" {
			continue // ParseConf 已设置前缀，rel.conf 走「不存在」
		}
		if !warnHas(in, want) {
			t.Errorf("缺少警告 %q: %v", want, in.Warnings())
		}
	}
	// 没有前缀时的相对 include
	in2 := x.inc(IncludeOptions{})
	_, _ = in2.ParseData("/etc/nginx/zz.conf", "system", []byte("include rel.conf;"))
	if !warnHas(in2, "没有 nginx 前缀") {
		t.Errorf("无前缀的相对 include 应告警: %v", in2.Warnings())
	}
}

func TestIncludeWarningsAreBounded(t *testing.T) {
	x := newIncTree(t)
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "include /etc/nginx/missing%d.conf;\n", i)
	}
	x.w("/etc/nginx/nginx.conf", b.String())
	in := x.inc(IncludeOptions{})
	_, _ = in.ParseConf("/etc/nginx/nginx.conf", "system")
	if len(in.Warnings()) > maxIncludeWarnings+1 {
		t.Fatalf("警告数 %d 未受限", len(in.Warnings()))
	}
}

// 端到端：Probe 跟随 include，补全目录扫描没有覆盖的站点，且不重复计入目录扫描已得到的站点。
func TestProbeFollowsIncludesAndDedupes(t *testing.T) {
	root := copyTree(t, "baota/bt-full")
	w := func(logical, content string) {
		p := filepath.Join(root, filepath.FromSlash(logical))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := Probe(context.Background(), Options{FS: OSFS{Root: root}})
	w("/www/server/nginx/conf/nginx.conf", "http {\n include mime.types;\n include /www/server/panel/vhost/nginx/*.conf;\n include /etc/nginx/extra/*.conf;\n}\n")
	w("/etc/nginx/extra/only-by-include.conf", srv("only-by-include.example.com", 8088))
	after := Probe(context.Background(), Options{FS: OSFS{Root: root}})
	if len(after.Sites) != len(before.Sites)+1 {
		t.Fatalf("应只多出 include 才能发现的 1 个站点（目录扫描的站点不得重复）：before=%d after=%d notes=%v", len(before.Sites), len(after.Sites), after.Notes)
	}
	uniq := map[string]bool{}
	for _, s := range after.Sites {
		k := s.File + "|" + strings.Join(s.ServerNames, ",")
		if uniq[k] {
			t.Fatalf("站点重复（目录扫描与 include 跟随各算一次）: %s", k)
		}
		uniq[k] = true
	}
	found := false
	for _, s := range after.Sites {
		if s.File == "/etc/nginx/extra/only-by-include.conf" && s.Listens[0].Port == 8088 {
			found = true
		}
	}
	if !found {
		t.Fatalf("未发现经 include 引入的站点: %+v", after.Sites)
	}
	if err := after.Validate(); err != nil {
		t.Fatal(err)
	}
}

// 不写入：跟随 include 不改变文件树。
func TestIncludeIsReadOnly(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include conf.d/*.conf;\n")
	x.w("/etc/nginx/conf.d/a.conf", srv("a.com", 80))
	before := snapshot(t, x.root)
	_, _ = x.inc(IncludeOptions{}).ParseConf("/etc/nginx/nginx.conf", "system")
	if after := snapshot(t, x.root); after != before {
		t.Fatalf("文件树被改变:\n%s\n---\n%s", before, after)
	}
}

func snapshot(t *testing.T, root string) string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		b, _ := os.ReadFile(p)
		out = append(out, fmt.Sprintf("%s %v %d %s", p, fi.Mode(), fi.ModTime().UnixNano(), b))
		return nil
	})
	return strings.Join(out, "\n")
}

func FuzzIncludeConf(f *testing.F) {
	for _, s := range []string{
		"include /etc/nginx/conf.d/*.conf;",
		"include a.conf; include a.conf;",
		"server { include a.conf; listen 80; }",
		"include /etc/nginx/../srv/x;",
		"include /etc/*/x.conf; include [;",
		"include \"unterminated",
		"http { include conf.d/*.conf; include loop.conf; }",
		"include $x; include ; include a b c;",
	} {
		f.Add([]byte(s), []byte(srv("z.com", 80)+"include loop.conf;include a.conf;"))
	}
	root := f.TempDir()
	write := func(l, c string) {
		p := filepath.Join(root, l)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(c), 0o644)
	}
	write("etc/nginx/conf.d/x.conf", srv("x.com", 80))
	write("etc/nginx/a.conf", srv("a.com", 81)+"include conf.d/*.conf;")
	_ = os.Symlink("/etc/nginx/loop.conf", filepath.Join(root, "etc/nginx/loop2.conf"))
	_ = os.Symlink("/srv/out", filepath.Join(root, "etc/nginx/out.conf"))
	f.Fuzz(func(t *testing.T, main, loop []byte) {
		write("etc/nginx/loop.conf", string(loop))
		in := NewIncluder(OSFS{Root: root}, IncludeOptions{MaxFiles: 40, MaxBytes: 1 << 20, MaxNodes: 20000})
		in.SetPrefix("/etc/nginx")
		sites, _ := in.ParseData("/etc/nginx/nginx.conf", "system", main)
		if len(sites) > maxServers {
			t.Fatal("站点数超限")
		}
		if files, b := in.Stats(); files > 40 || b > 1<<20+maxConfBytes {
			t.Fatalf("预算被突破: files=%d bytes=%d", files, b)
		}
		if len(in.Warnings()) > maxIncludeWarnings+1 {
			t.Fatal("警告数未受限")
		}
	})
}

// 符号链接中途越界再绕回白名单内也拒绝（每一跳的目标都必须在白名单根内，而不只是最终文件）。
func TestIncludeSymlinkHopOutsideAndBackIsRefused(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /etc/nginx/viaout.conf;\ninclude /etc/nginx/my.cnf;\n")
	x.w("/etc/nginx/real.conf", srv("real.com", 80))
	x.link("/srv/bounce", "/etc/nginx/real.conf")       // 白名单外的跳点，指回白名单内
	x.link("/etc/nginx/viaout.conf", "/srv/bounce")     // 第一跳就越界
	x.link("/etc/nginx/my.cnf", "/etc/nginx/real.conf") // 名字被 readGuard 禁止、目标无害：按 include 路径的名字也拒绝
	rec := &recordingFS{inner: OSFS{Root: x.root}}
	in := NewIncluder(rec, IncludeOptions{})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if len(sites) != 0 || !warnHas(in, "越出") || !warnHas(in, "敏感") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
}

// 列目录也不越界：通配展开不得对白名单根之外（含经符号链接指向外部的目录）调用 ReadDir。
func TestIncludeGlobNeverListsOutsideWhitelist(t *testing.T) {
	x := newIncTree(t)
	x.w("/etc/nginx/nginx.conf", "include /srv/*/x.conf;\ninclude /etc/nginx/linkdir/*.conf;\ninclude /etc/nginx/ok/*.conf;\n")
	x.w("/srv/d/x.conf", srv("leak.com", 80))
	x.w("/srv/dir/in.conf", srv("leakdir.com", 80))
	x.link("/etc/nginx/linkdir", "/srv/dir")
	x.w("/etc/nginx/ok/a.conf", srv("ok.com", 80))
	rec := &recordingFS{inner: OSFS{Root: x.root}}
	in := NewIncluder(rec, IncludeOptions{})
	sites, _ := in.ParseConf("/etc/nginx/nginx.conf", "system")
	if !eq(namesOf(sites), "ok.com") {
		t.Fatalf("sites=%v warnings=%v", namesOf(sites), in.Warnings())
	}
	for _, d := range rec.lists {
		// 列出的必须是白名单内的真实目录：符号链接目录 /etc/nginx/linkdir（指向 /srv/dir）本身就不该被列出
		if !in.allowed(d) || d == "/etc/nginx/linkdir" {
			t.Fatalf("对白名单之外（或指向外部的符号链接）的目录调用了 ReadDir：%s", d)
		}
	}
}
