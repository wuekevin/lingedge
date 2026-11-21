package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 签名域前缀（tools-design §9.2/§9.3）。本文件是对规范的独立实现，不 import internal/tools/manifest。
const (
	manifestDomain = "LINGEDGE-MANIFEST-V1\n"
	packageDomain  = "LINGEDGE-PKG-V1\n"

	// KeyFileFormat 是离线私钥文件的格式标识。仓库密钥扫描（gates secrets）把「该标识 + seed 字段」视为私钥泄露。
	keyFileFormat = "lingedge-release-key-v1"
	toSignSchema  = "lingedge-release-tosign-v1"

	kindPackageSums = "package-sums"
	kindManifest    = "manifest"

	maxFileBytes = 64 << 20
)

var (
	reKeyID     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reComponent = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	rePlatform  = regexp.MustCompile(`^(any|linux-(amd64|arm64))$`)
	reVersion   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
	reSumLine   = regexp.MustCompile(`^[0-9a-f]{64}  payload/[^\n]+$`)
	reHex64     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// --- 密钥 ---------------------------------------------------------------------------------------------------

type keyFile struct {
	Format string `json:"format"`
	KeyID  string `json:"key_id"`
	Alg    string `json:"alg"`
	Seed   string `json:"seed"` // base64，32 字节
}

// signer 持有已加载的私钥。
type signer struct {
	keyID string
	priv  ed25519.PrivateKey
}

func (s *signer) public() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// pubEntry 是 keys.json 里的一把公钥（tools-design §9.1）。
type pubEntry struct {
	KeyID     string `json:"key_id"`
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"`
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	Status    string `json:"status"`
}

type keysDoc struct {
	SchemaVersion int        `json:"schema_version"`
	Keys          []pubEntry `json:"keys"`
	Revoked       []string   `json:"revoked_key_ids"`
}

// generateKey 生成新密钥并把私钥写入 path：O_EXCL（绝不覆盖）、0600。返回公钥条目。
func generateKey(keyID, path string, notBefore, notAfter string) (*pubEntry, error) {
	if !reKeyID.MatchString(keyID) {
		return nil, fmt.Errorf("key_id %q 非法（小写字母数字与 ._-，≤64）", keyID)
	}
	var tb, ta time.Time
	var err error
	if notBefore != "" {
		if tb, err = time.Parse(time.RFC3339, notBefore); err != nil {
			return nil, fmt.Errorf("-not-before 不是 RFC 3339: %w", err)
		}
	}
	if notAfter != "" {
		if ta, err = time.Parse(time.RFC3339, notAfter); err != nil {
			return nil, fmt.Errorf("-not-after 不是 RFC 3339: %w", err)
		}
	}
	if !tb.IsZero() && !ta.IsZero() && !ta.After(tb) {
		return nil, errors.New("-not-after 必须晚于 -not-before")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	kf := keyFile{Format: keyFileFormat, KeyID: keyID, Alg: "ed25519", Seed: base64.StdEncoding.EncodeToString(priv.Seed())}
	b, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("无法创建私钥文件（不覆盖已有文件）: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("写私钥文件失败: %v %v", werr, cerr)
	}
	// umask 可能收紧但不会放宽；显式 chmod 防御异常 umask 之外的情形。
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	s := &signer{keyID: keyID, priv: priv}
	return s.entry(notBefore, notAfter), nil
}

func (s *signer) entry(notBefore, notAfter string) *pubEntry {
	return &pubEntry{KeyID: s.keyID, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(s.public()),
		NotBefore: notBefore, NotAfter: notAfter, Status: "active"}
}

// loadKey 读取私钥文件：必须是普通文件（不跟随符号链接）、不得被 group/other 访问、格式严格。
func loadKey(path string) (*signer, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取私钥文件: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("私钥文件必须是普通文件（不接受符号链接/目录）")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("私钥文件权限 %04o 过宽：group/other 不得有任何权限（chmod 600）", fi.Mode().Perm())
	}
	if fi.Size() > 4<<10 {
		return nil, errors.New("私钥文件过大")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := strictJSON(b, &kf); err != nil {
		return nil, fmt.Errorf("私钥文件格式非法: %w", err)
	}
	if kf.Format != keyFileFormat || kf.Alg != "ed25519" || !reKeyID.MatchString(kf.KeyID) {
		return nil, errors.New("私钥文件 format/alg/key_id 非法")
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(kf.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("私钥文件的 seed 必须是 32 字节的标准 base64")
	}
	return &signer{keyID: kf.KeyID, priv: ed25519.NewKeyFromSeed(seed)}, nil
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("JSON 文档后有多余内容")
	}
	return nil
}

// --- 签名输入（tools-design §9.2/§9.3）----------------------------------------------------------------------

func manifestSigInput(manifestBytes []byte) []byte {
	return append([]byte(manifestDomain), manifestBytes...)
}

func packageSigInput(component, version, platform string, sums []byte) ([]byte, error) {
	if !reComponent.MatchString(component) {
		return nil, fmt.Errorf("component %q 非法", component)
	}
	if !reVersion.MatchString(version) {
		return nil, fmt.Errorf("version %q 非法", version)
	}
	if !rePlatform.MatchString(platform) {
		return nil, fmt.Errorf("platform %q 非法", platform)
	}
	return append([]byte(packageDomain+component+"\n"+version+"\n"+platform+"\n"), sums...), nil
}

type sigDoc struct {
	KeyID string `json:"key_id"`
	Alg   string `json:"alg"`
	Sig   string `json:"sig"`
}

func (s *signer) sigFile(input []byte) []byte {
	sig := ed25519.Sign(s.priv, input)
	b, _ := json.Marshal(sigDoc{KeyID: s.keyID, Alg: "ed25519", Sig: base64.StdEncoding.EncodeToString(sig)})
	return append(b, '\n')
}

// --- 待签清单 -----------------------------------------------------------------------------------------------

// ToSign 是在线流水线产出的待签清单（to-sign-<轮>.json）。
type ToSign struct {
	Schema  string `json:"schema"`
	Release string `json:"release"`
	Commit  string `json:"commit"`
	Round   int    `json:"round"`
	Items   []Item `json:"items"`
}

// Item 是一个待签对象。File/Sig 均为相对待签清单所在目录的路径。
type Item struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Sig    string `json:"sig"`

	Component   string `json:"component,omitempty"`
	Version     string `json:"version,omitempty"`
	Platform    string `json:"platform,omitempty"`
	FilesCount  int    `json:"files_count,omitempty"`
	UnpackedSz  int64  `json:"unpacked_size,omitempty"`
	Channel     string `json:"channel,omitempty"`
	Sequence    int64  `json:"sequence,omitempty"`
	GeneratedAt string `json:"generated_at,omitempty"`
	ValidUntil  string `json:"valid_until,omitempty"`
}

// loadToSign 读取待签清单，返回清单、所在目录与清单文件自身的 SHA-256（供人工与流水线控制台输出的摘要比对）。
func loadToSign(path string) (*ToSign, string, string, error) {
	b, err := readRegular(path)
	if err != nil {
		return nil, "", "", err
	}
	var ts ToSign
	if err := strictJSON(b, &ts); err != nil {
		return nil, "", "", fmt.Errorf("待签清单格式非法: %w", err)
	}
	if ts.Schema != toSignSchema {
		return nil, "", "", fmt.Errorf("待签清单 schema 必须是 %s", toSignSchema)
	}
	if ts.Round != 1 && ts.Round != 2 {
		return nil, "", "", errors.New("待签清单 round 必须是 1 或 2")
	}
	if len(ts.Items) == 0 || len(ts.Items) > 4096 {
		return nil, "", "", errors.New("待签清单条目数非法")
	}
	sum := sha256.Sum256(b)
	return &ts, filepath.Dir(path), hex.EncodeToString(sum[:]), nil
}

func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件（不接受符号链接/特殊文件）", path)
	}
	if fi.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s 过大", path)
	}
	return os.ReadFile(path)
}

