package dbmigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// recRunner 记录传给 lingedge-server 的命令行与环境，返回预置输出（这是「操作系统」的接缝；Migrator 的参数/环境构造是被测的生产代码）。
type recRunner struct {
	cmds []safeexec.Cmd
	out  string
	err  error
	code int
}

func (r *recRunner) Run(_ context.Context, c safeexec.Cmd) (safeexec.Result, error) {
	r.cmds = append(r.cmds, c)
	if r.code != 0 {
		return safeexec.Result{ExitCode: r.code}, &safeexec.ExitError{Exe: "lingedge-server", Code: r.code, Stderr: "boom"}
	}
	return safeexec.Result{Stdout: []byte(r.out)}, r.err
}

func conn() *backup.DBConn {
	pw := "p" + "w-" + "s3cret"
	return &backup.DBConn{DBTarget: backup.DBTarget{Host: "127.0.0.1", Port: 3307, Name: "lingedge", User: "lingedge_migrate", TLS: "preferred"}, Password: []byte(pw)}
}

// AC9：口令只经环境变量传给 migrate 直接子进程；命令行里没有口令；连接目标经命令行。
func TestPasswordOnlyViaEnvironmentNeverOnCommandLine(t *testing.T) {
	r := &recRunner{out: `{"mode":"pending","db_version":18,"latest_version":19,"count":1,"migrations":[{"version":19,"name":"x","class":"expand","has_down":false}],"has_irreversible":false,"max_class":"expand"}`}
	m := &Migrator{Run: r}
	c := conn()
	pw := string(c.Password)
	p, err := m.Plan(context.Background(), "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", c)
	if err != nil || p.Count != 1 || *p.DBVersion != 18 || strings.Join(p.Classes(), ",") != "expand" {
		t.Fatalf("%+v %v", p, err)
	}
	cmd := r.cmds[0]
	if strings.Contains(strings.Join(cmd.Args, " "), pw) {
		t.Fatalf("口令不得出现在命令行: %v", cmd.Args)
	}
	found := false
	for _, e := range cmd.Env {
		if e == EnvPassword+"="+pw {
			found = true
		}
	}
	if !found {
		t.Fatalf("口令应经环境变量 %s 传递: %v", EnvPassword, cmd.Env)
	}
	want := []string{"migrate", "plan", "--host", "127.0.0.1", "--port", "3307", "--database", "lingedge", "--user", "lingedge_migrate", "--tls", "preferred"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("参数 %v", cmd.Args)
	}
	// socket 目标；up 带 --accept-irreversible 仅当调用方给出确认。
	r.out = `{"from_version":18,"to_version":19,"applied":[]}`
	c2 := conn()
	c2.Host, c2.Port, c2.Socket = "", 0, "/run/mysqld/mysqld.sock"
	if _, err := m.Up(context.Background(), "/b/lingedge-server", c2, false); err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(r.cmds[1].Args, " "); !strings.Contains(a, "--socket /run/mysqld/mysqld.sock") || strings.Contains(a, "--accept-irreversible") || strings.Contains(a, "--host") {
		t.Fatalf("up: %s", a)
	}
	if _, err := m.Up(context.Background(), "/b/lingedge-server", c2, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.cmds[2].Args, " "), "--accept-irreversible") {
		t.Fatal("确认后应带 --accept-irreversible")
	}
	if _, err := m.Down(context.Background(), "/b/lingedge-server", c2, 18); err != nil || !strings.Contains(strings.Join(r.cmds[3].Args, " "), "--to 18") {
		t.Fatalf("down: %v %v", err, r.cmds[3].Args)
	}
	if _, err := m.Down(context.Background(), "/b/lingedge-server", c2, 0); err == nil {
		t.Fatal("down 目标 <1 必须拒绝（不得退化为回退全部）")
	}
}

func TestErrorsCarryExitCodeAndNoPassword(t *testing.T) {
	r := &recRunner{code: 9}
	m := &Migrator{Run: r}
	_, err := m.Up(context.Background(), "/b/lingedge-server", conn(), false)
	var me *Error
	if !errors.As(err, &me) || me.ExitCode != 9 || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("%v", err)
	}
	if _, err := m.Plan(context.Background(), "relative/bin", conn()); err == nil {
		t.Fatal("相对路径二进制必须拒绝")
	}
	if _, err := m.Plan(context.Background(), "/b/x", nil); err == nil {
		t.Fatal("缺连接必须拒绝")
	}
	bad := conn()
	bad.Host = ""
	if _, err := m.Plan(context.Background(), "/b/x", bad); !errors.Is(err, backup.ErrNoTarget) {
		t.Fatalf("缺目标: %v", err)
	}
}
