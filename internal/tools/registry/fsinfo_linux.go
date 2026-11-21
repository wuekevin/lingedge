//go:build linux

package registry

import (
	"strconv"
	"syscall"
)

// Linux statfs f_type 魔数（linux/magic.h）。flock 在这些网络/FUSE 文件系统上语义不可靠或需要服务端配合。
var netFS = map[int64]string{
	0x6969:     "nfs",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x65735546: "fuse",
	0x517B:     "smb",
	0x564C:     "ncpfs",
	0x47504653: "gpfs",
	0x6B414653: "afs",
	0x7461636F: "ocfs2",
	0x01021997: "9p",
	0x00C36400: "ceph",
}

func fsKind(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	t := int64(st.Type) //nolint:unconvert // Type 的宽度随架构不同
	name, isNet := netFS[t]
	if name == "" {
		name = "magic:0x" + strconv.FormatInt(t, 16)
	}
	return FSInfo{Type: name, Network: isNet}, nil
}
