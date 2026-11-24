package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

var testKDF = KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}

const testPass = "correct horse battery"

func enc(t *testing.T, plain []byte, meta []byte, chunk uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewEncryptWriter(&buf, []byte(testPass), testKDF, meta, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dec(b []byte, pass string) ([]byte, error) {
	r, _, err := NewDecryptReader(bytes.NewReader(b), []byte(pass))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// 加密往返：各种长度（含空、恰好整块、整块 ± 1、多块）。
func TestEncryptRoundTripSizes(t *testing.T) {
	const chunk = 1024
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3*chunk - 1, 3 * chunk, 10*chunk + 7} {
		plain := make([]byte, n)
		_, _ = rand.Read(plain)
		ct := enc(t, plain, []byte(`{"a":1}`), chunk)
		got, err := dec(ct, testPass)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: 往返失败 err=%v equal=%v", n, err, bytes.Equal(got, plain))
		}
		if n > 64 && bytes.Contains(ct, plain[:64]) {
			t.Fatalf("n=%d: 密文里出现了明文", n)
		}
	}
}

// 同一明文两次加密密文不同（随机 salt/nonce 前缀）。
func TestEncryptIsRandomized(t *testing.T) {
	a, b := enc(t, []byte("same plaintext"), nil, 1024), enc(t, []byte("same plaintext"), nil, 1024)
	if bytes.Equal(a, b) {
		t.Fatal("两次加密结果相同")
	}
}

func TestWrongPassphraseAndWeakPassphrase(t *testing.T) {
	ct := enc(t, []byte("secret"), nil, 1024)
	if _, err := dec(ct, "totally wrong pass"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("错误口令应为 ErrBadPassphrase: %v", err)
	}
	if _, err := dec(ct, ""); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("空口令: %v", err)
	}
	if _, err := NewEncryptWriter(io.Discard, []byte("short"), testKDF, nil, 0); !errors.Is(err, ErrWeakPassphrase) {
		t.Fatalf("弱口令应被拒绝: %v", err)
	}
}

// 篡改：头部任意字节、分块 flag、密文、tag 的每个翻转都必须被发现；截断（任意位置）与尾部多余数据也必须被发现。
func TestEncryptedTamperAndTruncationDetected(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 200) // 3200 字节，chunk=1024 → 4 块
	ct := enc(t, plain, []byte(`{"kek":"k1"}`), 1024)
	for i := 0; i < len(ct); i++ {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x01
		if got, err := dec(bad, testPass); err == nil && bytes.Equal(got, plain) {
			t.Fatalf("翻转第 %d 字节未被发现", i)
		} else if err == nil {
			t.Fatalf("翻转第 %d 字节后解密「成功」但内容不同", i)
		}
	}
	for cut := 0; cut < len(ct); cut++ {
		if got, err := dec(ct[:cut], testPass); err == nil {
			t.Fatalf("截断到 %d 字节未被发现（读到 %d 字节）", cut, len(got))
		}
	}
	if _, err := dec(append(append([]byte(nil), ct...), 0), testPass); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("尾部多余数据应为 ErrCorrupt: %v", err)
	}
}

// 分块重排：交换两个中间块 → 认证失败（nonce 含序号）。
func TestEncryptedChunkReorderDetected(t *testing.T) {
	plain := make([]byte, 4096)
	_, _ = rand.Read(plain)
	ct := enc(t, plain, nil, 1024)
	hdr := fixedHeaderLen
	chunkLen := 5 + 1024 + gcmTag
	c1, c2 := hdr+0*chunkLen, hdr+1*chunkLen
	bad := append([]byte(nil), ct...)
	copy(bad[c1:c1+chunkLen], ct[c2:c2+chunkLen])
	copy(bad[c2:c2+chunkLen], ct[c1:c1+chunkLen])
	if _, err := dec(bad, testPass); err == nil {
		t.Fatal("分块重排未被发现")
	}
	// 去掉 final 块（把倒数第二块之后截掉并补回一个合法长度的中间块）：由截断用例覆盖；这里再验证「重复一个中间块」。
	dup := append(append([]byte(nil), ct[:hdr+chunkLen]...), ct[hdr:]...)
	if _, err := dec(dup, testPass); err == nil {
		t.Fatal("重复分块未被发现")
	}
}

func TestPeekMetaAndHeaderLimits(t *testing.T) {
	ct := enc(t, []byte("x"), []byte(`{"component":"server"}`), 1024)
	meta, err := PeekMeta(bytes.NewReader(ct))
	if err != nil || string(meta) != `{"component":"server"}` {
		t.Fatalf("PeekMeta: %q %v", meta, err)
	}
	if _, err := PeekMeta(strings.NewReader("not an encrypted file at all")); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("非加密文件: %v", err)
	}
	// 恶意头部：argon2 内存参数超限 → 在派生前拒绝（防资源耗尽）。
	bad := append([]byte(nil), ct...)
	binary.BigEndian.PutUint32(bad[len(encMagic)+1+4:], 4<<20) // memKiB = 4 GiB
	if _, _, err := NewDecryptReader(bytes.NewReader(bad), []byte(testPass)); err == nil || errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("超限的 KDF 参数应被明确拒绝: %v", err)
	}
	if _, err := NewEncryptWriter(io.Discard, []byte(testPass), KDFParams{Time: 99, MemoryKiB: 64, Threads: 1}, nil, 0); err == nil {
		t.Fatal("超限的 KDF 参数应被拒绝")
	}
	if _, err := NewEncryptWriter(io.Discard, []byte(testPass), testKDF, make([]byte, maxMetaLen+1), 0); err == nil {
		t.Fatal("过长的 meta 应被拒绝")
	}
}

// 头部 meta 被改（不改密文）→ 解密失败：meta 随头部一起作为 AAD 被认证。
func TestMetaIsAuthenticated(t *testing.T) {
	ct := enc(t, []byte("payload"), []byte(`{"kek_ids_required":["k1"]}`), 1024)
	i := bytes.Index(ct, []byte(`"k1"`))
	if i < 0 {
		t.Fatal("meta 应以明文存于头部")
	}
	bad := append([]byte(nil), ct...)
	bad[i+2] = '2' // k1 → k2
	if _, err := dec(bad, testPass); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("meta 被篡改应使认证失败: %v", err)
	}
}

// 截断攻击：去掉末尾若干块，再把新的最后一块的 flag 改成 final——若 flag 不在 AAD 里，这会被当成合法的（更短的）备份。
func TestEncryptedTruncationWithForgedFinalFlagDetected(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 400) // 6400 字节，chunk=1024 → 7 块
	ct := enc(t, plain, nil, 1024)
	chunkLen := 5 + 1024 + gcmTag
	for keep := 1; keep < 6; keep++ {
		cut := append([]byte(nil), ct[:fixedHeaderLen+keep*chunkLen]...)
		cut[fixedHeaderLen+(keep-1)*chunkLen] = 1 // 伪造 final 标志
		got, err := dec(cut, testPass)
		if err == nil {
			t.Fatalf("保留 %d 块并伪造 final 标志未被发现（解出 %d 字节）", keep, len(got))
		}
	}
}
