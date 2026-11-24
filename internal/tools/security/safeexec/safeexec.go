// Package safeexec 是 LingEdge 中唯一允许创建外部进程的位置（G-12、NFR-9、FR-9、tools-design §18.2）。
//
// 约束（结构性的，而不是靠调用方自觉）：
//   - 只接受参数数组：没有「命令串」入口，不经任何 shell，因此 shell 元字符只是普通字面量；
//   - 可执行文件白名单 + 绝对路径解析：Path 要么是白名单里的裸名（在固定目录集合内解析），要么是基名在白名单内的绝对路径；
//     解析结果必须是他人不可写的普通可执行文件；
//   - 环境清洗：不继承父进程环境，固定 PATH 与 LC_ALL=C，仅透传校验过的调用方变量（拒绝 LD_*/DYLD_*/IFS/BASH_ENV 等）；
//   - 输出有上限、受 context 取消与超时约束。
//
// 本包受 `gates shell` 检查（命令名不得拼接、不得是 shell 解释器）；forbidigo 对 os/exec 的豁免只给 internal/tools/security/ 目录。
package safeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 错误类别（errors.Is 可判定）。
var (
	ErrNotAllowed = errors.New("safeexec: 可执行文件不在白名单")
	ErrNotFound   = errors.New("safeexec: 找不到可执行文件")
	ErrUnsafeExe  = errors.New("safeexec: 可执行文件不安全")
	ErrBadArg     = errors.New("safeexec: 参数不合法")
	ErrBadEnv     = errors.New("safeexec: 环境变量不合法")
)

// DefaultAllowed 是 tools-design §18.2 列出的白名单（chown/chmod 用 Go 系统调用替代，因此不在其中）。
var DefaultAllowed = []string{
	"systemctl", "systemd-analyze", "journalctl", "useradd", "userdel", "groupadd", "id", "ss",
	"mysqldump", "mysql", "nginx", "openresty",
}

// DefaultSearchDirs 是裸名的固定解析目录（顺序即优先级）；不读取调用方的 PATH。
var DefaultSearchDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// fixedPath 是子进程看到的 PATH。
const fixedPath = "/usr/sbin:/usr/bin:/sbin:/bin"

const (
	maxArgs        = 1024
	maxArgLen      = 128 << 10
	defaultMaxOut  = 1 << 20
	defaultTimeout = 5 * time.Minute
)

// Cmd 描述一次外部命令调用。
type Cmd struct {
	// Path 是白名单内的裸名（如 "systemctl"）或基名在白名单内的绝对路径。
	Path string
	// Args 不含 argv[0]；每个元素原样作为一个参数传给程序，不做任何 shell 解释。
	Args []string
	// Env 是额外的 KEY=VALUE 变量（可选）；PATH/LC_ALL/LANG 与加载器注入类变量被拒绝。
	Env []string
	// Stdin 可为 nil（等价于空输入）。
	Stdin io.Reader
	// Dir 是工作目录；空表示 "/"，否则必须是绝对路径。
	Dir string
}

// Result 是一次调用的结果。
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
	Truncated      bool // 输出超过上限被截断
}

// Runner 持有白名单与搜索目录。请用 New 或 Run（默认白名单）。
type Runner struct {
	allowed   map[string]bool
	dirs      []string
	maxOutput int
	timeout   time.Duration
}

// Options 配置 Runner。
type Options struct {
	Allowed    []string      // 白名单裸名；空表示 DefaultAllowed
	SearchDirs []string      // 空表示 DefaultSearchDirs（均须为规范的绝对路径）
	MaxOutput  int           // 每路输出上限；0 表示 1 MiB
	Timeout    time.Duration // 无 deadline 的 context 使用的默认超时；0 表示 5 分钟
}

// New 构造 Runner。白名单项必须是不含路径分隔符与空白的裸名，且不得是 shell 解释器或「转调」类程序。
func New(o Options) (*Runner, error) {
	names := o.Allowed
	if len(names) == 0 {
		names = DefaultAllowed
	}
	dirs := o.SearchDirs
	if len(dirs) == 0 {
		dirs = DefaultSearchDirs
	}
	r := &Runner{allowed: map[string]bool{}, maxOutput: o.MaxOutput, timeout: o.Timeout}
	if r.maxOutput <= 0 {
		r.maxOutput = defaultMaxOut
	}
	if r.timeout <= 0 {
		r.timeout = defaultTimeout
	}
	for _, n := range names {
		if !reBaseName.MatchString(n) || isShell(n) {
			return nil, fmt.Errorf("safeexec: 白名单项 %q 非法", n)
		}
		r.allowed[n] = true
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d {
			return nil, fmt.Errorf("safeexec: 搜索目录 %q 必须是规范的绝对路径", d)
		}
		r.dirs = append(r.dirs, d)
	}
	return r, nil
}

