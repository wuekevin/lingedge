package secretbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	version   = "v1"
	dekSize   = 32
	nonceSize = 12
	tagSize   = 16
)

var b64 = base64.RawURLEncoding

// RecordContext 标识一个被保护值的归属（表名|列名|记录 uid），即 AAD 的来源。
type RecordContext struct {
	Table  string // 例如 certificates
	Column string // 例如 private_key_enc
	UID    string // 记录 uid（ULID）
}

// AAD 返回 `表名|列名|记录 uid`。
func (r RecordContext) AAD() ([]byte, error) {
	for _, s := range []string{r.Table, r.Column, r.UID} {
		if s == "" || strings.Contains(s, "|") {
			return nil, ErrInvalidContext
		}
	}
	return []byte(r.Table + "|" + r.Column + "|" + r.UID), nil
}

// Box 用 KeyProvider 提供的 KEK 做信封加解密。并发安全（取决于 KeyProvider）。
type Box struct{ kp KeyProvider }

// New 构造 Box；kp 不得为 nil。
func New(kp KeyProvider) (*Box, error) {
	if kp == nil {
		return nil, ErrMasterKeyMissing
	}
	return &Box{kp: kp}, nil
}

// CurrentKeyID 返回新写入使用的 kek_id（即各表 enc_key_id 的取值）。
func (b *Box) CurrentKeyID() string { return b.kp.CurrentKeyID() }

// Seal 加密 plaintext：随机 DEK → AES-256-GCM 加密值 → KEK 包装 DEK。plaintext 由调用方持有并负责清零。
func (b *Box) Seal(ctx context.Context, plaintext []byte, rc RecordContext) (string, error) {
	aad, err := rc.AAD()
	if err != nil {
		return "", err
	}
	kekID := b.kp.CurrentKeyID()
	if !ValidKEKID(kekID) {
		return "", ErrNoCurrentKey
	}
	dek := make([]byte, dekSize)
	defer Zero(dek)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("secretbox: 生成 DEK 失败: %w", err)
	}
	aead, err := newGCM(dek)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: 生成 nonce 失败: %w", err)
	}
	ct := aead.Seal(nil, nonce, plaintext, aad)
	wrapped, err := b.kp.Wrap(ctx, kekID, dek, aad)
	if err != nil {
		return "", err
	}
	return format(kekID, wrapped, nonce, ct), nil
}

// Open 解密。返回的明文由调用方使用后清零（Zero）。
func (b *Box) Open(ctx context.Context, enc string, rc RecordContext) ([]byte, error) {
	aad, err := rc.AAD()
	if err != nil {
		return nil, err
	}
	p, err := parse(enc)
	if err != nil {
		return nil, err
	}
	dek, err := b.kp.Unwrap(ctx, p.kekID, p.wrapped, aad)
	if err != nil {
		return nil, err
	}
	defer Zero(dek)
	if len(dek) != dekSize {
		return nil, ErrDecrypt
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, p.nonce, p.ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// KeyID 返回密文内的 kek_id（解密以它为准；各表 enc_key_id 仅用于扫描）。
func KeyID(enc string) (string, error) {
	p, err := parse(enc)
	if err != nil {
		return "", err
	}
	return p.kekID, nil
}

// Rewrap 是 secrets.rewrap 原语：只把 DEK 从旧 KEK 重新包装到 targetKEK，nonce 与密文原样保留（不重加密数据）。
// 已是 targetKEK 时返回原串与 changed=false。新包装会被解包回读核对后才返回，避免写出不可解的新值。
func (b *Box) Rewrap(ctx context.Context, enc string, rc RecordContext, targetKEK string) (newEnc string, changed bool, err error) {
	aad, err := rc.AAD()
	if err != nil {
		return "", false, err
	}
	if !ValidKEKID(targetKEK) {
		return "", false, ErrNoCurrentKey
	}
	p, err := parse(enc)
	if err != nil {
		return "", false, err
	}
	if p.kekID == targetKEK {
		return enc, false, nil
	}
	dek, err := b.kp.Unwrap(ctx, p.kekID, p.wrapped, aad)
	if err != nil {
		return "", false, err
	}
	defer Zero(dek)
	wrapped, err := b.kp.Wrap(ctx, targetKEK, dek, aad)
	if err != nil {
		return "", false, err
	}
	check, err := b.kp.Unwrap(ctx, targetKEK, wrapped, aad)
	if err != nil {
		return "", false, err
	}
	defer Zero(check)
	if subtle.ConstantTimeCompare(check, dek) != 1 {
		return "", false, ErrDecrypt
	}
	return format(targetKEK, wrapped, p.nonce, p.ct), true, nil
}

type parsed struct {
	kekID              string
	wrapped, nonce, ct []byte
}

func format(kekID string, wrapped, nonce, ct []byte) string {
	return version + ":" + kekID + ":" + b64.EncodeToString(wrapped) + ":" + b64.EncodeToString(nonce) + ":" + b64.EncodeToString(ct)
}

func parse(enc string) (parsed, error) {
	parts := strings.Split(enc, ":")
	if len(parts) != 5 || parts[0] != version || !ValidKEKID(parts[1]) {
		return parsed{}, ErrInvalidFormat
	}
	wrapped, err1 := b64.DecodeString(parts[2])
	nonce, err2 := b64.DecodeString(parts[3])
	ct, err3 := b64.DecodeString(parts[4])
	if err1 != nil || err2 != nil || err3 != nil || len(nonce) != nonceSize || len(ct) < tagSize ||
		len(wrapped) < nonceSize+dekSize+tagSize {
		return parsed{}, ErrInvalidFormat
	}
	return parsed{kekID: parts[1], wrapped: wrapped, nonce: nonce, ct: ct}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: 初始化 AES 失败: %w", err)
	}
	return cipher.NewGCM(blk)
}
