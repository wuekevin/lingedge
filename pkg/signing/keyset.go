package signing

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// KeyStatus 是 keyset 内 key 的状态（proto SigningKey.Status，名称与 proto 枚举一致）。
type KeyStatus string

// 状态取值。PENDING 不进入 keyset（尚未启用的 key 不下发）。
const (
	StatusActive   KeyStatus = "ACTIVE"   // 用于签发与验证
	StatusRetiring KeyStatus = "RETIRING" // 仅验证（轮换重叠期）
	StatusRevoked  KeyStatus = "REVOKED"  // 禁止验证；此类 key 签发的修订必须拒绝
)

// Key 是 keyset 中的一把签名公钥（proto SigningKey）。
type Key struct {
	KeyID     string
	Algorithm string // 当前仅 "ed25519"
	PublicKey []byte // 32 字节原始公钥
	Status    KeyStatus
	// NotBefore 只限制 key 的启用时间；配置签名本身无过期（R-4）。零值 = 未给出。
	NotBefore time.Time
}

// Keyset 是签名公钥集（proto SigningKeyset）：链式轮换，新 keyset 由旧版中仍有效的 key 签名。
type Keyset struct {
	Version         uint64 // 单调递增
	Keys            []Key
	IssuedAt        time.Time
	PreviousVersion uint64 // 首版（信任起点）为 0
	SignedByKeyID   string // 签署本 keyset 的、PreviousVersion 中的 key；首版为空
	Signature       []byte // Ed25519(signed_by, "LINGEDGE-KEYSET-V1\n" ‖ canonical_json(version,keys,issued_at,previous_version))
}

// 链验证错误。
var (
	ErrKeysetInvalid   = errors.New("signing: keyset 结构非法")
	ErrKeysetRollback  = errors.New("signing: keyset 版本回滚（新版本不大于已信任版本）")
	ErrKeysetBroken    = errors.New("signing: keyset 链断裂（版本不连续或 previous_version 不符）")
	ErrKeysetSigner    = errors.New("signing: keyset 的签署者不是上一版中仍有效（ACTIVE/RETIRING）的 key")
	ErrKeysetSignature = errors.New("signing: keyset 签名验证失败（内容被篡改或签署者不符）")
	ErrKeyTampered     = errors.New("signing: 轮换后 key 的算法或公钥与上一版不一致")
	ErrKeyResurrected  = errors.New("signing: 已吊销的 key 不得恢复为有效状态")
	ErrNotRoot         = errors.New("signing: 信任起点 keyset 必须 previous_version=0 且无签名")
)

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// canonical 是 canonical_json 的结构化形态：字段按键名字典序声明，encoding/json 按声明顺序输出，故输出键已排序；
// 无空白、不转义 HTML、UTF-8；整数不带小数点；时间一律 UTC 的 RFC 3339（纳秒部分去掉尾随 0）；公钥为标准 base64（带填充，与 proto JSON 一致）。
type canonicalKey struct {
	Algorithm string  `json:"algorithm"`
	KeyID     string  `json:"key_id"`
	NotBefore *string `json:"not_before"`
	PublicKey string  `json:"public_key"`
	Status    string  `json:"status"`
}

