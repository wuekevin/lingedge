//go:build !windows

package migrate

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// checkOwnerRootOrSelf：属主须是 root 或当前运行用户。
func checkOwnerRootOrSelf(st fs.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("无法确定文件属主")
	}
	if sys.Uid != 0 && sys.Uid != uint32(os.Geteuid()) {
		return errors.New("文件属主既不是 root 也不是运行用户")
	}
	return nil
}

// checkRootOwnedConfig：root 运行时，server.yaml 须属主 root 且不被 group/other 写入（它是连接目标的受信来源之一，ADR-026 §4）。
func checkRootOwnedConfig(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return errors.New("无法读取配置文件")
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 {
		return errors.New("以 root 运行时，--config 指向的 server.yaml 必须属主为 root（lingedge 可写的文件不是受信的连接目标来源，ADR-026 §4）")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return errors.New("以 root 运行时，--config 指向的 server.yaml 不得被 group/other 写入")
	}
	return nil
}
