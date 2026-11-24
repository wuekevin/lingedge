package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/term"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 默认路径（tools-design §3、§8）。
const (
	DefaultConfigPath = "/etc/lingedge/tools/tools.yaml"
	DefaultLogPath    = "/var/log/lingedge/tools.log"
	DefaultStatePath  = "/var/lib/lingedge/tools/state.json"
	// DefaultTrustDir 是信任域①的本地状态目录（tools-design §9.1/§9.2）：keys.d/（密钥轮换落盘）与 state（各通道已见最大 sequence），root 0700。
	DefaultTrustDir = "/var/lib/lingedge/tools/trust"
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
	// TrustDir 是信任库目录（缺省 DefaultTrustDir）；BuiltinKeys 返回内置发行公钥 keys.json 的字节（缺省 manifest.BuiltinKeys，
	// 未注入公钥的构建返回 no_builtin_keys 错误，验证一律失败关闭）。install/update（Story 1.33/1.43）经它们构造 manifest.Verifier。
	TrustDir    string
	BuiltinKeys func() ([]byte, error)
	// State 缺省为读取 StatePath 的 FileState（见 state.go）。
	State StateSource
	// Backup 是备份/恢复/卸载外部依赖的装配；零值 = 生产（真实 mysqldump/mysql 与 lingedge-server 经 safeexec，终端口令输入）。
	Backup BackupEnv
	// Install 是安装编排（install/recover）的外部依赖装配；零值 = 生产：真实系统、真实探测、installer.ProductionSpecs()（当前只有 server，
	// 所以 install 对尚未交付规格的其他组件以明确的错误失败）。测试注入临时根目录、伪二进制 Runner 与（只存在于 _test.go 的）假组件。
	Install InstallEnv
	// Ops 是运维命令（doctor/config/service/logs/status/list）的外部依赖装配；零值 = 生产（见 ops.go）。
	Ops OpsEnv
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
		TrustDir:       DefaultTrustDir,
		BuiltinKeys:    manifest.BuiltinKeys,
		Backup:         BackupEnv{ReadSecret: readSecretFromTerminal},
	}
}

// readSecretFromTerminal 在 stderr 提示并从终端无回显读取一行（仅在交互时被调用）。
func readSecretFromTerminal(label string) (string, error) {
	_, _ = fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	return string(b), nil
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
	if e.TrustDir == "" {
		// 信任库位于状态目录下（registry.Layout.TrustDir）：缺省随 StatePath 推出（生产即 DefaultTrustDir），使 installer 的 Layout 与 Verifier 指向同一处。
		e.TrustDir = filepath.Join(filepath.Dir(e.StatePath), "trust")
	}
	if e.BuiltinKeys == nil {
		e.BuiltinKeys = manifest.BuiltinKeys
	}
	return e
}
