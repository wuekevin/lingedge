package secretbox_test

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

var rc = secretbox.RecordContext{Table: "certificates", Column: "private_key_enc", UID: "01JABCDEFGHJKMNPQRSTVWXYZ0"}

// AC1：格式 v1:<kek_id>:<wrapped_dek>:<nonce>:<ct+tag>，各段长度可核对（AES-256-GCM：nonce 12、tag 16、DEK 32）。
func TestSealFormatAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	box := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	pt := []byte("-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n")
	enc, err := box.Seal(ctx, pt, rc)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v1:k1:[A-Za-z0-9_-]+:[A-Za-z0-9_-]+:[A-Za-z0-9_-]+$`).MatchString(enc) {
		t.Fatalf("格式不符: %q", enc)
	}
	parts := strings.Split(enc, ":")
	if len(parts) != 5 {
		t.Fatalf("段数=%d", len(parts))
	}
	dec := func(s string) int {
		b, err := b64().DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	if got := dec(parts[2]); got != 12+32+16 {
		t.Errorf("wrapped_dek 长度=%d，应为 nonce12+DEK32+tag16", got)
	}
	if got := dec(parts[3]); got != 12 {
		t.Errorf("nonce 长度=%d", got)
	}
	if got := dec(parts[4]); got != len(pt)+16 {
		t.Errorf("ct+tag 长度=%d，应为 %d", got, len(pt)+16)
	}
	if strings.Contains(enc, "PRIVATE") {
		t.Error("密文含明文片段")
	}
	got, err := box.Open(ctx, enc, rc)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip: %v", err)
	}
	if id, err := secretbox.KeyID(enc); err != nil || id != "k1" {
		t.Errorf("KeyID=%q %v", id, err)
	}
}

// 每值独立随机 DEK：同一明文两次加密，wrapped_dek/nonce/ct 全不同；且一个值的 DEK 解不开另一个值。
func TestEachValueHasIndependentDEK(t *testing.T) {
	ctx := context.Background()
	box := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	a, _ := box.Seal(ctx, []byte("same"), rc)
	b, _ := box.Seal(ctx, []byte("same"), rc)
	pa, pb := strings.Split(a, ":"), strings.Split(b, ":")
	for i := 2; i < 5; i++ {
		if pa[i] == pb[i] {
			t.Errorf("第 %d 段重复：DEK/nonce 未独立随机", i+1)
		}
	}
	// 把 b 的 wrapped_dek 换成 a 的：值无法解密
	forged := strings.Join([]string{pb[0], pb[1], pa[2], pb[3], pb[4]}, ":")
	if _, err := box.Open(ctx, forged, rc); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("DEK 互换应解密失败: %v", err)
	}
}

// AC1 AAD 搬迁：A 记录的密文复制到 B 记录/别的列/别的表，解密一律失败。
func TestAADRelocationFails(t *testing.T) {
	ctx := context.Background()
	box := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	enc, _ := box.Seal(ctx, []byte("secret-a"), rc)
	for name, other := range map[string]secretbox.RecordContext{
		"别的记录": {Table: rc.Table, Column: rc.Column, UID: "01JABCDEFGHJKMNPQRSTVWXYZ1"},
		"别的列":  {Table: rc.Table, Column: "credential_enc", UID: rc.UID},
		"别的表":  {Table: "acme_accounts", Column: rc.Column, UID: rc.UID},
	} {
		if _, err := box.Open(ctx, enc, other); !errors.Is(err, secretbox.ErrDecrypt) {
			t.Errorf("%s：搬迁后应 ErrDecrypt，得到 %v", name, err)
		}
		if _, _, err := box.Rewrap(ctx, enc, other, "k1"); err != nil { // 同 KEK 不做事，不应校验
			t.Errorf("%s: 同 KEK Rewrap 应直接返回: %v", name, err)
		}
	}
	if got, err := box.Open(ctx, enc, rc); err != nil || string(got) != "secret-a" {
		t.Fatalf("原位置应仍可解密: %v", err)
	}
	// AAD 分隔符混淆：('a|b','c') 与 ('a','b|c') 不得等价——含 '|' 的片段被直接拒绝
	if _, err := box.Seal(ctx, []byte("x"), secretbox.RecordContext{Table: "a|b", Column: "c", UID: "u"}); !errors.Is(err, secretbox.ErrInvalidContext) {
		t.Errorf("含 | 的上下文应被拒: %v", err)
	}
	if _, err := box.Seal(ctx, []byte("x"), secretbox.RecordContext{Table: "a", Column: "", UID: "u"}); !errors.Is(err, secretbox.ErrInvalidContext) {
		t.Errorf("空上下文应被拒: %v", err)
	}
}

// 篡改任何一段、换主密钥、畸形输入：都得到明确错误，不 panic。
func TestTamperAndMalformed(t *testing.T) {
	ctx := context.Background()
	box := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	enc, _ := box.Seal(ctx, []byte("hello world"), rc)
	parts := strings.Split(enc, ":")
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	for i := 2; i < 5; i++ {
		p := append([]string(nil), parts...)
		p[i] = flip(p[i])
		if _, err := box.Open(ctx, strings.Join(p, ":"), rc); err == nil {
			t.Errorf("篡改第 %d 段应失败", i+1)
		}
	}
	// 另一把主密钥（同 kek_id 不同内容）解不开
	other := newBox(t, newProvider(t, "k1:"+newKey(t)+"\n"))
	if _, err := other.Open(ctx, enc, rc); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("错误主密钥应 ErrDecrypt: %v", err)
	}
	for _, bad := range []string{"", "v1", "v2:k1:a:b:c", "v1:K1:a:b:c", "v1:k1:a:b", "v1:k1:a:b:c:d", "v1:k1:!!:!!:!!", enc + ":x", "v1:k1:" + parts[2] + ":AAAA:" + parts[4]} {
		if _, err := box.Open(ctx, bad, rc); !errors.Is(err, secretbox.ErrInvalidFormat) {
			t.Errorf("%q 应 ErrInvalidFormat: %v", bad, err)
		}
	}
	if _, err := box.Open(ctx, "v1:k9:"+strings.Join(parts[2:], ":"), rc); !errors.Is(err, secretbox.ErrKEKNotFound) {
		t.Errorf("未知 kek_id 应 ErrKEKNotFound: %v", err)
	}
}

func FuzzOpenNeverPanics(f *testing.F) {
	f.Add("v1:k1:AAAA:AAAA:AAAA")
	f.Add("::::")
	f.Add("v1:k1:" + strings.Repeat("A", 80) + ":" + strings.Repeat("A", 16) + ":" + strings.Repeat("A", 22))
	p, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: writeKeyFile(f, "k1:"+newKey(f)+"\n", 0o400)})
	if err != nil {
		f.Fatal(err)
	}
	defer p.Close()
	box := newBox(f, p)
	f.Fuzz(func(t *testing.T, s string) { _, _ = box.Open(context.Background(), s, rc) })
}
