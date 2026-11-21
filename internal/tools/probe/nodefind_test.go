package probe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
)

type nodeTree struct {
	t    *testing.T
	root string
	mark string // 若任何「node」脚本被执行，会写入此文件
}

func newNodeTree(t *testing.T) *nodeTree {
	return &nodeTree{t: t, root: t.TempDir(), mark: filepath.Join(t.TempDir(), "EXECUTED")}
}

func (n *nodeTree) path(l string) string { return filepath.Join(n.root, filepath.FromSlash(l)) }

// install 建一个 <prefix>/bin/node（若被执行会留下标记）与可选的 node_version.h。
func (n *nodeTree) install(prefix string, mode os.FileMode, header string) {
	n.t.Helper()
	bin := n.path(prefix + "/bin/node")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+n.mark+"\necho v0.0.0\n"), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.Chmod(bin, mode); err != nil { // 绕过 umask
		n.t.Fatal(err)
	}
	if header != "" {
		h := n.path(prefix + "/include/node/node_version.h")
		if err := os.MkdirAll(filepath.Dir(h), 0o755); err != nil {
			n.t.Fatal(err)
		}
		if err := os.WriteFile(h, []byte(header), 0o644); err != nil {
			n.t.Fatal(err)
		}
	}
}

func nodeHeader(maj, min, pat int) string {
	return fmt.Sprintf("#ifndef SRC_NODE_VERSION_H_\n#define NODE_MAJOR_VERSION %d\n#define NODE_MINOR_VERSION %d\n#define NODE_PATCH_VERSION %d\n#define NODE_VERSION_IS_RELEASE 1\n#endif\n", maj, min, pat)
}

func (n *nodeTree) assertNeverExecuted() {
	n.t.Helper()
	if _, err := os.Stat(n.mark); err == nil {
		n.t.Fatal("探测执行了非固定目录下的 node 二进制")
	}
}

func TestFindNodeInstallsLayouts(t *testing.T) {
	n := newNodeTree(t)
	home := "/home/dev"
	n.install(home+"/.nvm/versions/node/v20.11.0", 0o755, nodeHeader(20, 11, 0))
	n.install(home+"/.nvm/versions/node/v18.19.1", 0o755, nodeHeader(18, 19, 0)) // 头文件与目录名不一致：以头文件为准
	n.install(home+"/.nvm/versions/node/v16.20.2", 0o755, "")                    // 没有头文件：目录名
	n.install(home+"/.fnm/node-versions/v22.1.0/installation", 0o755, nodeHeader(22, 1, 0))
	n.install(home+"/.local/share/fnm/node-versions/v21.7.3/installation", 0o755, "")
	n.install(home+"/.volta/tools/image/node/20.9.0", 0o755, "")
	n.install(home+"/.asdf/installs/nodejs/19.9.0", 0o755, "")
	n.install(home+"/n/n/versions/node/17.9.1", 0o755, "")
	n.install("/usr/local/n/versions/node/20.12.2", 0o755, nodeHeader(20, 12, 2))
	n.install("/www/server/nodejs/v18.20.0", 0o755, "") // 宝塔 Node 项目管理器
	n.install("/www/server/nodejs/v14.21.3", 0o755, "")

	got := FindNodeInstalls(baota.OSFS{Root: n.root}, []string{home})
	var line []string
	for _, g := range got {
		line = append(line, g.Manager+":"+g.Version+":"+g.VersionSource)
	}
	want := []string{
		"fnm:v22.1.0:dirname+node_version.h", "fnm:v21.7.3:dirname", "n:v20.12.2:dirname+node_version.h", "nvm:v20.11.0:dirname+node_version.h", "volta:v20.9.0:dirname",
		"asdf:v19.9.0:dirname", "baota:v18.20.0:dirname", "nvm:v18.19.0:node_version.h", "n:v17.9.1:dirname", "nvm:v16.20.2:dirname", "baota:v14.21.3:dirname",
	}
	if strings.Join(line, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(line, "\n"), strings.Join(want, "\n"))
	}
	for _, g := range got {
		if !strings.HasSuffix(g.Bin, "/bin/node") || !strings.HasPrefix(g.Bin, g.Dir) {
			t.Errorf("路径: %+v", g)
		}
	}
	if got[0].Bin != home+"/.fnm/node-versions/v22.1.0/installation/bin/node" || got[len(got)-1].Bin != "/www/server/nodejs/v14.21.3/bin/node" {
		t.Errorf("bin 路径: %+v … %+v", got[0], got[len(got)-1])
	}
	n.assertNeverExecuted()
}

