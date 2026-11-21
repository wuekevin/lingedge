package setuptoken

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateHashVerify(t *testing.T) {
	tok, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 || len(hash) != 64 || hash != Hash(tok) || strings.ToLower(hash) != hash {
		t.Fatalf("格式: tok=%d hash=%d", len(tok), len(hash))
	}
	tok2, hash2, _ := Generate()
	if tok == tok2 || hash == hash2 {
		t.Fatal("每次生成必须不同（256 位随机）")
	}
	if !Verify(tok, hash) || !Verify(tok, "  "+strings.ToUpper(hash)+"\n") {
		t.Error("正确 token 应通过（哈希大小写/空白不敏感）")
	}
	for _, bad := range []string{"", tok2, tok + "x", tok[:len(tok)-1], strings.ToUpper(tok)} {
		if Verify(bad, hash) {
			t.Errorf("错误 token %q 不应通过", bad)
		}
	}
	for _, badHash := range []string{"", "zz", hash[:62], hash + "00", strings.Repeat("g", 64)} {
		if Verify(tok, badHash) {
			t.Errorf("非法哈希 %q 一律视为不匹配", badHash)
		}
	}
}

func TestHashFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h")
	if _, err := ReadHashFile(p); !errors.Is(err, ErrNoHash) {
		t.Fatalf("不存在应 ErrNoHash: %v", err)
	}
	tok, hash, _ := Generate()
	if err := WriteHashFile(p, hash, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("权限: %v", st.Mode())
	}
	got, err := ReadHashFile(p)
	if err != nil || got != hash || !Verify(tok, got) {
		t.Fatalf("往返: %v", err)
	}
	if err := WriteHashFile(p, "short", 0o600); err == nil {
		t.Error("非法哈希应拒绝写入")
	}
	// 损坏/空文件：读取失败（调用方 fail-closed）。
	for _, content := range []string{"", "not-hex", strings.Repeat("a", 63), strings.Repeat("a", 300)} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadHashFile(p); err == nil || errors.Is(err, ErrNoHash) {
			t.Errorf("%q 应报格式错误: %v", content, err)
		}
	}
	// 目录不是普通文件。
	if _, err := ReadHashFile(dir); err == nil {
		t.Error("目录应被拒绝")
	}
	// 临时文件无残留。
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".setup-token-") {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
}
