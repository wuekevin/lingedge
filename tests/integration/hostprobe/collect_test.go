package hostprobeit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// procRoot 把 tests/fixtures/hostprobe/proc 实体化到临时目录（fd 符号链接按 fdmap.txt 创建）。
func procRoot(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join(fixtureRoot, "proc")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, r), 0o755)
		}
		if r == "fdmap.txt" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, r), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(fixtureRoot, "proc/fdmap.txt"))
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) != 3 || strings.HasPrefix(ln, "#") {
			continue
		}
		dir := filepath.Join(dst, f[0], "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f[2], filepath.Join(dir, f[1])); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func options(t *testing.T, sc scripts, bt string, my, rd probe.Endpoint, mysqlBin string) probe.Options {
	t.Helper()
	dirs := []string{sc.dir}
	if node, err := exec.LookPath("node"); err == nil {
		dirs = append(dirs, filepath.Dir(node))
	}
	if mysqlBin != "" {
		dirs = append(dirs, filepath.Dir(mysqlBin))
	}
	r, err := probe.NewRunner(dirs)
	if err != nil {
		t.Fatal(err)
	}
	le := true
	return probe.Options{Runner: r, FS: baota.OSFS{Root: filepath.Join(fixtureRoot, "baota", bt)}, ProcRoot: procRoot(t),
		UserDBPath: filepath.Join(fixtureRoot, "passwd"), LittleEndian: &le, DialTimeout: 2 * time.Second,
		MySQL: []probe.Endpoint{my}, Redis: []probe.Endpoint{rd}, Now: time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)}
}

// 真实临时 MySQL/Redis 参与的完整 Collect：依赖版本来自真实实例；宝塔/监听/服务来自夹具；结果可 JSON 往返。
func TestCollectWithRealDependencies(t *testing.T) {
	my, rd := mysqltest.Start(t), redistest.Start(t)
	sc := newScripts(t)
	o := options(t, sc, "bt-full", probe.Endpoint{Host: my.Host(), Port: my.Port}, probe.Endpoint{Host: "127.0.0.1", Port: rd.Port}, "")
	res, err := probe.Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var wantMy string
	if err := my.Admin(t).QueryRow("SELECT VERSION()").Scan(&wantMy); err != nil {
		t.Fatal(err)
	}
	if d := res.System.Deps["mysql"]; !d.Detected || d.Version != wantMy || d.Flavor != "mysql" || !strings.HasPrefix(d.Source, "tcp 127.0.0.1:") {
		t.Errorf("mysql dep=%+v（期望 %s）", d, wantMy)
	}
	if d := res.System.Deps["redis"]; !d.Detected || d.Version != probe.ParseRedisServerVersion(redistest.Version()) {
		t.Errorf("redis dep=%+v", d)
	}
	if node, err := exec.LookPath("node"); err == nil {
		out, _ := exec.Command(node, "--version").Output()
		if d := res.System.Deps["nodejs"]; !d.Detected || d.Version != strings.TrimSpace(string(out)) {
			t.Errorf("nodejs dep=%+v", d)
		}
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Code, "dep.") {
			t.Errorf("依赖可达时不应有依赖警告: %+v", w)
		}
	}
	b, _ := json.Marshal(res)
	var back probe.Result
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back.System, res.System) {
		t.Errorf("JSON 往返: %v", err)
	}
	if err := res.Baota.Validate(); err != nil {
		t.Error(err)
	}
}

// 需要口令的 Redis、调用方没提供凭据：可达并警告由向导完成；提供凭据后版本可得；结果与警告里没有口令。
func TestCollectRedisAuthRequiredWithoutCreds(t *testing.T) {
	pw := mysqltest.RandomSecret(t)
	rd := redistest.Start(t, redistest.Options{Password: pw})
	o := options(t, newScripts(t), "empty", probe.Endpoint{Host: "127.0.0.1", Port: closedPort(t)}, probe.Endpoint{Host: "127.0.0.1", Port: rd.Port}, "")
	res, _ := probe.Collect(context.Background(), o)
	if d := res.System.Deps["redis"]; !d.Detected {
		t.Fatalf("redis=%+v", d)
	}
	has := false
	for _, w := range res.Warnings {
		has = has || w.Code == "dep.redis.auth_required"
	}
	if !has {
		t.Errorf("warnings=%v", res.Warnings)
	}
	o.Creds = probe.Creds{RedisPassword: pw}
	res, _ = probe.Collect(context.Background(), o)
	if d := res.System.Deps["redis"]; d.Version != probe.ParseRedisServerVersion(redistest.Version()) {
		t.Errorf("redis=%+v", d)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), pw) {
		t.Fatal("口令出现在 Collect 结果里")
	}
}