// resolveInside 把 rel 解析到 dir 之内：必须是干净的相对路径，不含 .. 与绝对路径，沿途不得有符号链接。
func resolveInside(dir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || rel != filepath.ToSlash(filepath.Clean(rel)) || rel == ".." || strings.HasPrefix(rel, "../") || strings.ContainsRune(rel, '\\') || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("路径 %q 不是待签目录内的干净相对路径", rel)
	}
	cur := dir
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("路径 %q 含符号链接 %q", rel, part)
		}
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// prepared 是通过全部核对、待签名的对象。
type prepared struct {
	item   Item
	input  []byte
	sigOut string
	lines  []string // 给人看的摘要
}

// prepare 逐项核对：文件存在、SHA-256 与清单一致、内容语义自洽（清单字段/SHA256SUMS 行），然后构造签名输入。
func prepare(ts *ToSign, dir string) ([]prepared, error) {
	var out []prepared
	seen := map[string]bool{}
	for i, it := range ts.Items {
		if it.ID == "" || seen[it.ID] {
			return nil, fmt.Errorf("条目 %d 的 id 为空或重复", i)
		}
		seen[it.ID] = true
		if !reHex64.MatchString(it.SHA256) {
			return nil, fmt.Errorf("条目 %s 的 sha256 非法", it.ID)
		}
		path, err := resolveInside(dir, it.File)
		if err != nil {
			return nil, fmt.Errorf("条目 %s: %w", it.ID, err)
		}
		sigOut, err := resolveInside(dir, it.Sig)
		if err != nil {
			return nil, fmt.Errorf("条目 %s 的输出路径: %w", it.ID, err)
		}
		data, err := readRegular(path)
		if err != nil {
			return nil, fmt.Errorf("条目 %s: %w", it.ID, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != it.SHA256 {
			return nil, fmt.Errorf("条目 %s: 文件 SHA-256 与待签清单不符（文件在传递途中被改动？）", it.ID)
		}
		p := prepared{item: it, sigOut: sigOut}
		switch it.Kind {
		case kindPackageSums:
			if ts.Round != 1 {
				return nil, fmt.Errorf("条目 %s: package-sums 只能出现在第 1 轮", it.ID)
			}
			if err := checkSums(data, it); err != nil {
				return nil, fmt.Errorf("条目 %s: %w", it.ID, err)
			}
			if p.input, err = packageSigInput(it.Component, it.Version, it.Platform, data); err != nil {
				return nil, fmt.Errorf("条目 %s: %w", it.ID, err)
			}
			p.lines = []string{fmt.Sprintf("包内 SHA256SUMS  %s %s %s  %d 个文件 / %d 字节  sha256=%s", it.Component, it.Version, it.Platform, it.FilesCount, it.UnpackedSz, it.SHA256)}
		case kindManifest:
			if ts.Round != 2 {
				return nil, fmt.Errorf("条目 %s: manifest 只能出现在第 2 轮", it.ID)
			}
			lines, err := checkManifest(data, it)
			if err != nil {
				return nil, fmt.Errorf("条目 %s: %w", it.ID, err)
			}
			p.input = manifestSigInput(data)
			p.lines = append([]string{fmt.Sprintf("清单 manifest.json  通道=%s sequence=%d generated_at=%s valid_until=%s  sha256=%s", it.Channel, it.Sequence, it.GeneratedAt, it.ValidUntil, it.SHA256)}, lines...)
		default:
			return nil, fmt.Errorf("条目 %s: 未知 kind %q", it.ID, it.Kind)
		}
		out = append(out, p)
	}
	return out, nil
}

func checkSums(data []byte, it Item) error {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return errors.New("SHA256SUMS 必须以换行结尾且非空")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for _, l := range lines {
		if !reSumLine.MatchString(l) {
			return fmt.Errorf("SHA256SUMS 行格式非法: %q", trim(l, 60))
		}
	}
	if it.FilesCount != len(lines) {
		return fmt.Errorf("files_count %d 与 SHA256SUMS 行数 %d 不符", it.FilesCount, len(lines))
	}
	return nil
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// checkManifest 对清单做「签前常识检查」：它不替代 Tools 的完整 schema 校验，目的是不对明显错误的内容签名，并给签名人看内容摘要。
func checkManifest(data []byte, it Item) ([]string, error) {
	var m struct {
		SchemaVersion int    `json:"schema_version"`
		Channel       string `json:"channel"`
		Sequence      int64  `json:"sequence"`
		GeneratedAt   string `json:"generated_at"`
		ValidUntil    string `json:"valid_until"`
		Revoked       []struct {
			Component string `json:"component"`
			Version   string `json:"version"`
		} `json:"revoked_versions"`
		Components map[string]struct {
			Status   string `json:"status"`
			Versions []struct {
				Version  string `json:"version"`
				Variants map[string]struct {
					Artifacts map[string]struct {
						URL    string `json:"url"`
						SHA256 string `json:"sha256"`
						Size   int64  `json:"size"`
					} `json:"artifacts"`
				} `json:"variants"`
			} `json:"versions"`
		} `json:"components"`
		KeyRotation *struct {
			Keys []struct {
				KeyID string `json:"key_id"`
			} `json:"keys"`
		} `json:"key_rotation"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("清单不是合法 JSON: %w", err)
	}
	if m.SchemaVersion != 1 {
		return nil, errors.New("清单 schema_version 必须为 1")
	}
	if m.Channel != it.Channel || (m.Channel != "stable" && m.Channel != "beta") {
		return nil, fmt.Errorf("清单通道 %q 与待签清单声明 %q 不符或非法", m.Channel, it.Channel)
	}
	if m.Sequence < 1 || m.Sequence != it.Sequence {
		return nil, fmt.Errorf("清单 sequence %d 与待签清单声明 %d 不符或非法", m.Sequence, it.Sequence)
	}
	ga, err1 := time.Parse(time.RFC3339, m.GeneratedAt)
	vu, err2 := time.Parse(time.RFC3339, m.ValidUntil)
	if err1 != nil || err2 != nil || !vu.After(ga) || m.GeneratedAt != it.GeneratedAt || m.ValidUntil != it.ValidUntil {
		return nil, errors.New("清单 generated_at/valid_until 非法、valid_until 不晚于 generated_at，或与待签清单声明不符")
	}
	if m.Revoked == nil {
		return nil, errors.New("清单缺少 revoked_versions（缺失意味着「静默没有吊销」，Tools 会拒绝）")
	}
	var lines []string
	if m.KeyRotation != nil {
		var ids []string
		for _, k := range m.KeyRotation.Keys {
			ids = append(ids, k.KeyID)
		}
		lines = append(lines, "含 key_rotation，新公钥: "+strings.Join(ids, ","))
	}
	if len(m.Revoked) > 0 {
		lines = append(lines, "吊销版本 "+strconv.Itoa(len(m.Revoked))+" 项")
	}
	narts := 0
	for name, c := range m.Components {
		for _, v := range c.Versions {
			for vn, vr := range v.Variants {
				for plat, a := range vr.Artifacts {
					narts++
					if !strings.HasPrefix(a.URL, "https://") || strings.ContainsAny(a.URL, "@ \t\n") {
						return nil, fmt.Errorf("%s %s/%s/%s 的 url 必须是不含凭据的 https 地址", name, v.Version, vn, plat)
					}
					if !reHex64.MatchString(a.SHA256) || a.Size < 1 {
						return nil, fmt.Errorf("%s %s/%s/%s 的 sha256/size 非法", name, v.Version, vn, plat)
					}
					lines = append(lines, fmt.Sprintf("  %s %s %s %s  size=%d sha256=%s", name, v.Version, vn, plat, a.Size, trim(a.SHA256, 16)))
				}
			}
		}
	}
	if narts == 0 {
		return nil, errors.New("清单不含任何包（拒绝对空清单签名）")
	}
	sortStrings(lines)
	return lines, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// writeSigs 把签名写到各自的输出路径：O_EXCL（除非 force）、0644，不覆盖。
func writeSigs(s *signer, ps []prepared, force bool) error {
	for _, p := range ps {
		if !force {
			if _, err := os.Lstat(p.sigOut); err == nil {
				return fmt.Errorf("%s 已存在（用 -force 覆盖）", p.sigOut)
			}
		}
	}
	for _, p := range ps {
		if err := os.MkdirAll(filepath.Dir(p.sigOut), 0o755); err != nil {
			return err
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if force {
			_ = os.Remove(p.sigOut)
		}
		f, err := os.OpenFile(p.sigOut, flags, 0o644)
		if err != nil {
			return err
		}
		_, werr := f.Write(s.sigFile(p.input))
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return fmt.Errorf("写 %s 失败: %v %v", p.sigOut, werr, cerr)
		}
	}
	return nil
}

// --- 复核 ---------------------------------------------------------------------------------------------------

// verifyAll 用 keys.json 复核每个待签条目的 .sig。返回通过的条数。
func verifyAll(keysPath string, ts *ToSign, dir string) (int, error) {
	kb, err := readRegular(keysPath)
	if err != nil {
		return 0, err
	}
	var kd keysDoc
	if err := json.Unmarshal(kb, &kd); err != nil {
		return 0, fmt.Errorf("keys.json 非法: %w", err)
	}
	pubs := map[string]ed25519.PublicKey{}
	for _, k := range kd.Keys {
		raw, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || k.Alg != "ed25519" {
			return 0, fmt.Errorf("keys.json 中 %s 的公钥非法", k.KeyID)
		}
		pubs[k.KeyID] = raw
	}
	ps, err := prepare(ts, dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		sb, err := readRegular(p.sigOut)
		if err != nil {
			return n, fmt.Errorf("条目 %s: 缺少签名文件: %w", p.item.ID, err)
		}
		var sd sigDoc
		if err := strictJSON(sb, &sd); err != nil || sd.Alg != "ed25519" {
			return n, fmt.Errorf("条目 %s: 签名文件格式非法", p.item.ID)
		}
		pub, ok := pubs[sd.KeyID]
		if !ok {
			return n, fmt.Errorf("条目 %s: key_id %q 不在 keys.json 中", p.item.ID, sd.KeyID)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(sd.Sig)
		if err != nil || len(raw) != ed25519.SignatureSize || !ed25519.Verify(pub, p.input, raw) {
			return n, fmt.Errorf("条目 %s: 签名校验失败", p.item.ID)
		}
		n++
	}
	return n, nil
}

func fprintf(w io.Writer, f string, a ...any) { _, _ = fmt.Fprintf(w, f, a...) }

// --- 公钥轮换背书（tools-design §9.1 key_rotation，域前缀 LINGEDGE-KEYROT-V1）------------------------------------

const keyRotationDomain = "LINGEDGE-KEYROT-V1\n"

type endorsement struct {
	KeyID      string `json:"key_id"`
	EndorsedBy string `json:"endorsed_by"`
	Sig        string `json:"sig"`
}

type keyRotation struct {
	Keys         []pubEntry    `json:"keys"`
	Endorsements []endorsement `json:"endorsements"`
}

// keyRotationInput 是背书的签名输入：域前缀 + 以 \n 分隔的 key_id、alg、public_key、not_before、not_after、status（缺省为空串，末尾 \n）。
func keyRotationInput(k pubEntry) []byte {
	return []byte(keyRotationDomain + k.KeyID + "\n" + k.Alg + "\n" + k.PublicKey + "\n" + k.NotBefore + "\n" + k.NotAfter + "\n" + k.Status + "\n")
}

// endorse 用旧 active 私钥为 newKeys（keys.json 格式，每把必须 status=active）背书，返回清单 key_rotation 段。
func endorse(old *signer, newKeysPath string) (*keyRotation, error) {
	b, err := readRegular(newKeysPath)
	if err != nil {
		return nil, err
	}
	var kd keysDoc
	if err := json.Unmarshal(b, &kd); err != nil {
		return nil, fmt.Errorf("新公钥文件非法: %w", err)
	}
	if kd.SchemaVersion != 1 || len(kd.Keys) == 0 || len(kd.Keys) > 8 {
		return nil, errors.New("新公钥文件 schema_version 必须为 1，且含 1..8 把公钥")
	}
	kr := &keyRotation{}
	seen := map[string]bool{}
	for _, k := range kd.Keys {
		raw, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
		if !reKeyID.MatchString(k.KeyID) || k.Alg != "ed25519" || err != nil || len(raw) != ed25519.PublicKeySize || k.Status != "active" || seen[k.KeyID] {
			return nil, fmt.Errorf("新公钥 %q 非法（须 ed25519、32 字节公钥、status=active、key_id 不重复）", k.KeyID)
		}
		if k.KeyID == old.keyID {
			return nil, errors.New("新公钥不能与背书者同名")
		}
		seen[k.KeyID] = true
		sig := ed25519.Sign(old.priv, keyRotationInput(k))
		kr.Keys = append(kr.Keys, k)
		kr.Endorsements = append(kr.Endorsements, endorsement{KeyID: k.KeyID, EndorsedBy: old.keyID, Sig: base64.StdEncoding.EncodeToString(sig)})
	}
	return kr, nil
}
