package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// externalScripts 在临时目录里放置「外部程序」ss/systemctl 的可执行脚本：它们把收到的 argv 记入日志并输出夹具内容。
// 这是对**外部 Linux 程序**的替身（macOS 上没有 ss/systemctl），用于验证 Collect 到 safeexec 的接线与 argv——
// 解析器本身用的是夹具文本，不是替身。真实 ss/systemctl 的输出在 ENV-LNX 复验。
type externalScripts struct{ dir, ssLog, sysLog string }

func writeScript(t *testing.T, dir, name, logFile, outFile string, exit int) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\ncat %q\nexit %d\n", logFile, outFile, exit)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newExternalScripts(t *testing.T, ssFixture string, ssExit int) externalScripts {
	t.Helper()
	dir := t.TempDir()
	abs, _ := filepath.Abs(fixtureRoot)
	e := externalScripts{dir: dir, ssLog: filepath.Join(dir, "ss.log"), sysLog: filepath.Join(dir, "systemctl.log")}
	writeScript(t, dir, "ss", e.ssLog, filepath.Join(abs, ssFixture), ssExit)
	writeScript(t, dir, "systemctl", e.sysLog, filepath.Join(abs, "systemctl/list-units.txt"), 0)
	return e
}

func readLog(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func collectOpts(t *testing.T, scripts externalScripts, bt string) Options {
	t.Helper()
	dirs := []string{scripts.dir}
	if node, err := exec.LookPath("node"); err == nil {
		dirs = append(dirs, filepath.Dir(node))
	}
	r, err := NewRunner(dirs)
	if err != nil {
		t.Fatal(err)
	}
	le := true
	return Options{Runner: r, FS: baota.OSFS{Root: filepath.Join(fixtureRoot, "baota", bt)}, ProcRoot: materializeProc(t),
		UserDBPath: filepath.Join(fixtureRoot, "passwd"), LittleEndian: &le, DialTimeout: 2 * time.Second,
		Now: time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)}
}

func TestCollectEndToEnd(t *testing.T) {
	sc := newExternalScripts(t, "ss/ss-lntup-root.txt", 0)
	o := collectOpts(t, sc, "bt-full")
	o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	res, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	// 只读 argv：ss 与 systemctl 各被调用一次，参数恰为白名单里的数组
	if got := readLog(t, sc.ssLog); !reflect.DeepEqual(got, []string{"-H -lntup"}) {
		t.Errorf("ss argv=%q", got)
	}
	if got := readLog(t, sc.sysLog); !reflect.DeepEqual(got, []string{"list-units --type=service --all --no-legend --no-pager --plain"}) {
		t.Errorf("systemctl argv=%q", got)
	}
	s := res.System
	if s.OS != runtime.GOOS || s.Arch != runtime.GOARCH {
		t.Errorf("os/arch=%s/%s", s.OS, s.Arch)
	}
	if s.MemoryTotalMB <= 0 {
		t.Errorf("总内存=%d", s.MemoryTotalMB)
	}
	rootDisk := false
	for _, d := range s.Disks {
		if d.Mount == "/" && d.FreeMB > 0 && d.TotalMB >= d.FreeMB {
			rootDisk = true
		}
	}
	if !rootDisk {
		t.Errorf("磁盘缺少 /: %+v", s.Disks)
	}
	// 监听：来自 ss 夹具，用 /proc 夹具补全属主与单元
	if !res.ListenersKnown || res.ListenersSource != "ss" || len(s.Listeners) != 18 {
		t.Fatalf("listeners known=%v source=%s n=%d", res.ListenersKnown, res.ListenersSource, len(s.Listeners))
	}
	var nginx80 resolver.Listener
	for _, l := range s.Listeners {
		if l.Proto == "tcp" && l.Port == 80 && l.Addr == "0.0.0.0" {
			nginx80 = l
		}
	}
	if nginx80.PID != 1201 || nginx80.Process != "nginx" || nginx80.User != "root" || nginx80.Unit != "nginx.service" {
		t.Errorf("nginx:80 = %+v", nginx80)
	}
	// 宝塔
	if !s.Baota.Detected || s.Baota.Confidence != "high" || s.Baota.Version != "7.9.10" || s.Baota.PanelPort != 8888 || s.Baota.NginxPath != "/www/server/nginx/sbin/nginx" {
		t.Errorf("baota=%+v", s.Baota)
	}
	if res.Baota.Services == nil || len(res.Baota.Services) != 8 {
		t.Errorf("services=%+v", res.Baota.Services)
	}
	fw := map[string]string{}
	for _, f := range res.Baota.Firewalls {
		fw[f.Kind] = f.Active + "/" + f.Source
	}
	if !reflect.DeepEqual(fw, map[string]string{"ufw": "yes//etc/ufw/ufw.conf", "firewalld": "no/systemctl"}) {
		t.Errorf("firewalls=%v", fw)
	}
	// 依赖：MySQL/Redis 端点不可达，但宝塔环境里有安装证据 → 记录版本（真实临时实例的版本/认证探测见 tests/integration/hostprobe）
	if d := s.Deps["mysql"]; !d.Detected || d.Version != "5.7.44" {
		t.Errorf("mysql dep=%+v", d)
	}
	if node, err := exec.LookPath("node"); err == nil {
		want := strings.TrimSpace(realOutput(t, node, "--version"))
		if d := s.Deps["nodejs"]; !d.Detected || d.Version != want {
			t.Errorf("nodejs dep=%+v，期望 %s", d, want)
		}
	}
	// 结果可序列化，且 System 与 resolver.System 形状一致（JSON 往返不丢字段）
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.System, res.System) || !reflect.DeepEqual(back.Baota, res.Baota) {
		t.Error("JSON 往返后 System/Baota 不一致")
	}
	if err := res.Baota.Validate(); err != nil {
		t.Errorf("Baota 报告应可通过校验: %v", err)
	}
	// 没有凭据字段泄露
	if strings.Contains(string(b), "password") {
		t.Errorf("结果含 password 字样")
	}
}