func TestFindNodeInstallsRejectsUnsafeAndNonVersions(t *testing.T) {
	n := newNodeTree(t)
	h := "/home/dev"
	n.install(h+"/.nvm/versions/node/v20.0.0", 0o755, "")
	n.install(h+"/.nvm/versions/node/v20.1.0", 0o777, "") // group/other 可写：与 safeexec 同标准，拒绝
	n.install(h+"/.nvm/versions/node/v20.2.0", 0o644, "") // 不可执行
	n.install(h+"/.nvm/versions/node/default", 0o755, "") // 不是版本目录名（别名）
	n.install(h+"/.nvm/versions/node/v20", 0o755, "")     // 不完整版本
	n.install(h+"/.nvm/versions/node/v20.3.0-rc.1", 0o755, "")
	n.install(h+"/.nvm/versions/node/v20.4.0;rm", 0o755, "")
	if err := os.MkdirAll(n.path(h+"/.nvm/versions/node/v20.5.0"), 0o755); err != nil { // 没有 bin/node
		t.Fatal(err)
	}
	// 符号链接的版本目录（指向别处）不跟随
	n.install("/opt/elsewhere/v99.0.0", 0o755, "")
	lnk := n.path(h + "/.nvm/versions/node/v99.0.0")
	rel, err := filepath.Rel(filepath.Dir(lnk), n.path("/opt/elsewhere/v99.0.0")) // 相对链接：在宿主机上真实可解析，「若被跟随就一定读得到」
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, lnk); err != nil {
		t.Fatal(err)
	}
	// bin/node 是符号链接：不接受（只接受普通文件）
	if err := os.MkdirAll(n.path(h+"/.nvm/versions/node/v20.6.0/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", n.path(h+"/.nvm/versions/node/v20.6.0/bin/node")); err != nil {
		t.Fatal(err)
	}
	// 头文件是垃圾：忽略，用目录名
	n.install(h+"/.nvm/versions/node/v20.7.0", 0o755, "#define NODE_MAJOR_VERSION x\n")
	got := FindNodeInstalls(baota.OSFS{Root: n.root}, []string{h})
	var vs []string
	for _, g := range got {
		vs = append(vs, g.Version)
	}
	if strings.Join(vs, ",") != "v20.7.0,v20.0.0" {
		t.Fatalf("只应识别 v20.7.0 与 v20.0.0，得到 %v", vs)
	}
	n.assertNeverExecuted()
	// 家目录非法 / 不存在：不 panic
	if got := FindNodeInstalls(baota.OSFS{Root: n.root}, []string{"relative", "/nonexistent", "/home/dev/../dev", ""}); len(got) != 0 {
		t.Errorf("%v", got)
	}
	// 不识别的目录与数量上限
	m := newNodeTree(t)
	for i := 0; i < maxNodeVersionsPerRoot+30; i++ {
		m.install(fmt.Sprintf("/www/server/nodejs/v18.%d.0", i), 0o755, "")
	}
	if got := FindNodeInstalls(baota.OSFS{Root: m.root}, nil); len(got) != maxNodeVersionsPerRoot {
		t.Errorf("每个根最多 %d 个，得到 %d", maxNodeVersionsPerRoot, len(got))
	}
}

func TestParseNodeVersionHeader(t *testing.T) {
	if v, ok := ParseNodeVersionHeader([]byte(nodeHeader(20, 11, 1))); !ok || v != "v20.11.1" {
		t.Fatalf("%q %v", v, ok)
	}
	for _, bad := range []string{"", "#define NODE_MAJOR_VERSION 20\n", "#define NODE_MAJOR_VERSION 20\n#define NODE_MINOR_VERSION 1\n", "garbage\x00\xff"} {
		if _, ok := ParseNodeVersionHeader([]byte(bad)); ok {
			t.Errorf("%q 应无法解析", bad)
		}
	}
}

