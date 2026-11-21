package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateMasterKeyIsFinalPermAndNeverOverwritten(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/lingedge/server"), 0o750); err != nil {
		t.Fatal(err)
	}
	own := &recOwn{}
	w := &FileWriter{Root: root, Own: own}
	secret := []byte("k1:" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + "\n")
	res, err := w.Create(ServerConfigDir+"/master.key", secret, SpecMasterKey(CredentialSystemd, "lingedge"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "etc/lingedge/server/master.key")
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o400 {
		t.Errorf("master.key 应为 0400：%v", st.Mode().Perm())
	}
	c, ok := own.callFor(p)
	if !ok || c.UID != 0 || c.GID != 0 {
		t.Errorf("master.key 应 chown root:root：%+v", c)
	}
	if c.SizeAtChown != 0 {
		t.Errorf("须先 chown、后写入内容（chown 时文件大小应为 0，得到 %d）", c.SizeAtChown)
	}
	if !res.Created || res.Undo.Action != ActRemovePath || !res.Undo.Created || res.Undo.SHA256 != sha256Hex(secret) || res.Undo.Path != ServerConfigDir+"/master.key" {
		t.Errorf("undo：%+v", res)
	}
	// 绝不覆盖主密钥：内容保持原样。
	if _, err := w.Create(ServerConfigDir+"/master.key", []byte("other"), SpecRootSecret); KindOf(err) != KindConflict {
		t.Fatalf("重复创建应冲突：%v", err)
	}
	if got := readFile(t, p); got != string(secret) {
		t.Error("主密钥被覆盖")
	}
	// 符号链接目标同样不跟随、不覆盖。
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, "etc/lingedge/server/link.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Create(ServerConfigDir+"/link.key", []byte("x"), SpecRootSecret); KindOf(err) != KindConflict {
		t.Fatalf("符号链接目标应冲突：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "elsewhere")); err == nil {
		t.Error("不得经符号链接创建文件")
	}
}

func TestFileSpecs(t *testing.T) {
	if s := SpecConfig("lingedge-node"); s.Mode != 0o640 || s.Owner != "root" || s.Group != "lingedge-node" {
		t.Errorf("配置文件：%+v", s)
	}
	if s := SpecMasterKey(CredentialSystemd, "lingedge"); s.Mode != 0o400 || s.Owner != "root" || s.Group != "root" {
		t.Errorf("LoadCredential 模式 master.key：%+v", s)
	}
	if s := SpecMasterKey(CredentialKeyFile, "lingedge"); s.Mode != 0o440 || s.Owner != "root" || s.Group != "lingedge" {
		t.Errorf("降级模式 master.key：%+v", s)
	}
	if SpecUnit.Mode != 0o644 || SpecUnit.Owner != "root" || SpecUnit.Group != "root" {
		t.Errorf("单元：%+v", SpecUnit)
	}
}

func TestCreateChownFailureLeavesNothing(t *testing.T) {
	root := t.TempDir()
	w := &FileWriter{Root: root, Own: &recOwn{Fail: os.ErrPermission}}
	_, err := w.Create("/secret", []byte("k"), SpecRootSecret)
	if KindOf(err) != KindPermission {
		t.Fatalf("%v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "secret")); err == nil {
		t.Error("chown 失败不得留下文件（更不得留下已写入的内容）")
	}
}

func TestCreateMissingParentFails(t *testing.T) {
	w := &FileWriter{Root: t.TempDir(), Own: &recOwn{}}
	if _, err := w.Create("/no/such/dir/f", []byte("x"), SpecUnit); KindOf(err) != KindIO {
		t.Fatalf("%v", err)
	}
}

func TestMasterKeyConfigValue(t *testing.T) {
	const p = "/etc/lingedge/server/master.key"
	if got := MasterKeyConfigValue(CredentialSystemd, p); got != "" {
		t.Errorf("LoadCredential 模式 secrets.master_key_file 必须留空（否则 Server 绕过凭据目录）：%q", got)
	}
	if got := MasterKeyConfigValue(CredentialKeyFile, p); got != p {
		t.Errorf("降级模式必须填路径：%q", got)
	}
}
