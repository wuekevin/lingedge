//go:build !linux && !darwin

package registry

import "os"

// 非 linux/darwin 的桩：没有 flock 语义就不能提供任务锁，一律失败关闭（Tools 的目标平台是 Linux，Windows 只用于交叉编译与单元测试）。
const noFollowFlag = 0

func flockTry(*os.File) (bool, error) {
	return false, &Error{Kind: KindUnsupported, Msg: "本平台不支持 flock 任务锁（Tools 仅支持 Linux）"}
}

func funlock(*os.File) {}

func fileUID(os.FileInfo) (int, bool) { return 0, false }

func nlink(os.FileInfo) uint64 { return 1 }

func euid() int { return -1 }

const platformSupported = false
