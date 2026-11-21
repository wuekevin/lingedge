package cli

import (
	"io"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/term"
)

// 默认路径（tools-design §3、§8）。
const (
	DefaultConfigPath = "/etc/lingedge/tools/tools.yaml"
	DefaultLogPath    = "/var/log/lingedge/tools.log"
	DefaultStatePath  = "/var/lib/lingedge/tools/state.json"
)

// Env 是命令树与外部世界的全部接触面：标准流、终端判定、环境变量、有效 UID、主机名、时钟与状态来源。
// 生产用 DefaultEnv()；测试构造真实的临时目录、缓冲区与固定时钟，不替换任何业务逻辑。
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// 终端判定：stdin 是否为 TTY 决定能否交互；stderr 是否为 TTY 决定是否着色。
	StdinTTY, StderrTTY bool
	Getenv              func(string) string
	Geteuid             func() int
	Hostname            func() (string, error)
	Now                 func() time.Time
	NewRunID            func(t time.Time) string

	StatePath      string
	DefaultLogFile string
	// State 缺省为读取 StatePath 的 FileState（见 state.go）。
	State StateSource
}

// DefaultEnv 返回绑定到真实进程环境的 Env。
func DefaultEnv() Env {
	return Env{
		Stdin:          os.Stdin,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		StdinTTY:       term.IsTerminal(int(os.Stdin.Fd())),
		StderrTTY:      term.IsTerminal(int(os.Stderr.Fd())),
		Getenv:         os.Getenv,
		Geteuid:        os.Geteuid,
		Hostname:       os.Hostname,
		Now:            time.Now,
		NewRunID:       func(t time.Time) string { return ulid.Make().String() },
		StatePath:      DefaultStatePath,
		DefaultLogFile: DefaultLogPath,
	}
}

func (e Env) withDefaults() Env {
	if e.Stdin == nil {
		e.Stdin = os.Stdin
	}
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Getenv == nil {
		e.Getenv = func(string) string { return "" }
	}
	if e.Geteuid == nil {
		e.Geteuid = os.Geteuid
	}
	if e.Hostname == nil {
		e.Hostname = os.Hostname
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.NewRunID == nil {
		e.NewRunID = func(time.Time) string { return ulid.Make().String() }
	}
	if e.StatePath == "" {
		e.StatePath = DefaultStatePath
	}
	if e.State == nil {
		e.State = FileState{Path: e.StatePath}
	}
	return e
}
