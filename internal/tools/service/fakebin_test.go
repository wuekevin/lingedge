package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 伪二进制（仅用于验证「argv 接线」，不验证 systemd/useradd 的真实行为——后者属 ENV-LNX）：
// 在临时目录放几个 /bin/sh 脚本冒充 systemctl/id/getent/useradd/userdel/systemd-analyze，把每次调用的 argv 逐个写入日志，
// 并用状态目录里的文件模拟「单元已启用/已运行/用户存在」。它们经由真实的 safeexec.Runner（白名单、固定目录解析、清洗环境）被执行，
// 因此被测的是本包构造的参数数组如何穿过 safeexec 到达进程，而不是一个 Runner 的 mock。
type fakeEnv struct {
	Dir   string // 放伪二进制的目录（作为 SearchDirs）
	State string // 状态目录
	Log   string // argv 日志
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	e := &fakeEnv{Dir: filepath.Join(root, "bin"), State: filepath.Join(root, "state"), Log: filepath.Join(root, "argv.log")}
	for _, d := range []string{e.Dir, e.State, filepath.Join(e.State, "users"), filepath.Join(e.State, "groups")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(e.Log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pre := "#!/bin/sh\nLOG='" + e.Log + "'\nST='" + e.State + "'\nprintf '%s' \"$(basename \"$0\")\" >> \"$LOG\"\nfor a in \"$@\"; do printf ' [%s]' \"$a\" >> \"$LOG\"; done\nprintf '\\n' >> \"$LOG\"\n"
	e.script(t, "systemctl", pre+`
case "$1" in
  --version) cat "$ST/version" 2>/dev/null || echo "systemd 252 (252.22-1~deb12u1)";;
  is-enabled) if [ -e "$ST/enabled-$2" ]; then echo enabled; exit 0; else echo disabled; exit 1; fi;;
  is-active) if [ -e "$ST/active-$2" ]; then echo active; exit 0; else echo inactive; exit 3; fi;;
  enable) [ -e "$ST/fail-enable" ] && { echo "Failed to enable unit: boom" >&2; exit 1; }; shift; for u in "$@"; do : > "$ST/enabled-$u"; done;;
  disable) shift; for u in "$@"; do rm -f "$ST/enabled-$u"; done;;
  start) [ -e "$ST/fail-start" ] && { echo "Job failed" >&2; exit 1; }; shift; for u in "$@"; do : > "$ST/active-$u"; done;;
  stop) shift; for u in "$@"; do rm -f "$ST/active-$u"; done;;
  restart|daemon-reload) ;;
  *) echo "unknown verb $1" >&2; exit 2;;
esac
`)
	e.script(t, "systemd-analyze", pre+`
[ -e "$ST/verify-fail" ] && { echo "lingedge-x.service: Unknown key name 'Foo' in section 'Service', ignoring." >&2; exit 1; }
exit 0
`)
	e.script(t, "id", pre+`
[ "$1" = "-u" ] || exit 9
if [ -e "$ST/users/$2" ]; then cat "$ST/users/$2"; exit 0; fi
echo "id: '$2': no such user" >&2; exit 1
`)
	e.script(t, "getent", pre+`
[ "$1" = "group" ] || exit 9
if [ -e "$ST/groups/$2" ]; then echo "$2:x:998:"; exit 0; fi
exit 2
`)
	e.script(t, "useradd", pre+`
for last; do :; done
[ -e "$ST/fail-useradd" ] && { echo "useradd: cannot lock /etc/passwd" >&2; exit 1; }
echo 998 > "$ST/users/$last"
`)
	e.script(t, "userdel", pre+`
for last; do :; done
rm -f "$ST/users/$last"
`)
	return e
}

func (e *fakeEnv) script(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.Dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (e *fakeEnv) set(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(e.State, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *fakeEnv) has(rel string) bool {
	_, err := os.Stat(filepath.Join(e.State, rel))
	return err == nil
}

// calls 返回 argv 日志的每一行，形如 `systemctl [enable] [lingedge-node.service]`。
func (e *fakeEnv) calls(t *testing.T) []string {
	t.Helper()
	b := readFile(t, e.Log)
	if strings.TrimSpace(b) == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(b, "\n"), "\n")
}

func (e *fakeEnv) runner(t *testing.T) Runner {
	t.Helper()
	r, err := NewRunner([]string{e.Dir})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *fakeEnv) systemctl(t *testing.T) *Systemctl {
	return &Systemctl{Run: e.runner(t), Booted: func() bool { return true }}
}

func (e *fakeEnv) provisioner(t *testing.T, euid int) *Provisioner {
	return &Provisioner{Run: e.runner(t), Geteuid: func() int { return euid }}
}