var defaultRunner = func() *Runner {
	r, err := New(Options{})
	if err != nil {
		panic(err) // 常量配置，不可能失败
	}
	return r
}()

// Run 用默认白名单执行 c。
func Run(ctx context.Context, c Cmd) (Result, error) { return defaultRunner.Run(ctx, c) }

var (
	reBaseName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	reEnvKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	shells     = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true, "tcsh": true, "fish": true, "ash": true, "busybox": true, "env": true, "xargs": true, "eval": true, "exec": true, "su": true, "sudo": true, "cmd": true, "cmd.exe": true, "powershell": true, "pwsh": true}
)

func isShell(n string) bool { return shells[strings.ToLower(n)] }

// Resolve 解析 p 为经过白名单与安全检查的绝对路径（不执行）。
func (r *Runner) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrNotAllowed)
	}
	var cand string
	if strings.ContainsRune(p, '/') {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return "", fmt.Errorf("%w: %q 必须是规范的绝对路径", ErrNotAllowed, security.RedactString(p))
		}
		base := filepath.Base(p)
		if !r.allowed[base] {
			return "", fmt.Errorf("%w: %q", ErrNotAllowed, base)
		}
		cand = p
	} else {
		if !r.allowed[p] {
			return "", fmt.Errorf("%w: %q", ErrNotAllowed, p)
		}
		for _, d := range r.dirs {
			c := filepath.Join(d, p)
			if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
				cand = c
				break
			}
		}
		if cand == "" {
			return "", fmt.Errorf("%w: %q（在 %s 中）", ErrNotFound, p, strings.Join(r.dirs, ":"))
		}
	}
	real, err := filepath.EvalSymlinks(cand)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s 不是普通文件", ErrUnsafeExe, cand)
	}
	if st.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s 不可执行", ErrUnsafeExe, cand)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%w: %s 对 group/other 可写", ErrUnsafeExe, cand)
	}
	if st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return "", fmt.Errorf("%w: %s 带 setuid/setgid 位", ErrUnsafeExe, cand)
	}
	return cand, nil
}

func buildEnv(extra []string) ([]string, error) {
	env := []string{"PATH=" + fixedPath, "LC_ALL=C", "LANG=C"}
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !reEnvKey.MatchString(k) {
			return nil, fmt.Errorf("%w: 键名非法", ErrBadEnv)
		}
		if strings.ContainsRune(kv, 0) {
			return nil, fmt.Errorf("%w: 含 NUL", ErrBadEnv)
		}
		up := strings.ToUpper(k)
		switch {
		case up == "PATH", up == "LC_ALL", up == "LANG", up == "IFS", up == "ENV", up == "BASH_ENV", up == "SHELLOPTS", up == "PS4", up == "PROMPT_COMMAND",
			strings.HasPrefix(up, "LD_"), strings.HasPrefix(up, "DYLD_"), strings.HasPrefix(up, "BASH_FUNC_"):
			return nil, fmt.Errorf("%w: 不允许设置 %s", ErrBadEnv, k)
		}
		env = append(env, kv)
	}
	return env, nil
}

// prepared 是经过全部校验的调用：白名单与路径解析、参数检查、环境清洗、工作目录。
type prepared struct {
	exe string
	env []string
	dir string
}

func (r *Runner) prepare(c Cmd) (prepared, error) {
	var p prepared
	if len(c.Args) > maxArgs {
		return p, fmt.Errorf("%w: 参数过多", ErrBadArg)
	}
	for i, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return p, fmt.Errorf("%w: 第 %d 个参数含 NUL", ErrBadArg, i)
		}
		if len(a) > maxArgLen {
			return p, fmt.Errorf("%w: 第 %d 个参数过长", ErrBadArg, i)
		}
	}
	exe, err := r.Resolve(c.Path)
	if err != nil {
		return p, err
	}
	env, err := buildEnv(c.Env)
	if err != nil {
		return p, err
	}
	dir := c.Dir
	if dir == "" {
		dir = "/"
	} else if !filepath.IsAbs(dir) {
		return p, fmt.Errorf("%w: 工作目录必须是绝对路径", ErrBadArg)
	}
	return prepared{exe: exe, env: env, dir: dir}, nil
}

