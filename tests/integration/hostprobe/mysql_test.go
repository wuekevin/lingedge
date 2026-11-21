package hostprobeit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

func TestProbeMySQLRealInstance(t *testing.T) {
	ctx := context.Background()
	inst := mysqltest.Start(t)
	ep := probe.Endpoint{Host: inst.Host(), Port: inst.Port}
	runner := runnerFor(t, realBinary(t, "mysql"))
	var wantVer string
	if err := inst.Admin(t).QueryRowContext(ctx, "SELECT VERSION()").Scan(&wantVer); err != nil {
		t.Fatal(err)
	}

	// 免凭据：只读握手包即可得到版本，不发送任何认证数据
	st := probe.ProbeMySQL(ctx, runner, ep, probe.Creds{}, 0)
	if !st.Reachable || st.Version != wantVer || st.Flavor != "mysql" || st.Authenticated || st.Error != "" {
		t.Fatalf("免凭据: %+v（期望版本 %s）", st, wantVer)
	}

	// 有凭据：经 safeexec 的 mysql 客户端认证，只执行只读 SELECT VERSION()
	user := mysqltest.RandomName("lp")
	pw := mysqltest.RandomSecret(t)
	inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pw))
	inst.DropUserOnCleanup(t, user)
	inst.Exec(t, "SET GLOBAL log_output='TABLE'")
	inst.Exec(t, "TRUNCATE mysql.general_log")
	inst.Exec(t, "SET GLOBAL general_log='ON'")
	t.Cleanup(func() { inst.Exec(t, "SET GLOBAL general_log='OFF'") })
	st = probe.ProbeMySQL(ctx, runner, ep, probe.Creds{MySQLUser: user, MySQLPassword: pw}, 0)
	if !st.Reachable || !st.Authenticated || st.Version != wantVer || st.Error != "" {
		t.Fatalf("有凭据: %+v", st)
	}
	// 只读证明：general_log 里该用户执行过的全部语句
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
		} else if ct != "Connect" && ct != "Quit" && ct != "Prepare" && ct != "Execute" && ct != "Close stmt" {
			t.Errorf("探测用户执行了意外的命令类型 %s: %s", ct, arg)
		}
	}
	// 客户端可能在 SELECT VERSION() 之外只多发 `select @@version_comment limit 1`（同样只读）；任何非 SELECT 语句都是失败
	sawVersion := false
	for _, c := range cmds {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c)), "SELECT") {
			t.Errorf("探测用户执行了非 SELECT 语句: %q", c)
		}
		if c == "SELECT VERSION()" {
			sawVersion = true
		}
	}
	if !sawVersion || len(cmds) > 2 {
		t.Fatalf("探测期间该用户执行的语句应是 SELECT VERSION()（至多外加一条只读 select），得到 %q", cmds)
	}

	// 错误口令：认证失败，但免凭据握手仍给出可达性与版本；口令不泄露
	wrong := pw + "-wrong"
	st = probe.ProbeMySQL(ctx, runner, ep, probe.Creds{MySQLUser: user, MySQLPassword: wrong}, 0)
	if !st.Reachable || st.Authenticated || !st.AuthRequired || st.Version != wantVer || !strings.Contains(st.Error, "Access denied") {
		t.Fatalf("错误口令: %+v", st)
	}
	for _, p := range []string{pw, wrong} {
		if b, _ := json.Marshal(st); strings.Contains(string(b), p) {
			t.Fatalf("口令出现在结果里: %s", b)
		}
	}

	// 不可达 / 非 MySQL / 沉默服务器（超时）
	if st := probe.ProbeMySQL(ctx, runner, probe.Endpoint{Host: "127.0.0.1", Port: closedPort(t)}, probe.Creds{}, time.Second); st.Reachable || st.Error == "" {
		t.Fatalf("不可达: %+v", st)
	}
	httpish := listenAnd(t, func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")) })
	if st := probe.ProbeMySQL(ctx, runner, httpish, probe.Creds{}, time.Second); st.Reachable {
		t.Fatalf("非 MySQL 服务不应判为可达: %+v", st)
	}
	silent := listenAnd(t, func(c net.Conn) { time.Sleep(3 * time.Second) })
	start := time.Now()
	if st := probe.ProbeMySQL(ctx, runner, silent, probe.Creds{}, 500*time.Millisecond); st.Reachable || time.Since(start) > 2*time.Second {
		t.Fatalf("沉默服务器应在超时内失败: %+v 用时 %v", st, time.Since(start))
	}
}
