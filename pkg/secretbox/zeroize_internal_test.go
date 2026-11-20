package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// AC4：Close 后 KEK 字节全部清零（直接检查内部 []byte）。
func TestKEKZeroedOnClose(t *testing.T) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte("k1:"+base64.RawURLEncoding.EncodeToString(raw)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := NewFileKeyProvider(FileOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	k := kp.s.keys["k1"]
	if string(k) != string(raw) {
		t.Fatal("加载的 KEK 与文件内容不符")
	}
	kp.Close()
	for i, b := range k {
		if b != 0 {
			t.Fatalf("Close 后 KEK[%d]=%d 未清零", i, b)
		}
	}
}
