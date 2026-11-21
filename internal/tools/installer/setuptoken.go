package installer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
)

// InstallLockName 是 Server 安装向导完成后在其数据目录写下的锁文件名（internal/platform/setup.LockFileName，Story 1.24）。
// Tools 不 import Server 的 internal 包（architecture §5.3 规则 5），这里重复该常量；tests/integration/toolsinstall 用真实向导核对两者一致。
const InstallLockName = "install.lock"

// ResetSetupTokenResult 是 `lingedge config setup-token --reset` 的结果。
type ResetSetupTokenResult struct {
	// Token 是新的 256 位令牌：只在本次输出中出现一次，不落盘、不写日志；磁盘上只有它的 SHA-256。
	Token    string
	HashFile string // 虚拟路径（/etc/lingedge/server/setup-token.hash）
	// Replaced 为 true 表示哈希文件原先存在（旧令牌随之失效）。
	Replaced bool
}

// serverSetupState 读取 server 的安装记录与向导状态。server 未安装 → KindPrecondition；向导已完成（install.lock 存在）→ KindPrecondition。
func (in *Installer) serverSetupState() (runUser, dataDir string, err error) {
	st, _, lerr := in.d.Reg.LoadState()
	if lerr != nil {
		return "", "", lerr
	}
	c, ok := st.Components["server"]
	if !ok {
		return "", "", &Error{Kind: KindPrecondition, Msg: "server 尚未安装，没有安装向导令牌可重置", Hint: "先运行 lingedge install server", SystemState: "unchanged"}
	}
	runUser, dataDir = c.RunUser, c.DataDir
	if runUser == "" {
		runUser = "lingedge"
	}
	if dataDir == "" {
		dataDir = service.ServerDataDir
	}
	if _, serr := os.Lstat(in.d.hostPath(dataDir + "/" + InstallLockName)); serr == nil {
		return "", "", &Error{Kind: KindPrecondition, Msg: "安装向导已完成（" + dataDir + "/" + InstallLockName + " 存在）：向导接口已永久关闭（410），不再需要令牌",
			Hint: "如需重新初始化，请按运维文档先备份再走重装流程；令牌重置不会重新打开已完成的向导", SystemState: "unchanged"}
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return "", "", wrap(KindStepFailed, serr, "无法确认向导是否已完成")
	}
	return runUser, dataDir, nil
}

// ResetSetupToken 生成新的 setup token 并原子替换哈希文件（旧令牌立即失效；Server 每次校验都重读该文件，不需要重启）。
// 哈希文件：lingedge:lingedge 0600，同目录临时文件 → chown → fsync → rename（任一时刻读者看到的都是完整的旧文件或新文件，且属主正确）。
// 调用方（cli）负责持有 server 组件锁与确认；本函数不写 journal（单文件原子替换，失败即原样保留旧文件，无需撤销）。
func (in *Installer) ResetSetupToken(ctx context.Context) (*ResetSetupTokenResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runUser, _, err := in.serverSetupState()
	if err != nil {
		return nil, err
	}
	virtual := setuptoken.DefaultHashPath
	real := in.d.hostPath(virtual)
	dir := filepath.Dir(real)
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		return nil, &Error{Kind: KindPrecondition, Msg: "配置目录 " + filepath.Dir(virtual) + " 不存在：server 的安装不完整", Hint: "运行 lingedge recover，或重新安装 server", SystemState: "unchanged"}
	}
	uid, gid, err := in.d.Own.Resolve(runUser, runUser)
	if err != nil {
		return nil, wrap(KindStepFailed, err, "无法解析运行用户 %s", runUser)
	}
	token, hash, err := setuptoken.Generate()
	if err != nil {
		return nil, wrap(KindStepFailed, err, "生成 setup token 失败")
	}
	_, statErr := os.Lstat(real)
	replaced := statErr == nil

	tmp, err := os.CreateTemp(dir, ".setup-token-*")
	if err != nil {
		return nil, wrap(KindStepFailed, err, "创建临时文件失败")
	}
	name := tmp.Name()
	fail := func(e error, msg string) (*ResetSetupTokenResult, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return nil, wrap(KindStepFailed, e, "%s", msg)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err, "设置权限失败")
	}
	if err := in.d.Own.Chown(name, uid, gid); err != nil {
		return fail(err, "设置属主失败")
	}
	if _, err := tmp.WriteString(hash + "\n"); err != nil {
		return fail(err, "写入失败")
	}
	if err := tmp.Sync(); err != nil {
		return fail(err, "fsync 失败")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return nil, wrap(KindStepFailed, err, "关闭临时文件失败")
	}
	if err := os.Rename(name, real); err != nil {
		_ = os.Remove(name)
		return nil, wrap(KindStepFailed, err, "原子替换哈希文件失败")
	}
	syncDir(dir)
	return &ResetSetupTokenResult{Token: token, HashFile: virtual, Replaced: replaced}, nil
}