func TestNodeHomesDerivation(t *testing.T) {
	n := newNodeTree(t)
	for _, d := range []string{"/home/alice", "/home/bob"} {
		if err := os.MkdirAll(n.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(n.path("/home/file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := nodeHomes(baota.OSFS{Root: n.root}, nil, "/Users/kevin")
	if strings.Join(got, ",") != "/root,/home/alice,/home/bob,/Users/kevin" {
		t.Fatalf("%v", got)
	}
	if got := nodeHomes(baota.OSFS{Root: n.root}, []string{}, "/x"); len(got) != 0 {
		t.Fatalf("显式空切片 = 不扫描: %v", got)
	}
}

// Collect：PATH 上没有 node 时，用非固定目录的安装填充 resolver.System 的 nodejs（取最高版本、来源为其绝对路径、带提示）；
// PATH 上有 node 时 PATH 优先（那才是实际会被执行的），非固定目录的安装仍列在 NodeInstalls。全程不执行这些路径下的二进制。
func TestCollectNodeFromNonStandardDirs(t *testing.T) {
	n := newNodeTree(t)
	n.install("/home/dev/.nvm/versions/node/v18.19.1", 0o755, nodeHeader(18, 19, 1))
	n.install("/home/dev/.nvm/versions/node/v20.11.0", 0o755, nodeHeader(20, 11, 0))
	n.install("/www/server/nodejs/v16.20.2", 0o755, "")

	sc := newExternalScripts(t, "ss/ss-lntup-root.txt", 0)
	o := collectOpts(t, sc, "empty")
	r, err := NewRunner([]string{sc.dir}) // 只含 ss/systemctl 替身所在目录：固定目录里没有 node
	if err != nil {
		t.Fatal(err)
	}
	o.Runner = r
	o.FS = baota.OSFS{Root: n.root}
	o.NodeHomes = []string{"/home/dev"}
	o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	res, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	d := res.System.Deps["nodejs"]
	if !d.Detected || d.Version != "v20.11.0" || d.Source != "/home/dev/.nvm/versions/node/v20.11.0/bin/node" || d.Flavor != "nodejs" {
		t.Fatalf("nodejs dep=%+v warnings=%v", d, res.Warnings)
	}
	if len(res.NodeInstalls) != 3 || !hasWarning(res, "dep.nodejs.not_in_path") {
		t.Fatalf("installs=%+v warnings=%v", res.NodeInstalls, res.Warnings)
	}
	n.assertNeverExecuted()

	// 没有任何安装且 PATH 无 node：不探测到
	o.NodeHomes = []string{}
	o.FS = baota.OSFS{Root: t.TempDir()}
	res, _ = Collect(context.Background(), o)
	if res.System.Deps["nodejs"].Detected || len(res.NodeInstalls) != 0 || hasWarning(res, "dep.nodejs.not_in_path") {
		t.Fatalf("不应探测到: %+v %v", res.System.Deps["nodejs"], res.Warnings)
	}

	// PATH 优先：用 collectOpts 默认（含本机 node 所在目录）
	node := realBinary(t, "node")
	o2 := collectOpts(t, newExternalScripts(t, "ss/ss-lntup-root.txt", 0), "empty")
	r2, err := NewRunner([]string{filepath.Dir(node)})
	if err != nil {
		t.Fatal(err)
	}
	o2.Runner = r2
	o2.FS = baota.OSFS{Root: n.root}
	o2.NodeHomes = []string{"/home/dev"}
	o2.MySQL, o2.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	res, _ = Collect(context.Background(), o2)
	want := strings.TrimSpace(realOutput(t, node, "--version"))
	if d := res.System.Deps["nodejs"]; d.Version != want || d.Source != "node" || len(res.NodeInstalls) != 3 || hasWarning(res, "dep.nodejs.not_in_path") {
		t.Fatalf("PATH 的 node 应优先: %+v installs=%d want=%s", d, len(res.NodeInstalls), want)
	}
	n.assertNeverExecuted()
}

func FuzzParseNodeVersionHeader(f *testing.F) {
	f.Add([]byte(nodeHeader(20, 11, 1)))
	f.Add([]byte("#define NODE_MAJOR_VERSION 99999999999999999999\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, ok := ParseNodeVersionHeader(b); ok && !reNodeVerDir.MatchString(v) && !strings.HasPrefix(v, "v") {
			t.Fatalf("版本格式异常: %q", v)
		}
	})
}
