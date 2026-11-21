package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// realBinary 找本机真实的程序；找不到则跳过（标注等待环境验证）。
func realBinary(t *testing.T, names ...string) string {
	t.Helper()
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	for _, p := range []string{"/opt/homebrew/opt/mysql@8.0/bin/" + names[0]} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skipf("等待环境验证: 本机缺少 %s", names[0])
	return ""
}

func runnerFor(t *testing.T, paths ...string) *safeexec.Runner {
	t.Helper()
	var dirs []string
	seen := map[string]bool{}
	for _, p := range paths {
		d := filepath.Dir(p)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	r, err := NewRunner(dirs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func realOutput(t *testing.T, bin string, arg string) string {
	t.Helper()
	b, err := exec.Command(bin, arg).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v", bin, arg, err)
	}
	return string(b)
}

// 真实二进制：经 safeexec 执行 -v/--version 并解析，结果与直接运行同一程序的输出所含版本一致。
func TestVersionExecRealBinaries(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		kind, name, arg string
		parse           func(string) string
	}{
		{"nginx", "nginx", "-v", func(s string) string { v, _ := ParseNginxV(s); return v }},
		{"mysqld", "mysqld", "--version", func(s string) string { v, _ := ParseMysqlVersion(s); return v }},
		{"redis-server", "redis-server", "--version", ParseRedisServerVersion},
		{"node", "node", "--version", func(s string) string { return strings.TrimPrefix(ParseNodeVersion(s), "v") }},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			bin := realBinary(t, c.name)
			want := c.parse(realOutput(t, bin, c.arg))
			if want == "" {
				t.Fatalf("真实输出无法解析: %q", realOutput(t, bin, c.arg))
			}
			// 用绝对路径（基名在白名单内）与裸名（固定目录解析）两种方式
			for _, p := range []string{bin, filepath.Base(bin)} {
				v, _, err := VersionExec{Runner: runnerFor(t, bin)}.Version(ctx, c.kind, p)
				if err != nil {
					t.Fatalf("%s: %v", p, err)
				}
				if strings.TrimPrefix(v, "v") != want {
					t.Errorf("%s: 版本 %q，期望 %q", p, v, want)
				}
			}
		})
	}
}

func TestReadOnlyArgvGuard(t *testing.T) {
	ctx := context.Background()
	r, err := NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := [][]string{
		{"systemctl", "stop", "nginx"}, {"systemctl", "restart", "nginx"}, {"systemctl", "list-units"}, {"systemctl", "daemon-reload"},
		{"systemctl", "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain", "--now"},
		{"ss", "-K"}, {"ss", "-H", "-lntup", "-K"}, {"ss", "-lntup"}, {"nginx", "-s", "stop"}, {"nginx", "-t"}, {"nginx"}, {"nginx", "-v", "-s", "quit"},
		{"mysqld", "--initialize"}, {"mysqld"}, {"redis-server", "--port", "1"}, {"node", "-e", "1"}, {"node"},
	}
	for _, f := range forbidden {
		_, _, err := readOnlyRun(ctx, r, f[0], f[1:])
		if !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("%v 应被只读表拒绝，得到 %v", f, err)
		}
	}
	// 不在 safeexec 白名单的程序：safeexec 本身拒绝（即便 argv 在表里也没有这一项）
	if _, err := r.Run(ctx, safeexec.Cmd{Path: "sh", Args: []string{"-c", "true"}}); !errors.Is(err, safeexec.ErrNotAllowed) {
		t.Errorf("sh 应不在白名单: %v", err)
	}
	if node, err := exec.LookPath("node"); err == nil { // 环境变量检查在可执行文件解析之后，需要一个真实存在的白名单程序
		if _, err := runnerFor(t, node).Run(ctx, safeexec.Cmd{Path: "node", Args: []string{"--version"}, Env: []string{"LD_PRELOAD=/x"}}); !errors.Is(err, safeexec.ErrBadEnv) {
			t.Errorf("加载器注入类环境变量应被拒绝: %v", err)
		}
	}
	if _, _, err := (VersionExec{Runner: r}).Version(ctx, "bash", "bash"); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("未知 kind: %v", err)
	}
	// 每一条只读 argv 都不含 shell 元字符与可写标志
	for name, lists := range readOnlyArgv {
		for _, a := range lists {
			for _, x := range a {
				if strings.ContainsAny(x, ";|&$`<> ") {
					t.Errorf("%s 的只读 argv 含 shell 元字符: %q", name, x)
				}
			}
		}
	}
}

func TestParseVersions(t *testing.T) {
	if v, f := ParseNginxV(string(fixture(t, "real/nginx-v.stderr.txt"))); v != "1.31.8" || f != "nginx" {
		t.Errorf("真实 nginx -v: %q %q", v, f)
	}
	nginx := map[string][2]string{
		"nginx version: nginx/1.24.0 (Ubuntu)\n":                {"1.24.0", "nginx"},
		"nginx version: openresty/1.21.4.1\n":                   {"1.21.4.1", "openresty"},
		"nginx version: nginx/1.18.0\nbuilt with OpenSSL 3.0\n": {"1.18.0", "nginx"},
		"nginx version: Tengine/2.3.3":                          {"", ""}, // 大小写敏感：未识别的发行版不猜
		"nginx: [alert] could not open error log file\n":        {"", ""},
		"": {"", ""},
	}
	for in, w := range nginx {
		if v, f := ParseNginxV(in); v != w[0] || f != w[1] {
			t.Errorf("%q → %q %q，期望 %v", in, v, f, w)
		}
	}
	my := map[string][2]string{
		string(fixture(t, "real/mysqld-version.txt")):                                                             {"8.0.46", "mysql"},
		string(fixture(t, "real/mysql-client-version.txt")):                                                       {"8.0.46", "mysql"},
		"/usr/sbin/mysqld  Ver 8.0.36-0ubuntu0.22.04.1 for Linux on x86_64 ((Ubuntu))\n":                          {"8.0.36-0ubuntu0.22.04.1", "mysql"},
		"mariadbd  Ver 10.11.6-MariaDB-0+deb12u1 for debian-linux-gnu on x86_64 (Debian 12)\n":                    {"10.11.6-MariaDB-0+deb12u1", "mariadb"},
		"mysql  Ver 15.1 Distrib 10.5.23-MariaDB, for debian-linux-gnu (x86_64) using  EditLine wrapper\n":        {"10.5.23-MariaDB", "mariadb"},
		"/www/server/mysql/bin/mysqld  Ver 5.7.44 for linux-glibc2.12 on x86_64 (MySQL Community Server (GPL))\n": {"5.7.44", "mysql"},
		"command not found": {"", ""},
	}
	for in, w := range my {
		if v, f := ParseMysqlVersion(in); v != w[0] || (v != "" && f != w[1]) {
			t.Errorf("%q → %q %q，期望 %v", in, v, f, w)
		}
	}
	if v := ParseNodeVersion(string(fixture(t, "real/node-version.txt"))); v != "v26.8.2" {
		t.Errorf("node=%q", v)
	}
	for in, w := range map[string]string{"v20.11.0\n": "v20.11.0", "v18.19.1-nightly\n": "v18.19.1-nightly", "20.11.0": "", "node: not found": ""} {
		if v := ParseNodeVersion(in); v != w {
			t.Errorf("%q → %q", in, v)
		}
	}
}
