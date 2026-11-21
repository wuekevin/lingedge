package serverintegration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// notifySocket 是测试自建的 Unix 数据报 socket，扮演 systemd 的 $NOTIFY_SOCKET，收集子进程发来的状态串。
type notifySocket struct {
	path string
	conn *net.UnixConn
	mu   sync.Mutex
	got  []string
}

func newNotifySocket(t testing.TB, dir string) *notifySocket {
	t.Helper()
	p := filepath.Join(dir, "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: p, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	n := &notifySocket{path: p, conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			k, _, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.got = append(n.got, string(buf[:k]))
			n.mu.Unlock()
		}
	}()
	return n
}

func (n *notifySocket) has(s string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, g := range n.got {
		if g == s {
			return true
		}
	}
	return false
}

func (n *notifySocket) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.got...)
}

// proc 是真实的 lingedge-server 子进程。
type proc struct {
	cmd    *exec.Cmd
	out    *syncBuf
	done   chan struct{}
	exit   int
	waitEr error
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func startProc(t testing.TB, e *env, extraEnv []string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(buildServer(t), args...)
	cmd.Env = e.environ(extraEnv...)
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		p.waitEr = cmd.Wait()
		p.exit = cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *proc) wait(t testing.TB, d time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
		return p.exit
	case <-time.After(d):
		t.Fatalf("子进程 %v 内未退出\n日志:\n%s", d, p.out.String())
		return -1
	}
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func httpGet(t testing.TB, url string) (int, []byte, http.Header) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func dialRefused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// AC1/AC3/AC4/AC5：真实子进程——默认全角色启动、READY=1 只写 NOTIFY_SOCKET、/healthz 与 /healthz/ready（含迁移版本）、
// metrics、healthz 子命令、日志含 request-id；SIGTERM → STOPPING=1 → 退出码 0，监听全部关闭。
func TestServerProcessLifecycle(t *testing.T) {
	e := newEnv(t, all)
	ns := newNotifySocket(t, e.dir)
	p := startProc(t, e, []string{"NOTIFY_SOCKET=" + ns.path}, "--config", e.cfgPath)

	eventually(t, "READY=1", 30*time.Second, func() bool { return ns.has("READY=1") || p.exited() })
	if p.exited() {
		t.Fatalf("进程提前退出: %d\n%s", p.exit, p.out.String())
	}
	api := "http://127.0.0.1:" + strconv.Itoa(e.ports.http)

	code, body, hdr := httpGet(t, api+"/healthz")
	if code != 200 || hdr.Get("X-Request-ID") == "" {
		t.Fatalf("/healthz: %d %s hdr=%v", code, body, hdr)
	}
	code, body, _ = httpGet(t, api+"/healthz/ready")
	var rep httpx.ReadyReport
	if err := json.Unmarshal(body, &rep); err != nil || code != 200 || rep.Status != httpx.StatusOK {
		t.Fatalf("/healthz/ready: %d %s %v", code, body, err)
	}
	if m := rep.Checks["migrations"]; m.Status != "ok" || m.Applied == nil || m.Latest == nil || *m.Applied != 18 || *m.Latest != 18 || m.Pending == nil || *m.Pending != 0 {
		t.Fatalf("app 账号应能读到迁移版本并与内嵌一致: %+v", rep.Checks["migrations"])
	}
	if rep.Checks["database"].Status != "up" || rep.Checks["redis"].Status != "up" {
		t.Fatalf("依赖状态: %+v", rep.Checks)
	}
	// 未实现的业务路由：不存在（404），不是 501 占位。
	if code, _, _ := httpGet(t, api+"/api/v1/sites"); code != 404 {
		t.Fatalf("M1 未实现路由应 404（不占位）: %d", code)
	}
	// metrics 仅在配置地址上（回环）。
	code, body, _ = httpGet(t, "http://127.0.0.1:"+strconv.Itoa(e.ports.metrics)+"/metrics")
	if code != 200 || !bytes.Contains(body, []byte("lingedge_db_open_connections")) || !bytes.Contains(body, []byte("lingedge_redis_up 1")) {
		t.Fatalf("/metrics: %d\n%s", code, body)
	}
	// grpc 角色是骨架：不监听 gRPC 端口。
	for _, port := range []int{e.ports.grpc, e.ports.enroll} {
		if !dialRefused("127.0.0.1:" + strconv.Itoa(port)) {
			t.Fatalf("gRPC 角色尚未实现，端口 %d 不应被监听", port)
		}
	}
	// healthz 子命令（真实）：对运行中的 Server 返回 0；--ready 同样。
	for _, args := range [][]string{{"healthz", "--config", e.cfgPath}, {"healthz", "--config", e.cfgPath, "--ready"}, {"healthz", "--addr", "127.0.0.1:" + strconv.Itoa(e.ports.http)}} {
		cmd := exec.Command(buildServer(t), args...)
		cmd.Env = e.environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v 应成功: %v\n%s", args, err, out)
		}
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, 40*time.Second); code != 0 {
		t.Fatalf("SIGTERM 后应退出 0，实际 %d\n%s", code, p.out.String())
	}
	if !ns.has("STOPPING=1") {
		t.Errorf("停机时应发 STOPPING=1: %v", ns.all())
	}
	for _, port := range []int{e.ports.http, e.ports.metrics} {
		if !dialRefused("127.0.0.1:" + strconv.Itoa(port)) {
			t.Errorf("停机后端口 %d 应已关闭", port)
		}
	}
	// 只写 NOTIFY_SOCKET：数据报恰为 READY=1 与 STOPPING=1，没有别的副作用。
	if got := ns.all(); len(got) != 2 || got[0] != "READY=1" || got[1] != "STOPPING=1" {
		t.Errorf("通知序列应为 [READY=1 STOPPING=1]: %v", got)
	}

	// 日志：JSON、含 request_id（启动/停机用进程级 id；访问日志用请求 id），且不含口令/密钥。
	logs := p.out.String()
	var startRID string
	sawAccess := false
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("日志行不是 JSON: %q", sc.Text())
		}
		rid, _ := m["request_id"].(string)
		if rid == "" {
			t.Errorf("日志行缺少 request_id: %s", sc.Text())
		}
		if m["msg"] == "lingedge-server 启动" {
			startRID = rid
		}
		if m["msg"] == "http_request" && m["path"] == "/api/v1/sites" {
			sawAccess = true
		}
		if m["msg"] == "优雅停机完成" && rid != startRID {
			t.Errorf("启动与停机日志应共用进程级 request_id: %q vs %q", startRID, rid)
		}
	}
	if startRID == "" || !sawAccess {
		t.Errorf("应见启动日志与访问日志: start=%q access=%v\n%s", startRID, sawAccess, logs)
	}
	if strings.Contains(logs, e.appPW) {
		t.Error("日志泄露了数据库口令")
	}
	b, _ := os.ReadFile(e.keyFile)
	if i := strings.IndexByte(string(b), ':'); i > 0 && strings.Contains(logs, strings.TrimSpace(string(b)[i+1:])) {
		t.Error("日志泄露了主密钥")
	}
}

