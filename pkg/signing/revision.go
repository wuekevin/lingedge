package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// 域分隔前缀（node-protocol §7 步骤 3、§8）。末尾的换行是前缀的一部分。
const (
	RevisionDomain = "LINGEDGE-REVISION-V1\n"
	KeysetDomain   = "LINGEDGE-KEYSET-V1\n"
)

// AlgEd25519 是配置签名唯一允许的算法名（proto SigningKey.algorithm / RevisionSignature.algorithm）。
const AlgEd25519 = "ed25519"

// 验证错误。错误文本不含密钥、签名或清单内容。
var (
	ErrBadSignature = errors.New("signing: 签名验证失败")
	ErrUnknownKey   = errors.New("signing: key_id 不在 keyset 中")
	ErrKeyRevoked   = errors.New("signing: key 已被吊销")
	ErrAlgorithm    = errors.New("signing: 不支持的签名算法（配置签名仅接受 ed25519）")
	ErrBadKey       = errors.New("signing: 公钥或私钥格式非法")
)

// RevisionSignature 是 proto RevisionSignature 的等价结构：没有过期字段（R-4）。
type RevisionSignature struct {
	KeyID     string
	Algorithm string
	Signature []byte // 64 字节
}

// revisionInput 构造签名输入：域前缀 ‖ manifest 原始字节。
func revisionInput(manifest []byte) []byte {
	in := make([]byte, 0, len(RevisionDomain)+len(manifest))
	in = append(in, RevisionDomain...)
	return append(in, manifest...)
}

// SignRevision 对清单字节（manifest_json，签名对象是字节本身，不再做任何规范化）签名并记录 key_id。
// 没有时间参数：签名不含过期时间，也不含签发时间。priv 由调用方持有并负责清零。
func SignRevision(keyID string, priv ed25519.PrivateKey, manifest []byte) (RevisionSignature, error) {
	if keyID == "" || len(priv) != ed25519.PrivateKeySize {
		return RevisionSignature{}, ErrBadKey
	}
	return RevisionSignature{KeyID: keyID, Algorithm: AlgEd25519, Signature: ed25519.Sign(priv, revisionInput(manifest))}, nil
}

// VerifyRevision 用 keyset 中状态为 ACTIVE/RETIRING 的 key 验证修订签名（node-protocol §7 步骤 3）。
// REVOKED 或未知 key 失败；**不检查过期时间、不读取本机时钟**（R-4）。
func VerifyRevision(ks Keyset, manifest []byte, sig RevisionSignature) error {
	if sig.Algorithm != AlgEd25519 {
		return ErrAlgorithm
	}
	k, err := ks.verifier(sig.KeyID)
	if err != nil {
		return err
	}
	if !ed25519.Verify(k, revisionInput(manifest), sig.Signature) {
		return ErrBadSignature
	}
	return nil
}

// Fingerprint 返回原始公钥的指纹：SHA-256 的小写十六进制。
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])
}

// CertFingerprint 返回证书 DER 的指纹（CA 的即 ca_fp，ADR-003 §7）：SHA-256 的小写十六进制。
func CertFingerprint(certDER []byte) string { return Fingerprint(certDER) }

// verifier 返回 keyset 中可用于验证的 key 的公钥（ACTIVE/RETIRING）。
func (ks Keyset) verifier(keyID string) (ed25519.PublicKey, error) {
	for _, k := range ks.Keys {
		if k.KeyID != keyID {
			continue
		}
		switch k.Status {
		case StatusActive, StatusRetiring:
		case StatusRevoked:
			return nil, ErrKeyRevoked
		default:
			return nil, fmt.Errorf("%w: 未知状态", ErrUnknownKey)
		}
		if k.Algorithm != AlgEd25519 {
			return nil, ErrAlgorithm
		}
		if len(k.PublicKey) != ed25519.PublicKeySize {
			return nil, ErrBadKey
		}
		return ed25519.PublicKey(k.PublicKey), nil
	}
	return nil, ErrUnknownKey
}
