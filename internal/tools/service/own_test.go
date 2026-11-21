package service

import (
	"os"
	"sync"
)

type chownCall struct {
	Path     string
	UID, GID int
	// SizeAtChown 是 chown 发生时文件的大小（-1 表示不是普通文件/不存在）：用来证明「先 chown、后写内容」。
	SizeAtChown int64
}

// recOwn 是注入的 Ownership：非 root 的测试无法真实 chown 到 lingedge 等用户，故只记录调用并断言（真实 chown 属 ENV-LNX）。
// Resolve 给每个名字一个固定、互不相同的 uid/gid（root=0:0）；Real=true 时 Chown 额外对「当前用户」做一次真实 lchown，
// 以验证调用链确实能走到 os.Lchown（当前用户 chown 给自己总是被允许的）。
type recOwn struct {
	mu    sync.Mutex
	Calls []chownCall
	Real  bool
	Same  bool // Resolve 把所有名字都映射成当前用户（让既有目录「属主正确」）
	Fail  error
}

var testIDs = map[string][2]int{"root": {0, 0}, "lingedge": {901, 901}, "lingedge-node": {902, 902}}

func (r *recOwn) Resolve(user, group string) (int, int, error) {
	if r.Same {
		return os.Getuid(), os.Getgid(), nil
	}
	u, ok := testIDs[user]
	if !ok {
		u = [2]int{999, 999}
	}
	g, ok := testIDs[group]
	if !ok {
		g = [2]int{999, 999}
	}
	return u[0], g[1], nil
}

func (r *recOwn) Chown(path string, uid, gid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := int64(-1)
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() {
		size = st.Size()
	}
	r.Calls = append(r.Calls, chownCall{path, uid, gid, size})
	if r.Fail != nil {
		return r.Fail
	}
	if r.Real {
		return os.Lchown(path, os.Getuid(), os.Getgid())
	}
	return nil
}

func (r *recOwn) callFor(path string) (chownCall, bool) {
	for _, c := range r.Calls {
		if c.Path == path {
			return c, true
		}
	}
	return chownCall{}, false
}
