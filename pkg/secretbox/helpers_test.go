package secretbox_test

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// newKey 生成一个真实的 32 字节随机 KEK 的 base64url 文本。
func newKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// writeKeyFile 在临时目录写主密钥文件并显式 chmod（绕开 umask）。
func writeKeyFile(t testing.TB, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func newProvider(t testing.TB, content string) *secretbox.FileKeyProvider {
	t.Helper()
	p, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: writeKeyFile(t, content, 0o400)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func newBox(t testing.TB, p secretbox.KeyProvider) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64() *base64.Encoding { return base64.RawURLEncoding }
