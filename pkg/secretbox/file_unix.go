//go:build unix

package secretbox

import (
	"os"
	"syscall"
)

func defaultAllowedUIDs() []uint32 {
	u := uint32(os.Geteuid())
	if u == 0 {
		return []uint32{0}
	}
	return []uint32{u, 0}
}

// readMasterKeyFile 以 O_NOFOLLOW 打开（拒绝符号链接），在同一个 fd 上校验类型/权限/属主后读取，避免检查与使用之间被替换（TOCTOU）。
func readMasterKeyFile(path string, allowed []uint32) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return nil, ErrMasterKeyMissing
		case os.IsPermission(err):
			return nil, invalid("权限不足，无法读取（运行用户需是属主或属组）")
		default:
			return nil, invalid("无法打开（不得是符号链接）")
		}
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, invalid("无法读取文件状态")
	}
	if err := checkMode(st.Mode()); err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, invalid("无法确定属主")
	}
	owned := false
	for _, u := range allowed {
		if sys.Uid == u {
			owned = true
		}
	}
	if !owned {
		return nil, invalid("属主不正确（uid=%d；应为 root 或运行用户）", sys.Uid)
	}
	return readAll(f)
}