// AC4：SIGTERM 时在途请求被排空（不被截断），新连接立即被拒，退出码 0。
// 手法：SIGSTOP 临时 mysqld，使 /healthz/ready 的数据库探测卡住（在途请求），随后发 SIGTERM，再 SIGCONT 放行。
func TestServerSIGTERMDrainsInFlightRequest(t *testing.T) {
	e := newEnv(t, all)
	ns := newNotifySocket(t, e.dir)
	p := startProc(t, e, []string{"NOTIFY_SOCKET=" + ns.path}, "--config", e.cfgPath, "--roles=api")
	eventually(t, "READY=1", 30*time.Second, func() bool { return ns.has("READY=1") || p.exited() })
	if p.exited() {
		t.Fatalf("进程提前退出\n%s", p.out.String())
	}
	api := "127.0.0.1:" + strconv.Itoa(e.ports.http)

	pidB, err := os.ReadFile(filepath.Join(e.inst.Dir, "m.pid"))
	if err != nil {
		t.Fatal(err)
	}
	mysqldPID, err := strconv.Atoi(strings.TrimSpace(string(pidB)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(mysqldPID, syscall.SIGSTOP); err != nil { // 仅暂停本测试自拉起的临时 mysqld
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			_ = syscall.Kill(mysqldPID, syscall.SIGCONT)
		}
	}
	t.Cleanup(resume)

	type result struct {
		code int
		body []byte
		at   time.Time
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		c := &http.Client{Timeout: 20 * time.Second}
		resp, err := c.Get("http://" + api + "/healthz/ready")
		if err != nil {
			resCh <- result{err: err, at: time.Now()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, rerr := io.ReadAll(resp.Body)
		resCh <- result{code: resp.StatusCode, body: b, at: time.Now(), err: rerr}
	}()
	time.Sleep(400 * time.Millisecond) // 请求已到达并卡在数据库探测上

	termAt := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	eventually(t, "停机开始后新连接被拒", 5*time.Second, func() bool { return dialRefused(api) })
	if p.exited() {
		t.Fatalf("在途请求尚未完成，进程不应已退出（应排空）\n%s", p.out.String())
	}
	resume() // 放行数据库；在途请求应完成
	r := <-resCh
	if r.err != nil {
		t.Fatalf("在途请求被截断（应被排空）: %v\n%s", r.err, p.out.String())
	}
	if !r.at.After(termAt) {
		t.Fatalf("在途请求应在 SIGTERM 之后才完成")
	}
	var rep httpx.ReadyReport
	if err := json.Unmarshal(r.body, &rep); err != nil || (r.code != 200 && r.code != 503) {
		t.Fatalf("在途请求应得到完整的就绪响应: %d %s %v", r.code, r.body, err)
	}
	if code := p.wait(t, 40*time.Second); code != 0 {
		t.Fatalf("排空后应退出 0，实际 %d\n%s", code, p.out.String())
	}
	if !strings.Contains(p.out.String(), "优雅停机完成") {
		t.Errorf("日志应含「优雅停机完成」\n%s", p.out.String())
	}
}

// AC5：缺主密钥 / 权限不符 / 数据库不可用 / 端口占用 / 非法角色 → 拒绝启动（非零退出、给出指引、不发 READY=1）。
func TestServerRefusesToStart(t *testing.T) {
	e := newEnv(t, all)
	cases := []struct {
		name  string
		setup func(t *testing.T) []string // 返回 args
		code  int
		want  string
	}{
		{"主密钥文件不存在", func(t *testing.T) []string {
			_ = os.Remove(e.keyFile)
			return []string{"--config", e.cfgPath}
		}, 1, "主密钥"},
		{"主密钥权限过宽（0644）", func(t *testing.T) []string {
			e.writeKey(t, 0o644)
			return []string{"--config", e.cfgPath}
		}, 1, "主密钥"},
		{"主密钥格式非法", func(t *testing.T) []string {
			_ = os.Remove(e.keyFile)
			if err := os.WriteFile(e.keyFile, []byte("garbage\n"), 0o400); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", e.cfgPath}
		}, 1, "主密钥"},
		{"非法角色", func(t *testing.T) []string {
			e.writeKey(t, 0o400)
			return []string{"--config", e.cfgPath, "--roles=api,bogus"}
		}, 2, "未知角色"},
		{"空角色项", func(t *testing.T) []string {
			return []string{"--config", e.cfgPath, "--roles=api,,worker"}
		}, 2, "空项"},
		{"端口被占用", func(t *testing.T) []string {
			l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.ports.http))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			return []string{"--config", e.cfgPath}
		}, 1, "监听"},
		{"配置文件不存在", func(t *testing.T) []string {
			return []string{"--config", filepath.Join(e.dir, "nope.yaml")}
		}, 1, "配置"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := c.setup(t)
			ns := newNotifySocket(t, e.dir)
			defer func() { _ = os.Remove(ns.path) }()
			p := startProc(t, e, []string{"NOTIFY_SOCKET=" + ns.path}, args...)
			code := p.wait(t, 30*time.Second)
			if code != c.code {
				t.Fatalf("退出码 = %d，期望 %d\n%s", code, c.code, p.out.String())
			}
			if !strings.Contains(p.out.String(), c.want) {
				t.Errorf("输出应含 %q\n%s", c.want, p.out.String())
			}
			if ns.has("READY=1") {
				t.Error("拒绝启动时不得发 READY=1")
			}
		})
	}
	t.Run("数据库口令错误", func(t *testing.T) {
		e.writeKey(t, 0o400)
		cmd := exec.Command(buildServer(t), "--config", e.cfgPath)
		cmd.Env = e.environ(envKV("LINGEDGE_DATABASE__PASSWORD", "wrong-"+e.appPW))
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), "无法连接数据库") {
			t.Fatalf("应拒绝启动: %v\n%s", err, out)
		}
		if strings.Contains(string(out), e.appPW) {
			t.Error("输出泄露了口令")
		}
	})
	t.Run("metrics 监听非回环被拒（Story 1.22b）", func(t *testing.T) {
		b, _ := os.ReadFile(e.cfgPath)
		p2 := filepath.Join(e.dir, "metricscfg.yaml")
		bad := bytes.Replace(b, []byte("metrics:\n  listen: 127.0.0.1:"), []byte("metrics:\n  listen: 0.0.0.0:"), 1)
		if bytes.Equal(b, bad) {
			t.Fatal("测试配置里没有 metrics 监听行（格式变化？）")
		}
		if err := os.WriteFile(p2, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(buildServer(t), "--config", p2)
		cmd.Env = e.environ()
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), "metrics.listen") || !strings.Contains(string(out), "回环") {
			t.Fatalf("非回环 metrics 监听应在配置阶段拒绝启动: %v\n%s", err, out)
		}
	})
	t.Run("配置里写了数据库口令被拒", func(t *testing.T) {
		b, _ := os.ReadFile(e.cfgPath)
		p2 := filepath.Join(e.dir, "badcfg.yaml")
		leaked := mysqltest.RandomSecret(t) // 运行期随机：仓库里不存在任何固定口令
		if err := os.WriteFile(p2, bytes.Replace(b, []byte("  user: "), []byte(fmt.Sprintf("  password: %s\n  user: ", leaked)), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(buildServer(t), "--config", p2)
		cmd.Env = e.environ()
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "不得写在配置文件中") || strings.Contains(string(out), leaked) {
			t.Fatalf("应拒绝且不回显口令: %v\n%s", err, out)
		}
	})
}

