package registry

// FSInfo 描述某路径所在文件系统。
type FSInfo struct {
	Type    string // 文件系统名（linux 为已知名或 magic:0x…；darwin 为 f_fstypename）
	Network bool   // 网络/FUSE 文件系统：flock 语义不可靠
}

// FSKind 识别 path 所在文件系统。doctor（Story 1.42）据此对「锁文件位于网络文件系统」告警（§16）；
// 本 Story 不自行告警，也不因此拒绝运行。
func FSKind(path string) (FSInfo, error) { return fsKind(path) }
