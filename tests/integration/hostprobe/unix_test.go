package hostprobeit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// Story 1.31b：unix socket 端点，对本机自启的真实临时 mysqld / redis-server 的真实 socket 做探测（不碰常驻实例）。

func TestProbeMySQLRealUnixSocket(t *testing.T) {
	ctx := context.Background()
	inst := mysqltest.Start(t)
	ep := probe.UnixEndpoint(inst.Socket)
	runner := runnerFor(t, realBinary(t, "mysql"))
	var wantVer string
	if err := inst.Admin(t).QueryRowContext(ctx, "SELECT VERSION()").Scan(&wantVer); err != nil {
		t.Fatal(err)
	}
	info, err := probe.CheckSocket(inst.Socket)
	if err != nil || !info.Trusted {
		t.Fatalf("真实 mysqld 的 socket 应存在、是 socket 且属主受信: %+v %v", info, err)
	}

	// 免凭据：读握手包取版本
	st := probe.ProbeMySQL(ctx, runner, ep, probe.Creds{}, 0)
	if !st.Reachable || st.Version != wantVer || st.Flavor != "mysql" || st.Authenticated || st.Error != "" || !st.Endpoint.IsUnix() {
		t.Fatalf("免凭据: %+v（期望 %s）", st, wantVer)
	}

	// 认证探测走 mysql 客户端 --socket；服务器端只看到 SELECT
	user, pw := mysqltest.RandomName("lps"), mysqltest.RandomSecret(t)
	inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED BY '%s'", user, pw))
	inst.DropUserOnCleanup(t, user)
	inst.Exec(t, "SET GLOBAL log_output='TABLE'")
	inst.Exec(t, "TRUNCATE mysql.general_log")
	inst.Exec(t, "SET GLOBAL general_log='ON'")
	t.Cleanup(func() { inst.Exec(t, "SET GLOBAL general_log='OFF'") })
	st = probe.ProbeMySQL(ctx, runner, ep, probe.Creds{MySQLUser: user, MySQLPassword: pw}, 0)
	if !st.Reachable || !st.Authenticated || st.Version != wantVer || st.Error != "" {
		t.Fatalf("有凭据: %+v", st)
	}
	rows, err := inst.Admin(t).QueryContext(ctx, "SELECT command_type, CONVERT(argument USING utf8mb4) FROM mysql.general_log WHERE user_host LIKE ?", user+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var cmds []string
	for rows.Next() {
		var ct, arg string
		if err := rows.Scan(&ct, &arg); err != nil {
			t.Fatal(err)
		}
		if ct == "Query" {
			cmds = append(cmds, arg)
		}
	}
	sawVersion := false
	for _, c := range cmds {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c)), "SELECT") {
			t.Errorf("探测用户经 socket 执行了非 SELECT 语句: %q", c)
		}
		sawVersion = sawVersion || c == "SELECT VERSION()"
	}
	if !sawVersion || len(cmds) > 2 {
		t.Fatalf("语句应是 SELECT VERSION()（至多外加一条只读 select），得到 %q", cmds)
	}
	// 错误口令：认证失败但握手仍给出版本；口令不泄露
	wrong := pw + "-wrong"
	st = probe.ProbeMySQL(ctx, runner, ep, probe.Creds{MySQLUser: user, MySQLPassword: wrong}, 0)
	if !st.Reachable || st.Authenticated || !st.AuthRequired || st.Version != wantVer || !strings.Contains(st.Error, "Access denied") {
		t.Fatalf("错误口令: %+v", st)
	}
	if b, _ := json.Marshal(st); strings.Contains(string(b), pw) || strings.Contains(string(b), wrong) {
		t.Fatalf("口令出现在结果里: %s", b)
	}
	// 符号链接（/tmp/mysql.sock → 真实 socket 的常见布局）
	link := filepath.Join(inst.Dir, "mysql.sock")
	if err := os.Symlink(inst.Socket, link); err != nil {
		t.Fatal(err)
	}
	if st := probe.ProbeMySQL(ctx, runner, probe.UnixEndpoint(link), probe.Creds{}, 0); !st.Reachable || st.Version != wantVer {
		t.Fatalf("经符号链接: %+v", st)
	}
	// 同路径下不是 socket 的文件：明确失败
	notSock := filepath.Join(inst.Dir, "notsock.sock")
	_ = os.WriteFile(notSock, []byte("x"), 0o600)
	if st := probe.ProbeMySQL(ctx, runner, probe.UnixEndpoint(notSock), probe.Creds{}, 0); st.Reachable || !strings.Contains(st.Error, "不是 socket") {
		t.Fatalf("非 socket: %+v", st)
	}
}

