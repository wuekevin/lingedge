//go:build unix

package service

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// 创建即最终权限：与调用方的 umask 无关（umask 0 与 077 得到相同结果）。umask 是进程级状态，测试串行运行并恢复。
func TestCreationIsUmaskIndependent(t *testing.T) {
	for _, um := range []int{0o000, 0o022, 0o077, 0o277} {
		root := t.TempDir() // 先建根目录：本测试要验证的是 Tools 自己创建的东西，而不是测试夹具
		old := syscall.Umask(um)
		res, err := (&Ensurer{Root: root, Own: &recOwn{}}).Ensure(NodeDirs("lingedge-node", "lingedge-node"))
		fw := &FileWriter{Root: root, Own: &recOwn{}}
		_, ferr := fw.Create("/etc/lingedge/node/node.yaml", []byte("x: 1\n"), SpecConfig("lingedge-node"))
		_, kerr := fw.Create("/etc/lingedge/node/master.key", []byte("k"), SpecRootSecret)
		syscall.Umask(old)
		if err != nil || ferr != nil || kerr != nil {
			t.Fatalf("umask %04o：%v %v %v", um, err, ferr, kerr)
		}
		for p, w := range nodeWant() {
			if st, err := os.Stat(filepath.Join(root, p)); err != nil || st.Mode().Perm() != w.mode {
				t.Errorf("umask %04o：%s 模式 %v，期望 %04o（%v）", um, p, st.Mode().Perm(), w.mode, err)
			}
		}
		if st, _ := os.Stat(filepath.Join(root, "etc/lingedge/node/node.yaml")); st.Mode().Perm() != 0o640 {
			t.Errorf("umask %04o：配置文件模式 %v", um, st.Mode().Perm())
		}
		if st, _ := os.Stat(filepath.Join(root, "etc/lingedge/node/master.key")); st.Mode().Perm() != 0o400 {
			t.Errorf("umask %04o：秘密文件模式 %v", um, st.Mode().Perm())
		}
		if len(res) == 0 {
			t.Fatal("无结果")
		}
	}
}

// 真实 lchown：对「当前用户」chown 自己总是被允许，据此验证调用链确实走到 os.Lchown 并生效（root 属主的真实 chown 属 ENV-LNX）。
func TestRealLchownPathWorksForCurrentUser(t *testing.T) {
	root := t.TempDir()
	own := &recOwn{Real: true}
	if _, err := (&Ensurer{Root: root, Own: own}).Ensure(ServerDirs("lingedge", "lingedge")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "var/lib/lingedge/server"))
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if int(sys.Uid) != os.Getuid() || int(sys.Gid) != os.Getgid() {
		t.Errorf("属主 %d:%d", sys.Uid, sys.Gid)
	}
	if len(own.Calls) == 0 {
		t.Error("没有 chown 调用")
	}
	// 同时覆盖生产实现：SystemOwnership 对当前用户/组的解析与 Chown。
	so := SystemOwnership()
	cur := filepath.Join(root, "var/lib/lingedge/server")
	if err := so.Chown(cur, os.Getuid(), os.Getgid()); err != nil {
		t.Errorf("SystemOwnership.Chown 对自己应成功：%v", err)
	}
	if _, _, err := so.Resolve("lingedge-no-such-user-xyz", ""); KindOf(err) != KindConflict {
		t.Errorf("不存在的用户应为 conflict：%v", err)
	}
	if _, _, err := so.Resolve("root", "lingedge-no-such-group-xyz"); KindOf(err) != KindConflict {
		t.Errorf("不存在的组应为 conflict：%v", err)
	}
	if uid, _, err := so.Resolve("root", ""); err != nil || uid != 0 {
		t.Errorf("root 应解析为 uid 0：%d %v", uid, err)
	}
}
