package manifest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 签名域前缀（tools-design §9.2/§9.3；与 Server 的 LINGEDGE-REVISION-V1 / LINGEDGE-KEYSET-V1 互不相同，domain separation）。
const (
	ManifestDomain    = "LINGEDGE-MANIFEST-V1\n"
	PackageDomain     = "LINGEDGE-PKG-V1\n"
	KeyRotationDomain = "LINGEDGE-KEYROT-V1\n"
)

// SigFile 是 manifest.json.sig / META/SHA256SUMS.sig 的内容（JSON，非二进制）。
type SigFile struct {
	KeyID string `json:"key_id"`
	Alg   string `json:"alg"`
	Sig   string `json:"sig"`

	sig []byte
}

// ParseSig 严格解析签名文件：未知字段、非 ed25519、非 64 字节签名一律拒绝。
func ParseSig(b []byte) (*SigFile, error) {
	if len(b) == 0 || len(b) > 4<<10 {
		return nil, newErr(KindSignature, "签名文件大小非法（%d 字节）", len(b))
	}
	var s SigFile
	if err := decodeStrict(b, &s); err != nil {
		return nil, &Error{Kind: KindSignature, Msg: "签名文件不是合法的 {key_id,alg,sig} JSON", Cause: err}
	}
	if s.Alg != "ed25519" {
		return nil, newErr(KindSignature, "签名算法必须是 ed25519，收到 %q", trimTo(s.Alg, 16))
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s.Sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, newErr(KindSignature, "sig 必须是 64 字节的标准 base64")
	}
	s.sig = raw
	return &s, nil
}

// SequenceStore 记录各通道已见的最大 sequence（防回滚）。
type SequenceStore interface {
	// Max 返回通道已见的最大 sequence；从未见过返回 (0,false,nil)。
	Max(channel string) (int64, bool, error)
	// Advance 把已见最大值提升到 seq（只升不降）。
	Advance(channel string, seq int64) error
}

// Verifier 执行 §9.5 第 1–4 步中与清单本身有关的全部检查。Now 必须由调用方注入（本包不读系统时钟）。
type Verifier struct {
	Trust *TrustSet
	Seen  SequenceStore
	Now   func() time.Time
}

// VerifyOptions 是一次验证的调用参数。
type VerifyOptions struct {
	Channel      string // 请求的通道（必填）：清单 channel 必须与之相同
	Offline      bool   // 清单来自离线 bundle
	AllowExpired bool   // --allow-expired-manifest：仅 Offline 时才允许；放行 valid_until 过期并告警
}

// Result 是验证通过的结果。副作用（落盘轮换公钥、推进 sequence）不在 Verify 里发生，
// 由调用方在整条校验链成功后调用 Commit（tools-design §16：sequence 验签成功后才写）。
type Result struct {
	Manifest *Manifest
	KeyID    string
	Warnings []string
	// Rotation 是已验证背书、待落盘的新公钥。
	Rotation []Key
}

