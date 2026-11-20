package secretbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// AC2：权限必须是 0400/0440；其余一律拒绝启动。
func TestFilePermissions(t *testing.T) {
	key := newKey(t)
	content := "k1:" + key + "\n"
	cases := []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o400, true}, {0o440, true},
		{0o600, false}, {0o640, false}, {0o644, false}, {0o444, false}, {0o404, false}, {0o444 | os.ModeSetuid, false}, {0o000, false}, {0o200, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%v", c.mode), func(t *testing.T) {
			p := writeKeyFile(t, content, c.mode)
			kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
			if kp != nil {
				defer kp.Close()
			}
			if c.ok && err != nil {
				t.Fatalf("应接受: %v", err)
			}
			if !c.ok {
				if !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
					t.Fatalf("应拒绝(ErrMasterKeyInvalid): %v", err)
				}
				if strings.Contains(err.Error(), key) {
					t.Fatal("错误信息泄露主密钥")
				}
			}
		})
	}
}

// AC2：属主校验。本机非 root：把允许属主设成「别的 uid」时必须拒绝；缺省（运行用户自身）接受。
// 真实「属主 root:root 0400 经 LoadCredential」的场景属等待环境验证（见 docs/operations/secrets.md）。
func TestFileOwnerCheck(t *testing.T) {
	p := writeKeyFile(t, "k1:"+newKey(t)+"\n", 0o400)
	euid := uint32(os.Geteuid())
	if kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p}); err != nil {
		t.Fatalf("属主为运行用户应接受: %v", err)
	} else {
		kp.Close()
	}
	if kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p, AllowedUIDs: []uint32{euid}}); err != nil {
		t.Fatalf("显式允许自身应接受: %v", err)
	} else {
		kp.Close()
	}
	_, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p, AllowedUIDs: []uint32{euid + 4242}})
	if !errors.Is(err, secretbox.ErrMasterKeyInvalid) || !strings.Contains(err.Error(), "属主") {
		t.Fatalf("属主不符应拒绝: %v", err)
	}
	if euid != 0 {
		// 非 root 运行时，缺省允许集含 root(0) 与自身；文件属主是自身，已在上面验证通过。
		_, err = secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p, AllowedUIDs: []uint32{}})
		if err == nil {
			t.Fatal("空允许集应拒绝一切属主")
		}
	}
}

// AC2：缺失主密钥 → 明确错误、拒绝（F-7）；解密侧缺 kek_id 同样拒绝。
func TestMissingMasterKey(t *testing.T) {
	_, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{})
	if !errors.Is(err, secretbox.ErrMasterKeyMissing) || !strings.Contains(err.Error(), "缺少主密钥") {
		t.Fatalf("未配置路径: %v", err)
	}
	_, err = secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: filepath.Join(t.TempDir(), "nope", "master.key")})
	if !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Fatalf("文件不存在: %v", err)
	}
	if _, err := secretbox.New(nil); !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Fatalf("Box 无 KeyProvider: %v", err)
	}
	// 数据库里有 k1 的密文，但主密钥文件只有 k2：拒绝解密并指出缺的 kek_id
	ctx := context.Background()
	box1 := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	enc, _ := box1.Seal(ctx, []byte("v"), rc)
	box2 := newBox(t, newProvider(t, "k2:"+newKey(t)+"\n"))
	_, err = box2.Open(ctx, enc, rc)
	if !errors.Is(err, secretbox.ErrKEKNotFound) || !strings.Contains(err.Error(), "k1") {
		t.Fatalf("缺 kek 应拒绝并指出 kek_id: %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	if p, err := secretbox.ResolveMasterKeyPath("", dir); err != nil || p != filepath.Join(dir, "master.key") {
		t.Errorf("凭据目录: %q %v", p, err)
	}
	if p, err := secretbox.ResolveMasterKeyPath("/etc/lingedge/server/master.key", dir); err != nil || p != "/etc/lingedge/server/master.key" {
		t.Errorf("配置路径优先: %q %v", p, err)
	}
	if _, err := secretbox.ResolveMasterKeyPath("rel/master.key", ""); !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
		t.Errorf("相对路径应拒: %v", err)
	}
	if _, err := secretbox.ResolveMasterKeyPath("", ""); !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Errorf("都缺: %v", err)
	}
	// 经 $CREDENTIALS_DIRECTORY 路径的真实读取（目录与文件在本机构造，不依赖 systemd）
	if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte("k1:"+newKey(t)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{CredentialsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	kp.Close()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if secretbox.CredentialsDirFromEnv() != dir {
		t.Error("CredentialsDirFromEnv")
	}
}

func TestFileTypeAndSymlinkRejected(t *testing.T) {
	good := writeKeyFile(t, "k1:"+newKey(t)+"\n", 0o400)
	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: link}); !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
		t.Errorf("符号链接应拒绝: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: dir}); !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
		t.Errorf("目录应拒绝: %v", err)
	}
}

func TestMasterKeyFileFormat(t *testing.T) {
	k1, k2 := newKey(t), newKey(t)
	good := func(content string) (*secretbox.FileKeyProvider, error) {
		return secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: writeKeyFile(t, content, 0o400)})
	}
	kp, err := good("# 注释\n\nk1:" + k1 + "\nk2:" + k2 + "\n")
	if err != nil || kp.CurrentKeyID() != "k2" || fmt.Sprint(kp.KeyIDs()) != "[k1 k2]" {
		t.Fatalf("缺省当前=最后一条: %v %v", err, kp)
	}
	kp.Close()
	kp, err = good("@current k1\nk1:" + k1 + "\r\nk2:" + k2 + "\r\n")
	if err != nil || kp.CurrentKeyID() != "k1" {
		t.Fatalf("@current: %v", err)
	}
	kp.Close()
	short := newKey(t)[:20]
	for name, c := range map[string]string{
		"空文件":          "",
		"只有注释":         "# x\n",
		"缺冒号":          "k1 " + k1 + "\n",
		"大写 kek_id":    "K1:" + k1 + "\n",
		"过短":           "k1:" + short + "\n",
		"非 base64":     "k1:!!!!\n",
		"全零":           "k1:" + strings.Repeat("A", 43) + "\n",
		"重复 id":        "k1:" + k1 + "\nk1:" + k2 + "\n",
		"@current 不存在": "@current k9\nk1:" + k1 + "\n",
		"未知指令":         "@foo k1\nk1:" + k1 + "\n",
	} {
		_, err := good(c)
		if !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
			t.Errorf("%s: 应拒绝: %v", name, err)
			continue
		}
		for _, secret := range []string{k1, k2} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: 错误泄露主密钥", name)
			}
		}
	}
	// 文件过大
	if _, err := good(strings.Repeat("#", 70<<10)); !errors.Is(err, secretbox.ErrMasterKeyInvalid) {
		t.Errorf("过大应拒: %v", err)
	}
}

// 主密钥不入日志：任何格式化动词都只输出占位文本。
func TestProviderNeverPrintsKey(t *testing.T) {
	key := newKey(t)
	kp := newProvider(t, "k1:"+key+"\n")
	for _, s := range []string{fmt.Sprintf("%v", kp), fmt.Sprintf("%+v", kp), fmt.Sprintf("%#v", kp), fmt.Sprintf("%s", kp), fmt.Sprint(*kp)} {
		if strings.Contains(s, key) {
			t.Errorf("格式化输出泄露主密钥: %q", s)
		}
	}
	if got := fmt.Sprintf("%v", kp); !strings.Contains(got, "***") {
		t.Errorf("输出 %q 应含掩码", got)
	}
}
