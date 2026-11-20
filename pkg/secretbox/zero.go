package secretbox

import "runtime"

// Zero 把 b 的内容清零。clear 后的 KeepAlive 防止编译器把「写后不再读」的清零当作死存储消除。
func Zero(b []byte) {
	clear(b)
	runtime.KeepAlive(b)
}
