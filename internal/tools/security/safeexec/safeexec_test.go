package safeexec

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 真实可用的系统程序（macOS 与 Linux 均有）。测试用白名单只含这些，不改变生产默认白名单。
func realRunner(t *testing.T, names ...string) *Runner {
	t.Helper()
	r, err := New(Options{Allowed: names, SearchDirs: []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func need(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("本机缺少 %s", name)
	}
}

func TestRunPassesArgumentsLiterally(t *testing.T) {
	need(t, "printf")
	r := realRunner(t, "printf")
	sentinel := filepath.Join(t.TempDir(), "pwned")
	vectors := []string{
		"; touch " + sentinel, "&& touch " + sentinel, "| touch " + sentinel, "$(touch " + sentinel + ")", "`touch " + sentinel + "`",
		"\ntouch " + sentinel, "'; touch " + sentinel + "; '", "\"; touch " + sentinel + "; \"", "${IFS}touch${IFS}" + sentinel,
		"> " + sentinel, "< /etc/passwd", "*", "~", "$HOME", "-rf /", "--help", "%s%s%n", "a b  c", "", "\\", "é x",
	}
	for _, v := range vectors {
		// printf 的格式串用固定的 "%s|"，向量只作为参数出现，输出必须与向量逐字节相同。
		res, err := r.Run(context.Background(), Cmd{Path: "printf", Args: []string{"%s|", v}})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if string(res.Stdout) != v+"|" {
			t.Errorf("%q 被改写为 %q", v, res.Stdout)
		}
	}
	if _, err := os.Lstat(sentinel); err == nil {
		t.Fatal("元字符被执行了（哨兵文件被创建）")
	}
}

// 以真实的 id 命令（默认白名单内）验证：元字符作为用户名参数，只会让 id 报「无此用户」，不会被执行。
func TestDefaultRunnerMetacharactersInert(t *testing.T) {
	need(t, "id")
	sentinel := filepath.Join(t.TempDir(), "pwned")
	for _, v := range []string{"; touch " + sentinel, "$(touch " + sentinel + ")", "`touch " + sentinel + "`", "x\ntouch " + sentinel} {
		_, err := Run(context.Background(), Cmd{Path: "id", Args: []string{v}})
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code == 0 {
			t.Fatalf("%q 应以非零退出: %v", v, err)
		}
	}
	if _, err := os.Lstat(sentinel); err == nil {
		t.Fatal("元字符被执行了")
	}
}

func TestAllowlist(t *testing.T) {
	need(t, "printf")
	r := realRunner(t, "printf")
	for _, p := range []string{"sh", "bash", "/bin/sh", "/bin/bash", "ls", "/bin/ls", "/usr/bin/env", "cat", "", "printf2", "PRINTF"} {
		if _, err := r.Run(context.Background(), Cmd{Path: p, Args: []string{"-c", "true"}}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q 应 ErrNotAllowed: %v", p, err)
		}
	}
	for _, p := range []string{"./printf", "../bin/printf", "bin/printf", "/usr/bin/../bin/printf", "/usr//bin/printf", "/usr/bin/./printf", "/tmp/x/printf/"} {
		if _, err := r.Resolve(p); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q 应 ErrNotAllowed: %v", p, err)
		}
	}
	// 白名单内的名字但不在固定目录里 → ErrNotFound（不读调用方 PATH）。
	t.Setenv("PATH", t.TempDir()+":"+os.Getenv("PATH"))
	r2, err := New(Options{Allowed: []string{"nonexistent-tool"}, SearchDirs: []string{"/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Run(context.Background(), Cmd{Path: "nonexistent-tool"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestNewRejectsDangerousAllowlist(t *testing.T) {
	for _, n := range []string{"sh", "bash", "env", "xargs", "sudo", "su", "powershell", "a b", "a/b", "../x", "-x", "x;y", ""} {
		if _, err := New(Options{Allowed: []string{"id", n}}); err == nil && n != "" {
			t.Errorf("白名单项 %q 应被拒绝", n)
		}
	}
	if _, err := New(Options{SearchDirs: []string{"relative"}}); err == nil {
		t.Error("相对搜索目录应被拒绝")
	}
	if _, err := New(Options{SearchDirs: []string{"/usr/../bin"}}); err == nil {
		t.Error("非规范搜索目录应被拒绝")
	}
	if len(DefaultAllowed) == 0 {
		t.Fatal("默认白名单为空")
	}
	for _, n := range DefaultAllowed {
		if isShell(n) || !reBaseName.MatchString(n) {
			t.Errorf("默认白名单含危险项 %q", n)
		}
	}
}

func TestUnsafeExecutableRejected(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r, err := New(Options{Allowed: []string{"okbin", "wbin", "xbin", "suid", "dirbin"}, SearchDirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	mk("okbin", 0o755)
	mk("wbin", 0o777)
	mk("xbin", 0o644)
	mk("suid", os.ModeSetuid|0o755)
	if err := os.Mkdir(filepath.Join(dir, "dirbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("okbin"); err != nil {
		t.Fatalf("正常文件应通过: %v", err)
	}
	for _, n := range []string{"wbin", "xbin", "suid"} {
		if _, err := r.Resolve(n); !errors.Is(err, ErrUnsafeExe) {
			t.Errorf("%s 应 ErrUnsafeExe: %v", n, err)
		}
	}
	if _, err := r.Resolve("dirbin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("目录应不被当作可执行文件: %v", err)
	}
	// 绝对路径形式同样适用。
	if _, err := r.Resolve(filepath.Join(dir, "wbin")); !errors.Is(err, ErrUnsafeExe) {
		t.Errorf("绝对路径的可写文件应被拒绝: %v", err)
	}
	// 软链接：解析后的真实文件才做安全检查。
	if err := os.Symlink(filepath.Join(dir, "wbin"), filepath.Join(dir, "okbin2")); err != nil {
		t.Fatal(err)
	}
	r3, _ := New(Options{Allowed: []string{"okbin2"}, SearchDirs: []string{dir}})
	if _, err := r3.Resolve("okbin2"); !errors.Is(err, ErrUnsafeExe) {
		t.Errorf("软链接到可写文件应被拒绝: %v", err)
	}
}

func TestEnvironmentSanitized(t *testing.T) {
	need(t, "env")
	// 用 env 打印子进程环境；env 不在默认白名单（且不得进入白名单），这里用测试专用的 /usr/bin/env 副本无法绕过 New 的检查，
	// 因此改用 printenv 类程序：Linux/macOS 均有 /usr/bin/printenv。
	need(t, "printenv")
	r := realRunner(t, "printenv")
	t.Setenv("SECRET_FROM_PARENT", "leak-me")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("BASH_ENV", "/tmp/evil")
	res, err := r.Run(context.Background(), Cmd{Path: "printenv", Env: []string{"EXTRA=1"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.Stdout)
	lines := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		lines[l] = true
	}
	for _, must := range []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "EXTRA=1"} {
		if !lines[must] {
			t.Errorf("缺少 %s；环境:\n%s", must, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "leak-me") || strings.HasPrefix(l, "LD_PRELOAD") || strings.HasPrefix(l, "BASH_ENV") || strings.HasPrefix(l, "SECRET_FROM_PARENT") {
			t.Errorf("父进程环境泄入子进程: %s", l)
		}
	}
	for _, kv := range []string{"PATH=/tmp", "path=/tmp", "LC_ALL=en_US", "LANG=x", "LD_PRELOAD=x", "ld_library_path=x", "DYLD_INSERT_LIBRARIES=x", "IFS=x", "BASH_ENV=x", "ENV=x", "BASH_FUNC_x%%=()", "NOEQUALS", "1BAD=x", "A B=x", "A\x00=x"} {
		if _, err := r.Run(context.Background(), Cmd{Path: "printenv", Env: []string{kv}}); !errors.Is(err, ErrBadEnv) {
			t.Errorf("%q 应 ErrBadEnv: %v", kv, err)
		}
	}
}

func TestBadArgsAndDir(t *testing.T) {
	need(t, "printf")
	r := realRunner(t, "printf")
	if _, err := r.Run(context.Background(), Cmd{Path: "printf", Args: []string{"a\x00b"}}); !errors.Is(err, ErrBadArg) {
		t.Errorf("NUL 参数: %v", err)
	}
	if _, err := r.Run(context.Background(), Cmd{Path: "printf", Args: []string{strings.Repeat("a", maxArgLen+1)}}); !errors.Is(err, ErrBadArg) {
		t.Errorf("超长参数: %v", err)
	}
	if _, err := r.Run(context.Background(), Cmd{Path: "printf", Args: make([]string, maxArgs+1)}); !errors.Is(err, ErrBadArg) {
		t.Errorf("过多参数: %v", err)
	}
	if _, err := r.Run(context.Background(), Cmd{Path: "printf", Dir: "relative"}); !errors.Is(err, ErrBadArg) {
		t.Errorf("相对工作目录: %v", err)
	}
}

func TestStdinExitCodeAndOutputCap(t *testing.T) {
	need(t, "tr")
	r, err := New(Options{Allowed: []string{"tr", "head", "false"}, SearchDirs: []string{"/usr/bin", "/bin"}, MaxOutput: 16})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Cmd{Path: "tr", Args: []string{"a-z", "A-Z"}, Stdin: strings.NewReader("hello")})
	if err != nil || string(res.Stdout) != "HELLO" {
		t.Fatalf("%q %v", res.Stdout, err)
	}
	res, err = r.Run(context.Background(), Cmd{Path: "tr", Args: []string{"a", "b"}, Stdin: strings.NewReader(strings.Repeat("a", 100000))})
	if err != nil || len(res.Stdout) != 16 || !res.Truncated {
		t.Fatalf("输出应被截断到 16 字节: len=%d trunc=%v err=%v", len(res.Stdout), res.Truncated, err)
	}
	if _, err = r.Run(context.Background(), Cmd{Path: "false"}); err == nil {
		t.Fatal("false 应非零退出")
	} else {
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 1 {
			t.Fatalf("%v", err)
		}
	}
}

func TestExitErrorIsRedacted(t *testing.T) {
	need(t, "id")
	leak := "le_abc_" + strings.Repeat("Z9", 16)
	_, err := Run(context.Background(), Cmd{Path: "id", Args: []string{"--tok" + "en", leak, "no-such-user-" + leak}})
	if err == nil {
		t.Fatal("应失败")
	}
	if strings.Contains(err.Error(), leak) {
		t.Fatalf("错误文本泄露令牌: %v", err)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	need(t, "sleep")
	r := realRunner(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, Cmd{Path: "sleep", Args: []string{"30"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("超时没有及时终止子进程")
	}
	// 无 deadline 的 context 使用 Runner 默认超时。
	r2, _ := New(Options{Allowed: []string{"sleep"}, SearchDirs: []string{"/usr/bin", "/bin"}, Timeout: 200 * time.Millisecond})
	if _, err := r2.Run(context.Background(), Cmd{Path: "sleep", Args: []string{"30"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("默认超时未生效: %v", err)
	}
	_ = runtime.GOOS
}

func FuzzRunArgsInert(f *testing.F) {
	for _, s := range []string{"a", "; id", "$(id)", "`id`", "a\nb", "\x00", "-x", "%n"} {
		f.Add(s)
	}
	if _, err := exec.LookPath("printf"); err != nil {
		f.Skip("无 printf")
	}
	r, err := New(Options{Allowed: []string{"printf"}, SearchDirs: []string{"/usr/bin", "/bin"}, Timeout: 10 * time.Second})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, s string) {
		res, err := r.Run(context.Background(), Cmd{Path: "printf", Args: []string{"%s", s}})
		if strings.ContainsRune(s, 0) {
			if !errors.Is(err, ErrBadArg) {
				t.Fatalf("NUL 应被拒绝: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(res.Stdout) != s {
			t.Fatalf("参数被改写: %q -> %q", s, res.Stdout)
		}
	})
}

// Stream：与 Run 同一套白名单/解析/环境清洗，stdout 实时写给调用方的 writer（无输出上限）；取消 ctx 终止子进程。
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestStreamWritesStdoutAndEnforcesSameRules(t *testing.T) {
	need(t, "printf")
	r := realRunner(t, "printf")
	var out syncBuf
	if err := r.Stream(context.Background(), Cmd{Path: "printf", Args: []string{"%s\n", "你好; touch /tmp/x"}}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "你好; touch /tmp/x\n" {
		t.Errorf("stdout：%q", out.String())
	}
	// 白名单与 Run 相同：不在白名单的程序被拒绝，shell 解释器不能进白名单。
	if err := r.Stream(context.Background(), Cmd{Path: "ls"}, &out, nil); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("未列入白名单应被拒绝：%v", err)
	}
	if _, err := New(Options{Allowed: []string{"bash"}}); err == nil {
		t.Error("shell 不得进入白名单")
	}
	if err := r.Stream(context.Background(), Cmd{Path: "printf", Args: []string{"a\x00b"}}, &out, nil); !errors.Is(err, ErrBadArg) {
		t.Errorf("NUL 参数应被拒绝：%v", err)
	}
	if err := r.Stream(context.Background(), Cmd{Path: "printf", Env: []string{"LD_PRELOAD=/x"}}, &out, nil); !errors.Is(err, ErrBadEnv) {
		t.Errorf("加载器注入环境变量应被拒绝：%v", err)
	}
}

func TestStreamExitErrorIsRedactedAndLargeOutputIsNotTruncated(t *testing.T) {
	need(t, "sh")
	dir := t.TempDir()
	script := filepath.Join(dir, "gen")
	body := "#!/bin/sh\ni=0; while [ $i -lt 20000 ]; do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done\necho 'password=hunter2' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{Allowed: []string{"gen"}, SearchDirs: []string{dir}, MaxOutput: 1024})
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	err = r.Stream(context.Background(), Cmd{Path: "gen"}, &out, nil)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("应返回退出码 3：%v", err)
	}
	if strings.Contains(ee.Stderr, "hunter2") {
		t.Errorf("stderr 里的口令必须脱敏：%q", ee.Stderr)
	}
	if n := strings.Count(out.String(), "\n"); n != 20000 {
		t.Errorf("Stream 不应按 MaxOutput 截断 stdout：%d 行", n)
	}
}

func TestStreamStopsOnContextCancel(t *testing.T) {
	need(t, "sleep")
	r := realRunner(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Stream(ctx, Cmd{Path: "sleep", Args: []string{"30"}}, io.Discard, nil) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("应返回 context.Canceled：%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后子进程没有被终止")
	}
}