// Verify 对 manifest.json 的原始字节与 .sig 做完整验证。顺序：大小 → 签名文件 → 选钥（未知/吊销/状态）→ 验签 → schema →
// 有效期窗口（以 generated_at 为准）→ channel → sequence → valid_until → key_rotation。任一步失败即返回 *Error。
func (v *Verifier) Verify(manifestBytes, sigBytes []byte, opt VerifyOptions) (*Result, error) {
	if v.Trust == nil || v.Now == nil || v.Seen == nil {
		return nil, newErr(KindOptions, "Verifier 未完整配置（Trust/Seen/Now 必填）")
	}
	if opt.Channel != "stable" && opt.Channel != "beta" {
		return nil, newErr(KindOptions, "必须指定通道 stable|beta")
	}
	if opt.AllowExpired && !opt.Offline {
		return nil, newErr(KindOptions, "--allow-expired-manifest 只能与 --offline 一起使用")
	}
	if len(manifestBytes) == 0 || len(manifestBytes) > maxManifestBytes {
		return nil, newErr(KindSchema, "清单大小非法（%d 字节）", len(manifestBytes))
	}
	sf, err := ParseSig(sigBytes)
	if err != nil {
		return nil, err
	}
	key, err := v.Trust.Lookup(sf.KeyID)
	if err != nil {
		return nil, err
	}
	if key.Status != StatusActive && key.Status != StatusRetired {
		return nil, newErr(KindKeyInvalid, "key_id %q 状态为 %s，不可用于验证", sf.KeyID, key.Status)
	}
	input := make([]byte, 0, len(ManifestDomain)+len(manifestBytes))
	input = append(append(input, ManifestDomain...), manifestBytes...)
	if !ed25519.Verify(key.pub, input, sf.sig) {
		return nil, newErr(KindSignature, "清单签名校验失败").with("key_id", sf.KeyID)
	}

	m, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	// 公钥有效期窗口以清单 generated_at 为准：旧清单（离线 bundle）不会因公钥后来到期而无法验证，
	// 而一把公钥不能签出「生成时间在其窗口之外」的清单；公钥泄露的补救是吊销，不靠时钟。
	if !key.validAt(m.GeneratedAt) {
		return nil, newErr(KindKeyInvalid, "清单 generated_at 不在 key_id %q 的有效期窗口内", sf.KeyID)
	}
	if m.Channel != opt.Channel {
		return nil, newErr(KindChannel, "清单通道 %s 与请求的通道 %s 不符", m.Channel, opt.Channel)
	}
	seen, ok, err := v.Seen.Max(m.Channel)
	if err != nil {
		return nil, err
	}
	if ok && m.Sequence < seen {
		return nil, newErr(KindRollback, "清单 sequence %d 小于本地已见最大值 %d（疑似回滚攻击）", m.Sequence, seen).
			with("sequence", m.Sequence).with("seen", seen)
	}
	res := &Result{Manifest: m, KeyID: sf.KeyID}
	now := v.Now()
	if !now.Before(m.ValidUntil) {
		if !opt.Offline || !opt.AllowExpired {
			return nil, newErr(KindExpired, "清单已于 %s 过期", m.ValidUntil.UTC().Format(time.RFC3339)).
				with("valid_until", m.ValidUntil.UTC().Format(time.RFC3339))
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("清单已于 %s 过期，按 --allow-expired-manifest 显式放行（气隙环境）；sequence、签名与吊销检查仍然生效", m.ValidUntil.UTC().Format(time.RFC3339)))
	}
	if m.KeyRotation != nil {
		res.Rotation, err = v.verifyRotation(m)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Commit 在整条校验链（包 SHA256、包内签名……）成功后调用：先落盘轮换公钥，再推进 sequence。
// 两步都幂等：中途崩溃后以同一份清单重试（sequence 相等允许）会得到相同结果。
func (v *Verifier) Commit(r *Result, keysDir string) error {
	for _, k := range r.Rotation {
		if err := writeRotationKey(keysDir, k); err != nil {
			return err
		}
		v.Trust.add(k)
	}
	return v.Seen.Advance(r.Manifest.Channel, r.Manifest.Sequence)
}

// --- key_rotation -----------------------------------------------------------------------------------------

const maxRotationKeys = 8

func validateRotationShape(kr *KeyRotation) error {
	if len(kr.Keys) == 0 || len(kr.Keys) > maxRotationKeys {
		return newErr(KindRotation, "key_rotation.keys 数量必须在 1..%d", maxRotationKeys)
	}
	ids := map[string]bool{}
	for i := range kr.Keys {
		k := &kr.Keys[i]
		if err := k.validate(); err != nil {
			return &Error{Kind: KindRotation, Msg: "key_rotation 公钥非法", Cause: err}
		}
		if k.Status != StatusActive {
			return newErr(KindRotation, "key_rotation 中的新公钥 %s 必须是 active", k.KeyID)
		}
		if ids[k.KeyID] {
			return newErr(KindRotation, "key_rotation 公钥 %s 重复", k.KeyID)
		}
		ids[k.KeyID] = true
	}
	return nil
}

// KeyRotationInput 返回新公钥背书的签名输入：域前缀 + 以 \n 分隔的 key_id、alg、public_key、not_before、not_after、status（缺省为空串，末尾 \n）。
// 背书覆盖新公钥的全部语义字段，因此背书后无法改动其有效期或状态。
func KeyRotationInput(k Key) []byte {
	return []byte(KeyRotationDomain + k.KeyID + "\n" + k.Alg + "\n" + k.PublicKey + "\n" + k.NotBefore + "\n" + k.NotAfter + "\n" + k.Status + "\n")
}

// verifyRotation 验证清单（本身已由可信公钥验签）里 key_rotation 的背书：每把新公钥必须有一条背书，
// 背书者是当前可信集里的 **active** 旧公钥（未吊销、generated_at 在其窗口内），签名对 KeyRotationInput 成立。
func (v *Verifier) verifyRotation(m *Manifest) ([]Key, error) {
	kr := m.KeyRotation
	endorse := map[string]Endorsement{}
	for _, e := range kr.Endorsements {
		if _, dup := endorse[e.KeyID]; dup {
			return nil, newErr(KindRotation, "公钥 %s 有多条背书", trimTo(e.KeyID, 64))
		}
		endorse[e.KeyID] = e
	}
	var out []Key
	for _, nk := range kr.Keys {
		e, ok := endorse[nk.KeyID]
		if !ok {
			return nil, newErr(KindRotation, "新公钥 %s 缺少背书", nk.KeyID)
		}
		delete(endorse, nk.KeyID)
		old, err := v.Trust.Lookup(e.EndorsedBy)
		if err != nil {
			return nil, &Error{Kind: KindRotation, Msg: "背书者 " + trimTo(e.EndorsedBy, 64) + " 不可信", Cause: err}
		}
		if old.Status != StatusActive {
			return nil, newErr(KindRotation, "背书者 %s 不是 active（只有旧 active key 可背书）", old.KeyID)
		}
		if !old.validAt(m.GeneratedAt) {
			return nil, newErr(KindRotation, "背书者 %s 在清单生成时不在有效期内", old.KeyID)
		}
		sig, err := base64.StdEncoding.Strict().DecodeString(e.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			return nil, newErr(KindRotation, "新公钥 %s 的背书签名格式非法", nk.KeyID)
		}
		if !ed25519.Verify(old.pub, KeyRotationInput(nk), sig) {
			return nil, newErr(KindRotation, "新公钥 %s 的背书签名校验失败", nk.KeyID)
		}
		if _, revoked := v.Trust.revoked[nk.KeyID]; revoked {
			return nil, newErr(KindRotation, "新公钥 %s 的 key_id 已在吊销集中", nk.KeyID)
		}
		if ex, exists := v.Trust.keys[nk.KeyID]; exists {
			if string(ex.pub) != string(nk.pub) {
				return nil, newErr(KindRotation, "新公钥 %s 与已有同名公钥冲突", nk.KeyID)
			}
			continue // 已落盘/内置：幂等
		}
		out = append(out, nk)
	}
	if len(endorse) > 0 {
		return nil, newErr(KindRotation, "存在没有对应新公钥的背书")
	}
	return out, nil
}

// writeRotationKey 把一把已验证的新公钥原子落盘到 keysDir/<key_id>.json（目录 0700、文件 0600，创建即最终权限）。
func writeRotationKey(keysDir string, k Key) error {
	if keysDir == "" {
		return &Error{Kind: KindTrustStore, Msg: "未指定 trust/keys.d 目录，无法落盘轮换公钥"}
	}
	if err := security.MkdirAllPerm(keysDir, 0o700); err != nil {
		return &Error{Kind: KindTrustStore, Msg: "无法创建 " + keysDir, Cause: err}
	}
	doc := KeysFile{SchemaVersion: 1, Keys: []Key{k}, RevokedKeyIDs: []string{}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := security.WriteFileAtomic(filepath.Join(keysDir, k.KeyID+".json"), append(b, '\n'), 0o600); err != nil {
		return &Error{Kind: KindTrustStore, Msg: "落盘公钥 " + k.KeyID + " 失败", Cause: err}
	}
	return nil
}

// --- trust/state ------------------------------------------------------------------------------------------

// FileSequenceStore 把各通道已见最大 sequence 存在 trust/state（tools-design §9.2；JSON，0600，原子写）。
// 并发由调用方的全局任务锁保证（§16）；本类型只保证「只升不降」与「文件损坏失败关闭」。
type FileSequenceStore struct{ Path string }

type stateDoc struct {
	SchemaVersion int              `json:"schema_version"`
	MaxSequence   map[string]int64 `json:"max_sequence"`
}

func (s FileSequenceStore) load() (stateDoc, bool, error) {
	fi, err := os.Lstat(s.Path)
	if os.IsNotExist(err) {
		return stateDoc{SchemaVersion: 1, MaxSequence: map[string]int64{}}, false, nil
	}
	if err != nil {
		return stateDoc{}, false, &Error{Kind: KindTrustStore, Msg: "无法读取 " + s.Path, Cause: err}
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 || fi.Size() > 64<<10 {
		return stateDoc{}, false, newErr(KindTrustStore, "%s 不是普通文件、权限过宽或过大，拒绝信任（失败关闭）", s.Path)
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return stateDoc{}, false, &Error{Kind: KindTrustStore, Msg: "无法读取 " + s.Path, Cause: err}
	}
	var d stateDoc
	if err := decodeStrict(b, &d); err != nil || d.SchemaVersion != 1 || d.MaxSequence == nil {
		return stateDoc{}, false, &Error{Kind: KindTrustStore, Msg: s.Path + " 已损坏，拒绝把它当作「从未见过任何清单」（会使回滚防护失效）", Cause: err}
	}
	for ch, n := range d.MaxSequence {
		if (ch != "stable" && ch != "beta") || n < 1 {
			return stateDoc{}, false, newErr(KindTrustStore, "%s 含非法记录，拒绝信任", s.Path)
		}
	}
	return d, true, nil
}

// Max 实现 SequenceStore。
func (s FileSequenceStore) Max(channel string) (int64, bool, error) {
	d, _, err := s.load()
	if err != nil {
		return 0, false, err
	}
	n, ok := d.MaxSequence[channel]
	return n, ok, nil
}

// Advance 实现 SequenceStore：只升不降。
func (s FileSequenceStore) Advance(channel string, seq int64) error {
	d, _, err := s.load()
	if err != nil {
		return err
	}
	if cur, ok := d.MaxSequence[channel]; ok && cur >= seq {
		return nil
	}
	d.MaxSequence[channel] = seq
	if err := security.MkdirAllPerm(filepath.Dir(s.Path), 0o700); err != nil {
		return &Error{Kind: KindTrustStore, Msg: "无法创建 " + filepath.Dir(s.Path), Cause: err}
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := security.WriteFileAtomic(s.Path, append(b, '\n'), 0o600); err != nil {
		return &Error{Kind: KindTrustStore, Msg: "无法写入 " + s.Path, Cause: err}
	}
	return nil
}
