//go:build linux

package service

import "os"

// systemdBooted 等价于 sd_booted(3)：/run/systemd/system 存在且是目录，表示本机由 systemd 启动。
func systemdBooted() bool {
	st, err := os.Lstat("/run/systemd/system")
	return err == nil && st.IsDir()
}