// Run 执行 c。退出码非 0 时返回 (Result, *ExitError)；其余失败（白名单、解析、启动、超时）返回对应错误。
func (r *Runner) Run(ctx context.Context, c Cmd) (Result, error) {
	var res Result
	p, err := r.prepare(c)
	if err != nil {
		return res, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, p.exe, c.Args...)
	cmd.Env = p.env // 非 nil：不继承父进程环境
	cmd.Dir = p.dir
	cmd.Stdin = c.Stdin
	so, se := &limitedBuffer{max: r.maxOutput}, &limitedBuffer{max: r.maxOutput}
	cmd.Stdout, cmd.Stderr = so, se
	cmd.WaitDelay = 3 * time.Second
	applyPlatform(cmd)

	err = cmd.Run()
	res = Result{Stdout: so.buf.Bytes(), Stderr: se.buf.Bytes(), Truncated: so.truncated || se.truncated}
	exe := p.exe
	if cerr := ctx.Err(); cerr != nil {
		res.ExitCode = -1
		return res, fmt.Errorf("safeexec: %s 被中止: %w", filepath.Base(exe), cerr)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, &ExitError{Exe: filepath.Base(exe), Args: security.RedactArgs(c.Args), Code: res.ExitCode, Stderr: security.RedactString(tail(res.Stderr, 512))}
		}
		res.ExitCode = -1
		return res, fmt.Errorf("safeexec: 启动 %s 失败: %w", filepath.Base(exe), security.RedactError(err))
	}
	return res, nil
}

// Stream 与 Run 共用同一套白名单、解析、参数与环境检查，但把 stdout 实时写给 stdout（不缓冲、不设输出上限），stderr 写给 stderr
// （nil 时只保留尾部用于错误信息）。用于 `logs -f` 的 journalctl 跟随与 `config edit` 的交互式编辑器：这两者的输出不是「一次性结果」。
// 调用方负责经 writer 脱敏。ctx 取消即终止子进程（独立进程组，Linux 上父进程死亡由内核补发 SIGKILL）；
// 没有 deadline 的 ctx 不加默认超时（跟随/编辑都可能长时间运行），需要上限的调用方自带 deadline。
func (r *Runner) Stream(ctx context.Context, c Cmd, stdout, stderr io.Writer) error {
	p, err := r.prepare(c)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, p.exe, c.Args...)
	cmd.Env = p.env
	cmd.Dir = p.dir
	cmd.Stdin = c.Stdin
	cmd.Stdout = stdout
	se := &limitedBuffer{max: r.maxOutput}
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, se)
	} else {
		cmd.Stderr = se
	}
	cmd.WaitDelay = 3 * time.Second
	applyPlatform(cmd)
	err = cmd.Run()
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("safeexec: %s 被中止: %w", filepath.Base(p.exe), cerr)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &ExitError{Exe: filepath.Base(p.exe), Args: security.RedactArgs(c.Args), Code: ee.ExitCode(), Stderr: security.RedactString(tail(se.buf.Bytes(), 512))}
		}
		return fmt.Errorf("safeexec: 启动 %s 失败: %w", filepath.Base(p.exe), security.RedactError(err))
	}
	return nil
}

// ExitError 表示命令以非 0 退出；Args 与 Stderr 已脱敏，可安全写入日志。
type ExitError struct {
	Exe    string
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("safeexec: %s 退出码 %d（参数 %q）：%s", e.Exe, e.Code, e.Args, e.Stderr)
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

// limitedBuffer 只保留前 max 字节，其余丢弃但继续消费（避免子进程因管道写满而阻塞）。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	room := l.max - l.buf.Len()
	switch {
	case len(p) <= room:
		l.buf.Write(p)
	case room > 0:
		l.buf.Write(p[:room])
		l.truncated = true
	default:
		l.truncated = true
	}
	return len(p), nil
}