// 端到端：probe 的输出直接作为 resolver 的 System 输入；宝塔存在不阻止安装，真实端口冲突才阻止并带占用者信息。
func TestSystemFeedsResolver(t *testing.T) {
	my, rd := mysqltest.Start(t), redistest.Start(t)
	o := options(t, newScripts(t), "bt-full", probe.Endpoint{Host: my.Host(), Port: my.Port}, probe.Endpoint{Host: "127.0.0.1", Port: rd.Port}, "")
	res, err := probe.Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	sys := res.System
	sys.OS, sys.Arch = "linux", "amd64" // 本机是 darwin；其余字段保持探测结果
	sys.MemoryTotalMB, sys.MemoryAvailableMB = 16384, 8192
	sys.Disks = []resolver.Disk{{Mount: "/", FreeMB: 500000}}
	m, err := manifest.ParseManifest([]byte(`{"schema_version":1,"channel":"stable","sequence":1,"generated_at":"2026-01-01T00:00:00Z","valid_until":"2026-04-01T00:00:00Z","revoked_versions":[],"components":{
	 "server":{"status":"available","versions":[{"version":"0.1.0","requires":{"external":{"mysql":">=8.0.16 <9","redis":">=7"},"system":{"min_memory_mb":1024,"min_disk_mb":2048}},
	   "ports":[{"proto":"tcp","port":8080,"bind":"127.0.0.1"}],
	   "variants":{"default":{"artifacts":{"any":{"url":"https://releases.example.invalid/server.tar.gz","sha256":"` + strings.Repeat("a", 64) + `","size":1048576}}}}}]},
	 "node":{"status":"available","versions":[{"version":"0.1.0","ports":[{"proto":"tcp","port":80,"bind":"0.0.0.0"},{"proto":"tcp","port":443,"bind":"0.0.0.0"}],
	   "variants":{"default":{"artifacts":{"any":{"url":"https://releases.example.invalid/node.tar.gz","sha256":"` + strings.Repeat("b", 64) + `","size":1048576}}}}}]}}}`))
	if err != nil {
		t.Fatalf("清单解析: %v", err)
	}
	plan := resolver.Resolve(resolver.Input{Want: []resolver.Want{{Component: "server"}, {Component: "node"}}, Installed: registry.NewState(), Manifest: m, System: sys})
	ports := []string{}
	seen := map[string]bool{}
	for _, b := range plan.Blockers {
		if b.Kind != resolver.KindPort {
			t.Errorf("除端口冲突外不应有其他阻断（宝塔存在、MySQL/Redis 版本都满足）: %+v", b)
			continue
		}
		occ, _ := b.Details["occupant"].(map[string]any)
		k := fmt.Sprintf("%s:%v:%v:%v", b.Component, b.Details["port"], occ["process"], occ["user"])
		if !seen[k] { // resolver 对同一端口上每个监听地址各出一条阻断
			seen[k] = true
			ports = append(ports, k)
		}
	}
	// node 的 80/443 被 nginx（宝塔）占用；server 的 8080/127.0.0.1 被 node(*:8080) 的通配占用
	if want := []string{"node:443:nginx:root", "node:80:nginx:root", "server:8080:node:ubuntu"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("端口阻断=%v，期望 %v", ports, want)
	}
	hasBT := false
	for _, w := range plan.Warnings {
		hasBT = hasBT || w.Code == "baota.detected"
	}
	if !hasBT {
		t.Errorf("应有 baota.detected 提示: %+v", plan.Warnings)
	}
	// 同样的探测结果：Assess 与 resolver 对端口冲突的结论一致（同一套地址语义），且同一端口合并为一条
	conf := probe.Assess(res, nil, probe.Intent{Ports: []probe.PortWant{{Component: "node", Proto: "tcp", Port: 80, Bind: "0.0.0.0"}, {Component: "node", Proto: "tcp", Port: 443, Bind: "0.0.0.0"},
		{Component: "server", Proto: "tcp", Port: 8080, Bind: "127.0.0.1"}, {Component: "server", Proto: "tcp", Port: 9443, Bind: "0.0.0.0"}}})
	if len(conf) != 3 {
		t.Errorf("Assess 冲突数=%d: %+v", len(conf), conf)
	}
}
