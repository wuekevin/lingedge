package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// 主密钥指纹（F-7、ADR-011 §8）。备份清单记录两种指纹：
//   - master_key_fingerprint：master.key 文件内容的 SHA-256（与 `lingedge install server` 输出的「主密钥指纹（sha256 前 8 字节）」同源，运维可用
//     `sha256sum master.key` 目视核对）；
//   - kek_fingerprints：每个 kek_id 的单键指纹 SHA-256(域分隔前缀 ‖ kek_id ‖ 0x00 ‖ 密钥字节)，与文件里的注释、行序、@current 声明无关，
//     用于判定「是不是同一把密钥」。KEK 是 256 位随机值，SHA-256 的原像抗性使两种指纹都不泄露密钥。
// 恢复时的判定（CheckKeyMatch）：整体指纹相同直接通过；否则按「备份所需的每个 kek_id」逐个比对单键指纹——主密钥文件在备份之后新增了 KEK（轮换）
// 仍可恢复旧备份，缺少或不同的 KEK 则拒绝。

const fingerprintDomain = "lingedge-kek-fingerprint-v1\x00"

// KeySet 是主密钥文件的非机密摘要（只含 kek_id 与指纹，从不含密钥字节）。
type KeySet struct {
	Current string
	IDs     []string          // 升序
	FP      map[string]string // kek_id → 单键指纹（64 位十六进制）
	Overall string            // master.key 文件内容的 SHA-256（64 位十六进制）
}

// ShortFingerprint 返回便于目视核对的短形式：前 16 位十六进制（sha256 前 8 字节）——与 `lingedge install server` 输出的「主密钥指纹」同一格式。
func ShortFingerprint(fp string) string {
	if len(fp) < 16 {
		return fp
	}
	return fp[:16]
}

func kekFingerprint(id string, key []byte) string {
	h := sha256.New()
	h.Write([]byte(fingerprintDomain))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))
}

func fileFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// LoadKeySet 读取并校验主密钥文件，返回指纹摘要。
//
// 先经 secretbox.NewFileKeyProvider（生产同一套校验：O_NOFOLLOW、0400/0440、属主 root 或当前用户、格式、32 字节非全零）fail-closed，
// 再读一次原文件计算指纹；密钥字节在函数返回前清零，也从不写入日志或错误信息。
// allowedUIDs 为 nil 时取 {当前 euid, 0}。
func LoadKeySet(path string, allowedUIDs []uint32) (KeySet, error) {
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: path, AllowedUIDs: allowedUIDs})
	if err != nil {
		if errors.Is(err, secretbox.ErrMasterKeyMissing) {
			return KeySet{}, ErrMasterKeyMissing
		}
		return KeySet{}, fmt.Errorf("%w: %v", ErrMasterKeyInvalid, err)
	}
	defer kp.Close()
	f, err := openNoFollow(path)
	if err != nil {
		return KeySet{}, fmt.Errorf("%w: 无法打开", ErrMasterKeyInvalid)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return KeySet{}, fmt.Errorf("%w: 读取失败", ErrMasterKeyInvalid)
	}
	defer zero(data)
	ks := KeySet{FP: map[string]string{}, Current: kp.CurrentKeyID()}
	enc := base64.RawURLEncoding
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' || line[0] == '@' {
			continue
		}
		id, b64, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		key := make([]byte, enc.DecodedLen(len(b64)))
		n, derr := enc.Decode(key, b64)
		key = key[:n]
		if derr != nil {
			zero(key)
			return KeySet{}, fmt.Errorf("%w: 格式", ErrMasterKeyInvalid)
		}
		ks.FP[string(id)] = kekFingerprint(string(id), key)
		zero(key)
	}
	for id := range ks.FP {
		ks.IDs = append(ks.IDs, id)
	}
	sort.Strings(ks.IDs)
	if len(ks.IDs) == 0 {
		return KeySet{}, fmt.Errorf("%w: 没有任何主密钥", ErrMasterKeyInvalid)
	}
	ks.Overall = fileFingerprint(data)
	return ks, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// 主密钥相关错误。
var (
	ErrMasterKeyMissing = errors.New("backup: 主密钥文件不存在")
	ErrMasterKeyInvalid = errors.New("backup: 主密钥文件不可用")
)

// KeyMismatch 说明主密钥与备份不匹配的原因（可直接展示给用户；不含密钥字节）。
type KeyMismatch struct {
	Missing   []string // 备份需要但当前主密钥里没有的 kek_id
	Different []string // kek_id 相同但指纹不同（不是同一把密钥）
	Overall   bool     // 没有 kek_ids_required 可比对时，整体指纹不一致
}

func (m *KeyMismatch) Error() string {
	var parts []string
	if len(m.Missing) > 0 {
		parts = append(parts, "当前 master.key 缺少备份所需的 kek_id："+strings.Join(m.Missing, "、"))
	}
	if len(m.Different) > 0 {
		parts = append(parts, "下列 kek_id 的主密钥与备份时不是同一把（指纹不一致）："+strings.Join(m.Different, "、"))
	}
	if m.Overall {
		parts = append(parts, "master.key 的整体指纹与备份记录不一致")
	}
	return "主密钥与备份不匹配：" + strings.Join(parts, "；")
}

// CheckKeyMatch 比对当前主密钥与备份清单。规则：
//   - 备份没有任何指纹（组件无主密钥，如 node）：不比对，返回 nil；
//   - 整体指纹相同：通过；
//   - 否则逐个比对「所需的 kek_id」（kek_ids_required；库内无密文时取备份记录的全部 kek_id）：必须存在于当前主密钥且单键指纹相同
//     （当前文件可以多出后来新增的 KEK，轮换不阻碍旧备份恢复）；缺少或指纹不同 → 不匹配。
func CheckKeyMatch(m Manifest, cur KeySet) *KeyMismatch {
	if m.MasterKeyFingerprint == "" && len(m.KEKFingerprints) == 0 {
		return nil
	}
	if m.MasterKeyFingerprint != "" && m.MasterKeyFingerprint == cur.Overall {
		return nil
	}
	required := append([]string(nil), m.KEKIDsRequired...)
	if len(required) == 0 {
		for id := range m.KEKFingerprints {
			required = append(required, id)
		}
		sort.Strings(required)
	}
	var mm KeyMismatch
	for _, id := range required {
		have, ok := cur.FP[id]
		switch {
		case !ok:
			mm.Missing = append(mm.Missing, id)
		case m.KEKFingerprints[id] == "" || have != m.KEKFingerprints[id]:
			mm.Different = append(mm.Different, id)
		}
	}
	if len(required) == 0 {
		mm.Overall = true
	}
	if len(mm.Missing) == 0 && len(mm.Different) == 0 && !mm.Overall {
		return nil
	}
	return &mm
}
