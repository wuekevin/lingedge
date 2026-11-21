//go:build darwin

package registry

import "syscall"

func fsKind(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	name := string(b)
	switch name {
	case "nfs", "smbfs", "afpfs", "webdav", "cifs", "osxfuse", "macfuse":
		return FSInfo{Type: name, Network: true}, nil
	}
	return FSInfo{Type: name}, nil
}