// ss 不可用（找不到/非 0 退出）→ 回退 /proc/net；两个来源给出相同的监听集合（地址、端口、占用者）。
func TestCollectFallsBackToProc(t *testing.T) {
	cases := map[string]externalScripts{}
	// ss 脚本以非 0 退出（例如容器里没有权限）
	cases["ss 退出码 1"] = newExternalScripts(t, "ss/ss-lntup-root.txt", 1)
	// 找不到 ss：目录里没有 ss 脚本
	noss := newExternalScripts(t, "ss/ss-lntup-root.txt", 0)
	_ = os.Remove(filepath.Join(noss.dir, "ss"))
	cases["没有 ss"] = noss
	var viaSS Result
	{
		o := collectOpts(t, newExternalScripts(t, "ss/ss-lntup-root.txt", 0), "empty")
		o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
		var err error
		if viaSS, err = Collect(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	for name, sc := range cases {
		o := collectOpts(t, sc, "empty")
		o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
		res, err := Collect(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if res.ListenersSource != "proc" || !res.ListenersKnown {
			t.Fatalf("%s: source=%s known=%v warnings=%v", name, res.ListenersSource, res.ListenersKnown, res.Warnings)
		}
		if !hasWarning(res, "listeners.ss_unavailable") {
			t.Errorf("%s: 应有回退说明: %v", name, res.Warnings)
		}
		// 两个来源对同一台「机器」的描述：监听的 (proto, port, addr, pid, process) 一致
		if a, b := brief(viaSS.System.Listeners), brief(res.System.Listeners); !reflect.DeepEqual(a, b) {
			t.Errorf("%s: ss 与 /proc 的监听不一致:\n ss   %v\n proc %v", name, a, b)
		}
	}
}

func brief(ls []resolver.Listener) []string {
	var out []string
	for _, l := range ls {
		a := l.Addr
		if a == "*" || a == "::" { // ss 的 `*` 与 /proc/net/tcp6 的 :: 是同一类（双栈通配）
			a = "any6"
		}
		out = append(out, fmt.Sprintf("%s/%d/%s/%d/%s", l.Proto, l.Port, a, l.PID, l.Process))
	}
	sort.Strings(out)
	return out
}

func hasWarning(r Result, code string) bool {
	for _, w := range r.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// 既没有 ss 也没有可读的 /proc：ListenersKnown=false 且有明确警告；Assess 对每个想要的端口给出「无法检测」冲突，而不是放行。
func TestCollectListenersUnknownIsNotSilentlyClean(t *testing.T) {
	sc := newExternalScripts(t, "ss/ss-lntup-root.txt", 0)
	_ = os.Remove(filepath.Join(sc.dir, "ss"))
	o := collectOpts(t, sc, "empty")
	o.ProcRoot = filepath.Join(t.TempDir(), "no-proc")
	o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	res, _ := Collect(context.Background(), o)
	if res.ListenersKnown || res.ListenersSource != "none" || len(res.System.Listeners) != 0 || !hasWarning(res, "listeners.unavailable") {
		t.Fatalf("known=%v source=%s warnings=%v", res.ListenersKnown, res.ListenersSource, res.Warnings)
	}
	cs := Assess(res, nil, Intent{Ports: []PortWant{{Component: "node", Proto: "tcp", Port: 80}}})
	if len(cs) != 1 || cs[0].Kind != "port" || cs[0].Details["code"] != "unknown" {
		t.Fatalf("应报告无法检测: %+v", cs)
	}
}

// MySQL/Redis 不可达：只警告并引导（Tools 不代装、不改动）；本机有安装证据（宝塔）时仍记录版本。
func TestCollectUnreachableDepsWarnOnly(t *testing.T) {
	sc := newExternalScripts(t, "ss/ss-lntup-root.txt", 0)
	o := collectOpts(t, sc, "bt-full")
	o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	res, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.System.Deps["mysql"]; !d.Detected || d.Version != "5.7.44" || d.Source != "/www/server/mysql/bin/mysqld" {
		t.Errorf("宝塔 MySQL 的识别: %+v", d)
	}
	if !hasWarning(res, "dep.mysql.unreachable") || !hasWarning(res, "dep.redis.unreachable") {
		t.Errorf("warnings=%v", res.Warnings)
	}
	for _, w := range res.Warnings {
		if w.Code == "dep.mysql.unreachable" && !strings.Contains(w.Message, "不读取宝塔存储的 MySQL root 密码") {
			t.Errorf("宝塔环境下应说明不读取 root 密码: %s", w.Message)
		}
	}
	// 完全没有安装证据：未探测到 + 引导
	res, _ = Collect(context.Background(), collectOptsWith(t, "empty"))
	if d := res.System.Deps["mysql"]; d.Detected {
		t.Errorf("不应探测到: %+v", d)
	}
	if !hasWarning(res, "dep.mysql.absent") || !hasWarning(res, "dep.redis.absent") {
		t.Errorf("warnings=%v", res.Warnings)
	}
}

func collectOptsWith(t *testing.T, bt string) Options {
	o := collectOpts(t, newExternalScripts(t, "ss/ss-lntup-root.txt", 0), bt)
	o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
	return o
}

// 探测结果经 registry 写入 state.json 的 host.baota（既有原子写与 schema），读回一致；其他字段不受影响。
func TestRecordHostBaotaRoundTrip(t *testing.T) {
	res, err := Collect(context.Background(), collectOptsWith(t, "bt-full"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(registry.Layout{Root: filepath.Join(t.TempDir(), "tools")}, registry.Options{RunID: "run-probe-1", Command: "install server"})
	if err := reg.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	// 先有一个已装组件，确认写 host.baota 不破坏它
	if err := reg.UpdateState(context.Background(), func(s *registry.State) error {
		s.Components["server"] = registry.Component{Version: "0.1.0", State: "running", Ports: []int{8080}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordHostBaota(context.Background(), res.Baota); err != nil {
		t.Fatal(err)
	}
	st, exists, err := reg.LoadState()
	if err != nil || !exists {
		t.Fatalf("读回: %v %v", exists, err)
	}
	if st.SchemaVersion != registry.StateSchemaVersion || st.Host == nil || st.Host.Baota == nil || st.Components["server"].Version != "0.1.0" {
		t.Fatalf("state=%+v", st)
	}
	if !reflect.DeepEqual(*st.Host.Baota, res.Baota) {
		t.Errorf("读回的 host.baota 与探测结果不一致")
	}
	if fi, err := os.Stat(reg.L.StateFile()); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("state.json 权限: %v %v", fi, err)
	}
}