func TestProbeRedisRealUnixSocket(t *testing.T) {
	ctx := context.Background()
	wantVer := probe.ParseRedisServerVersion(redistest.Version())
	open := redistest.Start(t, redistest.Options{UnixSocket: true})
	if open.Socket == "" {
		t.Fatal("redistest 应暴露 socket 路径")
	}
	st := probe.ProbeRedis(ctx, probe.UnixEndpoint(open.Socket), probe.Creds{}, 0)
	if !st.Reachable || st.AuthRequired || st.Version != wantVer || st.Error != "" || !st.Endpoint.IsUnix() {
		t.Fatalf("无口令: %+v（期望 %s）", st, wantVer)
	}

	pw := mysqltest.RandomSecret(t)
	locked := redistest.Start(t, redistest.Options{Password: pw, UnixSocket: true})
	lep := probe.UnixEndpoint(locked.Socket)
	if st := probe.ProbeRedis(ctx, lep, probe.Creds{}, 0); !st.Reachable || !st.AuthRequired || st.Authenticated || st.Version != "" {
		t.Fatalf("需要口令且未提供: %+v", st)
	}
	tcp := probe.Endpoint{Host: "127.0.0.1", Port: locked.Port}
	before := redisCommandstats(t, tcp, pw)
	st = probe.ProbeRedis(ctx, lep, probe.Creds{RedisPassword: pw}, 0)
	if !st.Reachable || !st.Authenticated || st.Version != wantVer || st.Error != "" {
		t.Fatalf("正确口令（经 socket）: %+v", st)
	}
	after := redisCommandstats(t, tcp, pw)
	for cmd, n := range after {
		if n > before[cmd] && cmd != "ping" && cmd != "auth" && cmd != "info" {
			t.Errorf("经 socket 探测期间执行了非只读命令 %s（%d → %d）", cmd, before[cmd], n)
		}
	}
	if after["auth"] <= before["auth"] || after["info"] <= before["info"] {
		t.Errorf("commandstats 应记录到 AUTH/INFO: %v → %v", before, after)
	}
	wrong := "wrong-" + pw
	st = probe.ProbeRedis(ctx, lep, probe.Creds{RedisPassword: wrong}, 0)
	if !st.Reachable || st.Authenticated || !strings.Contains(st.Error, "AUTH 被拒绝") {
		t.Fatalf("错误口令: %+v", st)
	}
	if b, _ := json.Marshal(st); strings.Contains(string(b), pw) || strings.Contains(string(b), wrong) {
		t.Fatalf("口令出现在结果里: %s", b)
	}
	// 实例关闭后 socket 文件消失或拒绝连接：不可达
	locked.Stop()
	if st := probe.ProbeRedis(ctx, lep, probe.Creds{}, time.Second); st.Reachable {
		t.Fatalf("已停止的实例不应可达: %+v", st)
	}
}

// 完整 Collect：TCP 端点不可达（宝塔常见：只开 socket），声明的 socket / 自动发现的 socket 分别被探测到，写入 resolver.System 的来源是 unix。
func TestCollectWithRealUnixSockets(t *testing.T) {
	my, rd := mysqltest.Start(t), redistest.Start(t, redistest.Options{UnixSocket: true})
	closedTCP := func() probe.Endpoint { return probe.Endpoint{Host: "127.0.0.1", Port: closedPort(t)} }
	wantMy := ""
	if err := my.Admin(t).QueryRow("SELECT VERSION()").Scan(&wantMy); err != nil {
		t.Fatal(err)
	}
	base := func() probe.Options {
		return options(t, newScripts(t), "bt-full", closedTCP(), closedTCP(), "")
	}

	// 声明 socket
	o := base()
	o.MySQLSockets, o.RedisSockets = []string{my.Socket}, []string{rd.Socket}
	res, err := probe.Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.System.Deps["mysql"]; !d.Detected || d.Version != wantMy || d.Source != "unix "+my.Socket {
		t.Errorf("mysql dep=%+v warnings=%v", d, res.Warnings)
	}
	if d := res.System.Deps["redis"]; !d.Detected || d.Version != probe.ParseRedisServerVersion(redistest.Version()) || d.Source != "unix "+rd.Socket {
		t.Errorf("redis dep=%+v", d)
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Code, "dep.") {
			t.Errorf("依赖经 socket 可达时不应有依赖警告: %+v", w)
		}
	}
	b, _ := json.Marshal(res)
	var back probe.Result
	if err := json.Unmarshal(b, &back); err != nil || len(back.Deps) != len(res.Deps) {
		t.Errorf("JSON 往返: %v", err)
	}

	// 自动发现：socket 出现在（模拟的）/proc/net/unix 里，TCP 都不可达。默认候选清空，避免探测到开发机上常驻的 /tmp/mysql.sock。
	oldMy, oldRd := probe.DefaultMySQLSockets, probe.DefaultRedisSockets
	probe.DefaultMySQLSockets, probe.DefaultRedisSockets = nil, nil
	t.Cleanup(func() { probe.DefaultMySQLSockets, probe.DefaultRedisSockets = oldMy, oldRd })
	dir, err := os.MkdirTemp("", "lpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// 名字符合约定（mysqld.sock / redis.sock）的符号链接指向真实 socket；/proc/net/unix 列出这两个监听路径
	myLink, rdLink := filepath.Join(dir, "mysqld.sock"), filepath.Join(dir, "redis.sock")
	if err := os.Symlink(my.Socket, myLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rd.Socket, rdLink); err != nil {
		t.Fatal(err)
	}
	proc := procRoot(t)
	unixTab := "Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"ffff0001: 00000002 00000000 00010000 0001 01 1001 " + myLink + "\n" +
		"ffff0002: 00000002 00000000 00010000 0001 01 1002 " + rdLink + "\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(unixTab), 0o644); err != nil {
		t.Fatal(err)
	}
	o = base()
	o.ProcRoot = proc
	o.MySQLSockets, o.RedisSockets = nil, nil
	res, err = probe.Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.System.Deps["mysql"]; !d.Detected || d.Version != wantMy || d.Source != "unix "+myLink {
		t.Errorf("自动发现 mysql: %+v warnings=%v", d, res.Warnings)
	}
	if d := res.System.Deps["redis"]; !d.Detected || d.Source != "unix "+rdLink {
		t.Errorf("自动发现 redis: %+v", d)
	}
}