type canonicalKeyset struct {
	IssuedAt        string         `json:"issued_at"`
	Keys            []canonicalKey `json:"keys"`
	PreviousVersion uint64         `json:"previous_version"`
	Version         uint64         `json:"version"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// CanonicalBody 返回被签名的 canonical_json(version, keys, issued_at, previous_version)（不含 signed_by_key_id 与 signature）。
// keys 的顺序即数组顺序（签发方应按 key_id 升序排列，使同一内容恒得到同一字节）。
func (ks Keyset) CanonicalBody() ([]byte, error) {
	if err := ks.validateShape(); err != nil {
		return nil, err
	}
	c := canonicalKeyset{IssuedAt: ts(ks.IssuedAt), PreviousVersion: ks.PreviousVersion, Version: ks.Version, Keys: make([]canonicalKey, 0, len(ks.Keys))}
	for _, k := range ks.Keys {
		ck := canonicalKey{Algorithm: k.Algorithm, KeyID: k.KeyID, PublicKey: base64.StdEncoding.EncodeToString(k.PublicKey), Status: string(k.Status)}
		if !k.NotBefore.IsZero() {
			s := ts(k.NotBefore)
			ck.NotBefore = &s
		}
		c.Keys = append(c.Keys, ck)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeysetInvalid, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// keysetInput 构造签名输入：域前缀 ‖ canonical body。
func (ks Keyset) keysetInput() ([]byte, error) {
	body, err := ks.CanonicalBody()
	if err != nil {
		return nil, err
	}
	in := make([]byte, 0, len(KeysetDomain)+len(body))
	in = append(in, KeysetDomain...)
	return append(in, body...), nil
}

// validateShape 检查结构：key_id 格式与唯一、算法/公钥长度、状态取值、至少一把 ACTIVE key。
func (ks Keyset) validateShape() error {
	if ks.Version == 0 {
		return fmt.Errorf("%w: version 必须大于 0", ErrKeysetInvalid)
	}
	if ks.PreviousVersion >= ks.Version {
		return fmt.Errorf("%w: previous_version 必须小于 version", ErrKeysetInvalid)
	}
	if ks.IssuedAt.IsZero() {
		return fmt.Errorf("%w: 缺少 issued_at", ErrKeysetInvalid)
	}
	if len(ks.Keys) == 0 {
		return fmt.Errorf("%w: keys 为空", ErrKeysetInvalid)
	}
	seen := map[string]bool{}
	active := 0
	for _, k := range ks.Keys {
		if !keyIDRe.MatchString(k.KeyID) {
			return fmt.Errorf("%w: key_id 格式非法", ErrKeysetInvalid)
		}
		if seen[k.KeyID] {
			return fmt.Errorf("%w: key_id 重复", ErrKeysetInvalid)
		}
		seen[k.KeyID] = true
		if k.Algorithm != AlgEd25519 {
			return fmt.Errorf("%w: 算法 %q", ErrAlgorithm, k.Algorithm)
		}
		if len(k.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: 公钥必须是 32 字节", ErrBadKey)
		}
		switch k.Status {
		case StatusActive:
			active++
		case StatusRetiring, StatusRevoked:
		default:
			return fmt.Errorf("%w: 状态非法", ErrKeysetInvalid)
		}
	}
	if active == 0 {
		return fmt.Errorf("%w: 没有 ACTIVE key", ErrKeysetInvalid)
	}
	return nil
}

// VerifyRoot 检查信任起点 keyset（enroll 阶段经 TLS+CA 钉扎获得，previous_version=0，无签名）。
func VerifyRoot(ks Keyset) error {
	if ks.PreviousVersion != 0 || len(ks.Signature) != 0 || ks.SignedByKeyID != "" {
		return ErrNotRoot
	}
	if ks.Version == 0 {
		return fmt.Errorf("%w: version 必须大于 0", ErrKeysetInvalid)
	}
	// 结构校验要求 previous_version < version：version>=1 恒成立。
	return ks.validateShape()
}

// SignKeyset 生成并签署下一版 keyset：next 的 Version/PreviousVersion/Keys/IssuedAt 由调用方填好，本函数写入 SignedByKeyID 与 Signature。
// signer 须是上一版 prev 中仍有效（ACTIVE/RETIRING）的 key 的私钥，且与 prev 里登记的公钥一致——签发方在签署前就用 VerifyKeysetLink 同一套规则自检，
// 不会产出节点必然拒绝的 keyset。
func SignKeyset(prev, next Keyset, signerKeyID string, signer ed25519.PrivateKey) (Keyset, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return Keyset{}, ErrBadKey
	}
	out := next
	out.SignedByKeyID = signerKeyID
	in, err := out.keysetInput()
	if err != nil {
		return Keyset{}, err
	}
	out.Signature = ed25519.Sign(signer, in)
	if err := VerifyKeysetLink(prev, out); err != nil {
		return Keyset{}, err
	}
	return out, nil
}

// VerifyKeysetLink 验证 next 是否是 prev 的合法后继（node-protocol §8）：
//   - 版本严格连续：next.Version = prev.Version+1 且 next.PreviousVersion = prev.Version（回滚/重放 → ErrKeysetRollback，缺版本 → ErrKeysetBroken）；
//   - 签署者必须是 prev 中 ACTIVE/RETIRING 的 key（REVOKED/未知 → ErrKeysetSigner），签名对 "LINGEDGE-KEYSET-V1\n" ‖ canonical body 成立（篡改 → ErrKeysetSignature）；
//   - 轮换后保留的 key，算法与公钥不得改变；REVOKED 不得恢复。
func VerifyKeysetLink(prev, next Keyset) error {
	if err := next.validateShape(); err != nil {
		return err
	}
	if next.Version <= prev.Version {
		return ErrKeysetRollback
	}
	if next.Version != prev.Version+1 || next.PreviousVersion != prev.Version {
		return ErrKeysetBroken
	}
	signer, err := prev.verifier(next.SignedByKeyID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeysetSigner, err)
	}
	in, err := next.keysetInput()
	if err != nil {
		return err
	}
	if len(next.Signature) != ed25519.SignatureSize || !ed25519.Verify(signer, in, next.Signature) {
		return ErrKeysetSignature
	}
	cur := map[string]Key{}
	for _, k := range next.Keys {
		cur[k.KeyID] = k
	}
	for _, p := range prev.Keys {
		k, ok := cur[p.KeyID]
		if !ok {
			continue
		}
		if k.Algorithm != p.Algorithm || !bytes.Equal(k.PublicKey, p.PublicKey) {
			return ErrKeyTampered
		}
		if p.Status == StatusRevoked && k.Status != StatusRevoked {
			return ErrKeyResurrected
		}
	}
	return nil
}

// VerifyChain 从已信任的 keyset 出发逐级验证 chain（按版本升序，node-protocol §8：「响应为 known_version 之后的完整链，节点逐级验证」），
// 返回链尾（新的已信任 keyset）。空链返回 trusted 本身。任何一级失败即整体失败，调用方不得部分采用。
func VerifyChain(trusted Keyset, chain []Keyset) (Keyset, error) {
	cur := trusted
	for i, next := range chain {
		if err := VerifyKeysetLink(cur, next); err != nil {
			return Keyset{}, fmt.Errorf("keyset 链第 %d 级: %w", i+1, err)
		}
		cur = next
	}
	return cur, nil
}
