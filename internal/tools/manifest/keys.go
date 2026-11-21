package manifest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Key 状态（tools-design §9.1）。
const (
	StatusActive  = "active"
	StatusRetired = "retired"
	StatusRevoked = "revoked"
)

// Key 是 keys.json 中的一把 Ed25519 公钥。
type Key struct {
	KeyID     string `json:"key_id"`
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"` // 标准 base64（带填充），32 字节
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	Status    string `json:"status"`

	pub    ed25519.PublicKey
	nb, na time.Time
}

// KeysFile 是 keys.json（内置）与 trust/keys.d/*.json（轮换落盘）共用的格式。
type KeysFile struct {
	SchemaVersion int      `json:"schema_version"`
	Keys          []Key    `json:"keys"`
	RevokedKeyIDs []string `json:"revoked_key_ids"`
}

var (
	reKeyID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reDirKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}\.json$`)
)

const maxKeysFileBytes = 256 << 10

// decodeStrict 解码 JSON：不允许未知字段与尾随内容（keys.json 是受信配置，拼写错误必须暴露而不是被忽略）。
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("JSON 文档后有多余内容")
	}
	var tail json.RawMessage
	if err := dec.Decode(&tail); err == nil {
		return errors.New("JSON 文档后有多余内容")
	}
	return nil
}

// ParseKey 校验并补全一把公钥（来自 keys.json 或 key_rotation）。
func (k *Key) validate() error {
	if !reKeyID.MatchString(k.KeyID) {
		return fmt.Errorf("key_id %q 非法（小写字母数字与 ._-，≤64）", trimTo(k.KeyID, 32))
	}
	if k.Alg != "ed25519" {
		return fmt.Errorf("key %s：alg 必须是 ed25519，收到 %q", k.KeyID, trimTo(k.Alg, 16))
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("key %s：public_key 必须是 32 字节的标准 base64", k.KeyID)
	}
	k.pub = ed25519.PublicKey(raw)
	switch k.Status {
	case StatusActive, StatusRetired, StatusRevoked:
	default:
		return fmt.Errorf("key %s：status 必须是 active|retired|revoked，收到 %q", k.KeyID, trimTo(k.Status, 16))
	}
	parse := func(name, s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("key %s：%s 不是 RFC 3339 时间: %q", k.KeyID, name, trimTo(s, 32))
		}
		return t, nil
	}
	if k.nb, err = parse("not_before", k.NotBefore); err != nil {
		return err
	}
	if k.na, err = parse("not_after", k.NotAfter); err != nil {
		return err
	}
	if !k.nb.IsZero() && !k.na.IsZero() && !k.na.After(k.nb) {
		return fmt.Errorf("key %s：not_after 必须晚于 not_before", k.KeyID)
	}
	return nil
}

// Public 返回公钥副本。
func (k Key) Public() ed25519.PublicKey { return append(ed25519.PublicKey(nil), k.pub...) }

// validAt：有效期窗口 [not_before, not_after)；窗口缺省视为无界。
func (k Key) validAt(t time.Time) bool {
	if !k.nb.IsZero() && t.Before(k.nb) {
		return false
	}
	if !k.na.IsZero() && !t.Before(k.na) {
		return false
	}
	return true
}

// ParseKeysFile 严格解析并校验 keys.json 格式的文档。
func ParseKeysFile(data []byte) (*KeysFile, error) {
	if len(data) == 0 || len(data) > maxKeysFileBytes {
		return nil, newErr(KindSchema, "keys 文档大小非法（%d 字节）", len(data))
	}
	var kf KeysFile
	if err := decodeStrict(data, &kf); err != nil {
		return nil, &Error{Kind: KindSchema, Msg: "keys 文档不是合法 JSON 或含未知字段", Cause: err}
	}
	if kf.SchemaVersion != 1 {
		return nil, newErr(KindSchema, "keys 文档 schema_version 必须为 1，收到 %d", kf.SchemaVersion)
	}
	seen := map[string]bool{}
	for i := range kf.Keys {
		if err := kf.Keys[i].validate(); err != nil {
			return nil, &Error{Kind: KindSchema, Msg: "公钥条目非法", Cause: err}
		}
		if seen[kf.Keys[i].KeyID] {
			return nil, newErr(KindSchema, "key_id %s 重复", kf.Keys[i].KeyID)
		}
		seen[kf.Keys[i].KeyID] = true
	}
	seenRev := map[string]bool{}
	for _, id := range kf.RevokedKeyIDs {
		if !reKeyID.MatchString(id) {
			return nil, newErr(KindSchema, "revoked_key_ids 含非法 key_id %q", trimTo(id, 32))
		}
		if seenRev[id] {
			return nil, newErr(KindSchema, "revoked_key_ids 重复项 %s", id)
		}
		seenRev[id] = true
	}
	return &kf, nil
}

// TrustSet 是可信公钥集：内置 keys.json ∪ trust/keys.d/*.json；吊销集是各文件 revoked_key_ids 的并集。
type TrustSet struct {
	keys    map[string]Key
	revoked map[string]struct{}
}

// NewTrustSet 合并若干 keys 文档；先出现者权威（内置在前）。同 key_id 不同公钥/算法视为冲突并拒绝
// （防止 keys.d 里的条目替换内置公钥）；status=revoked 的条目并入吊销集。至少要有一把非吊销公钥。
func NewTrustSet(files ...*KeysFile) (*TrustSet, error) {
	t := &TrustSet{keys: map[string]Key{}, revoked: map[string]struct{}{}}
	for _, f := range files {
		for _, id := range f.RevokedKeyIDs {
			t.revoked[id] = struct{}{}
		}
	}
	for _, f := range files {
		for _, k := range f.Keys {
			if k.Status == StatusRevoked {
				t.revoked[k.KeyID] = struct{}{}
			}
			if old, ok := t.keys[k.KeyID]; ok {
				if !bytes.Equal(old.pub, k.pub) {
					return nil, newErr(KindTrustStore, "key_id %s 在多个信任来源中公钥不一致，拒绝加载（失败关闭）", k.KeyID)
				}
				continue
			}
			t.keys[k.KeyID] = k
		}
	}
	usable := 0
	for id, k := range t.keys {
		if _, rev := t.revoked[id]; !rev && k.Status != StatusRevoked {
			usable++
		}
	}
	if usable == 0 {
		return nil, newErr(KindNoBuiltinKeys, "可信公钥集中没有任何未吊销的公钥")
	}
	return t, nil
}

// Lookup 按 key_id 取公钥：未知 → KindUnknownKey；已吊销 → KindKeyRevoked。
func (t *TrustSet) Lookup(id string) (Key, error) {
	if _, rev := t.revoked[id]; rev {
		return Key{}, newErr(KindKeyRevoked, "key_id %q 已被吊销", trimTo(id, 64)).with("key_id", trimTo(id, 64))
	}
	k, ok := t.keys[id]
	if !ok {
		return Key{}, newErr(KindUnknownKey, "key_id %q 不在可信公钥集", trimTo(id, 64)).with("key_id", trimTo(id, 64))
	}
	return k, nil
}

// IDs 返回全部可信（未吊销）key_id，升序。
func (t *TrustSet) IDs() []string {
	var out []string
	for id := range t.keys {
		if _, rev := t.revoked[id]; !rev {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (t *TrustSet) add(k Key) { t.keys[k.KeyID] = k }

// LoadTrust 从内置 keys.json 字节与 trust/keys.d/ 目录构建可信公钥集（§9.5 第 1 步）。
// builtin 为空 → KindNoBuiltinKeys（失败关闭）。keysDir 不存在视为没有轮换落盘；存在则逐个 *.json 严格解析，
// 非普通文件（符号链接等）、group/other 可写、解析失败都使整次加载失败——信任库被动过必须暴露而不是静默忽略。
func LoadTrust(builtin []byte, keysDir string) (*TrustSet, error) {
	if len(builtin) == 0 {
		return nil, newErr(KindNoBuiltinKeys, "本构建未注入发行公钥 keys.json，无法验证任何清单")
	}
	bf, err := ParseKeysFile(builtin)
	if err != nil {
		return nil, err
	}
	files := []*KeysFile{bf}
	if keysDir != "" {
		ents, err := os.ReadDir(keysDir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, &Error{Kind: KindTrustStore, Msg: "无法读取信任目录 " + keysDir, Cause: err}
		default:
			for _, e := range ents { // ReadDir 已按文件名排序
				if !reDirKey.MatchString(e.Name()) {
					continue
				}
				p := filepath.Join(keysDir, e.Name())
				data, err := readTrustFile(p)
				if err != nil {
					return nil, err
				}
				kf, err := ParseKeysFile(data)
				if err != nil {
					return nil, &Error{Kind: KindTrustStore, Msg: "信任文件 " + p + " 无效", Cause: err}
				}
				files = append(files, kf)
			}
		}
	}
	return NewTrustSet(files...)
}

// readTrustFile 读取信任库里的文件：必须是普通文件（Lstat，不跟随符号链接）且 group/other 不可写。
func readTrustFile(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, &Error{Kind: KindTrustStore, Msg: "无法读取 " + p, Cause: err}
	}
	if !fi.Mode().IsRegular() {
		return nil, newErr(KindTrustStore, "%s 不是普通文件（符号链接/特殊文件不被信任）", p)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, newErr(KindTrustStore, "%s 权限 %o 允许 group/other 写入，拒绝信任", p, fi.Mode().Perm())
	}
	if fi.Size() > maxKeysFileBytes {
		return nil, newErr(KindTrustStore, "%s 过大", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, &Error{Kind: KindTrustStore, Msg: "无法读取 " + p, Cause: err}
	}
	return data, nil
}
