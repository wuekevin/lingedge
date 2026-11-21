package resolver

import (
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// complexInput 覆盖多种机制：被动升级、内部闭包、端口覆盖、监听、多挂载点、外部依赖、警告。
func complexInput(t testing.TB) Input {
	m := buildManifest(t, [][2]string{{"server", "0.2.0"}},
		cs{name: "server", vers: []vs{serverVer("0.1.0"), serverVer("0.2.0"), serverVer("0.3.0")}},
		fronts("admin-web", "0.1.0"), fronts("console", "0.1.0", "0.2.0"), fronts("docs", "0.1.0"),
		cs{name: "website", vers: []vs{{ver: "0.1.0", variants: []string{"static", "ssr"}, ports: []pt{{"tcp", 3000, "127.0.0.1", false}},
			variantReq: map[string]map[string]any{"ssr": {"runtime": map[string]string{"node": ">=20 <23"}}}}}},
		cs{name: "node", vers: []vs{{ver: "0.1.0", comps: map[string]string{"openresty": ">=1.25 <2"}, peer: map[string]string{"server": ">=0.1 <1"}, ports: []pt{{"tcp", 80, "0.0.0.0", false}, {"tcp", 443, "0.0.0.0", false}}, memMB: 256, diskMB: 200}}},
		cs{name: "openresty", internal: true, vers: []vs{{ver: "1.25.3"}, {ver: "1.27.1"}}})
	sys := okSystem()
	sys.Disks = []Disk{{Mount: "/", FreeMB: 100000}, {Mount: "/opt", FreeMB: 50000}, {Mount: "/var", FreeMB: 40000}, {Mount: "/etc", FreeMB: 10}}
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 22, Process: "sshd"}, {Proto: "tcp", Addr: "::1", Port: 5432, Process: "postgres"},
		{Proto: "tcp", Addr: "127.0.0.1", Port: 3000, Process: "node", PID: 9}, {Proto: "udp", Addr: "0.0.0.0", Port: 68, Process: "dhclient"}}
	return Input{Manifest: m, System: sys, Installed: inst("server@0.1.0", "openresty@1.25.3", "console@0.1.0", "docs@0.1.0"),
		Want:          []Want{{Component: "website", Variant: "ssr"}, {Component: "node"}, {Component: "server"}, {Component: "admin-web"}, {Component: "console"}},
		PortOverrides: []PortOverride{{Component: "website", Proto: "tcp", Port: 3000, To: 3001}}}
}

// 确定性：同输入同输出——多次运行；打乱 Want/Listeners/Disks 顺序；Installed 与 Manifest 的 map 迭代序由 Go 运行时随机化，也不得影响输出。
func TestDeterministicAcrossRunsAndInputOrder(t *testing.T) {
	base := complexInput(t)
	ref := jsonOf(t, Resolve(base))
	if !strings.Contains(ref, `"overridden":true`) {
		t.Fatalf("前提：复杂输入应包含端口覆盖: %s", ref)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		in := complexInput(t) // 重新构建清单（map 迭代序每次不同）
		rng.Shuffle(len(in.Want), func(a, b int) { in.Want[a], in.Want[b] = in.Want[b], in.Want[a] })
		rng.Shuffle(len(in.System.Listeners), func(a, b int) {
			in.System.Listeners[a], in.System.Listeners[b] = in.System.Listeners[b], in.System.Listeners[a]
		})
		rng.Shuffle(len(in.System.Disks), func(a, b int) { in.System.Disks[a], in.System.Disks[b] = in.System.Disks[b], in.System.Disks[a] })
		if got := jsonOf(t, Resolve(in)); got != ref {
			t.Fatalf("第 %d 次输出不同:\n%s\n---\n%s", i, got, ref)
		}
	}
	// 同一个 Input 重复求解
	for i := 0; i < 50; i++ {
		if jsonOf(t, Resolve(base)) != ref {
			t.Fatal("重复求解结果不同")
		}
	}
}

// 纯函数护栏：本包的非测试源码不得 import 任何 IO/时钟/随机/进程/网络（net/netip 只是地址类型，允许）。
func TestPackageIsPure(t *testing.T) {
	banned := map[string]bool{"os": true, "io": true, "io/ioutil": true, "io/fs": true, "time": true, "math/rand": true, "math/rand/v2": true, "crypto/rand": true,
		"os/exec": true, "os/signal": true, "os/user": true, "net": true, "net/http": true, "syscall": true, "path/filepath": true, "runtime": true,
		"log": true, "log/slog": true, "unsafe": true, "context": true, "sync": true, "bufio": true}
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if banned[p] {
				t.Errorf("%s import 了 %q：resolver 必须是纯函数（无 IO/时钟/随机）", f, p)
			}
		}
		src, _ := os.ReadFile(f)
		for _, bad := range []string{"os.Getenv", "time.Now", "rand."} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s 含 %s", f, bad)
			}
		}
	}
	if n < 5 {
		t.Fatal("没有扫到源文件")
	}
}