// 子命令面：setup 在 1.24 前不注册；version、未知子命令；healthz 对未运行的 Server 失败。
func TestSubcommandSurface(t *testing.T) {
	e := newEnv(t, m1Only)
	bin := buildServer(t)
	run := func(args ...string) (string, int) {
		out, err := exec.Command(bin, args...).CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	// Story 1.24：setup 子命令已注册（无头安装）：缺 --from-file 是用法错误（2），而不是「未知子命令」。
	if out, code := run("setup"); code != 2 || strings.Contains(out, "未知子命令") || !strings.Contains(out, "--from-file") {
		t.Errorf("setup 应已注册（缺 --from-file 为用法错误）: %d %s", code, out)
	}
	if out, code := run("bogus"); code != 2 || !strings.Contains(out, "未知子命令") {
		t.Errorf("未知子命令: %d %s", code, out)
	}
	if out, code := run("version"); code != 0 || !strings.Contains(out, "lingedge-server") {
		t.Errorf("version: %d %s", code, out)
	}
	if out, code := run("--version"); code != 0 || !strings.Contains(out, "lingedge-server") {
		t.Errorf("--version: %d %s", code, out)
	}
	if out, code := run("healthz", "--addr", "127.0.0.1:"+strconv.Itoa(e.ports.http), "--timeout", "1s"); code != 1 {
		t.Errorf("healthz 对未运行的 Server 应退出 1: %d %s", code, out)
	}
	// migrate 子命令真实可用：status 经迁移账号（环境变量凭据）连库。
	cmd := exec.Command(bin, "migrate", "status", "--config", e.cfgPath, "--user", e.migUser, "--json")
	cmd.Env = e.environ(envKV("LINGEDGE_MIGRATE_DB_PASSWORD", e.migPW), envKV("LINGEDGE_MIGRATE_DB_USER", e.migUser))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "latest") {
		t.Errorf("migrate status: %v\n%s", err, out)
	}
}

// envKV 组装 KEY=VALUE 环境变量项（口令均来自运行期随机值）。
func envKV(k, v string) string { return k + "=" + v }
