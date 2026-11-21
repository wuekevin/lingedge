//go:build !unix

package probe

import "io/fs"

// 非 unix 平台取不到属主：socket 一律视为不受信（不发送凭据）。
func fileOwnerUID(fs.FileInfo) (int, bool) { return -1, false }
