package secretbox

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// 主密钥文件格式（文本，由 Tools 安装步骤 18 用 crypto/rand 生成；ADR-011 §2、tools-design §13）：
//
//	# 注释与空行忽略
//	@current k2            （可选；缺省取最后一条密钥）
//	k1:<base64url 无填充，32 字节随机值>
//	k2:<base64url 无填充，32 字节随机值>
//
// 一个文件可含多个 kek_id（轮换期间新旧并存）。
const (
	masterKeyName   = "master.key"
	maxMasterKeyLen = 64 << 10
	kekSize         = 32
)

// FileOptions 配置 NewFileKeyProvider。
type FileOptions struct {
	// Path 是配置的主密钥文件绝对路径（secrets.master_key_file）；非空时优先于 CredentialsDir。
	Path string
	// CredentialsDir 是 $CREDENTIALS_DIRECTORY（systemd LoadCredential），读取其下 master.key；用 CredentialsDirFromEnv 取得。
	CredentialsDir string
	// AllowedUIDs 是允许的文件属主；nil 时取 {当前进程 euid, 0}（运行用户自身或 root）。
	AllowedUIDs []uint32
}

// CredentialsDirFromEnv 返回环境变量 CREDENTIALS_DIRECTORY 的值（可能为空）。
func CredentialsDirFromEnv() string { return os.Getenv("CREDENTIALS_DIRECTORY") }

// ResolveMasterKeyPath 决定主密钥文件路径：配置路径优先，其次 $CREDENTIALS_DIRECTORY/master.key；都没有则 ErrMasterKeyMissing。
func ResolveMasterKeyPath(configured, credentialsDir string) (string, error) {
	switch {
	case configured != "":
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("%w：路径必须是绝对路径", ErrMasterKeyInvalid)
		}
		return filepath.Clean(configured), nil
	case credentialsDir != "":
		if !filepath.IsAbs(credentialsDir) {
			return "", fmt.Errorf("%w：CREDENTIALS_DIRECTORY 必须是绝对路径", ErrMasterKeyInvalid)
		}
		return filepath.Join(credentialsDir, masterKeyName), nil
	}
	return "", ErrMasterKeyMissing
}

// FileKeyProvider 是 M1 默认的 KeyProvider：从文件读取一个或多个 KEK，常驻内存（[]byte），Close 时清零。
//
// 状态放在内部指针后：即使调用方按值拷贝并用 fmt 反射打印，输出的也只是指针地址而不是 KEK 字节。
type FileKeyProvider struct{ s *fileState }

type fileState struct {
	mu      sync.RWMutex
	keys    map[string][]byte
	current string
	closed  bool
}

// NewFileKeyProvider 读取并校验主密钥文件：不存在 → ErrMasterKeyMissing；权限/属主/类型/格式不符 → ErrMasterKeyInvalid。
// 任何失败都应让调用方拒绝启动（fail-closed）。
func NewFileKeyProvider(o FileOptions) (*FileKeyProvider, error) {
	path, err := ResolveMasterKeyPath(o.Path, o.CredentialsDir)
	if err != nil {
		return nil, err
	}
	allowed := o.AllowedUIDs
	if allowed == nil {
		allowed = defaultAllowedUIDs()
	}
	data, err := readMasterKeyFile(path, allowed)
	if err != nil {
		return nil, err
	}
	defer Zero(data)
	keys, current, err := parseMasterKeys(data)
	if err != nil {
		for _, k := range keys {
			Zero(k)
		}
		return nil, err
	}
	return &FileKeyProvider{s: &fileState{keys: keys, current: current}}, nil
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w：%s", ErrMasterKeyInvalid, fmt.Sprintf(format, a...))
}

// checkMode 要求权限恰为 0400 或 0440 且无 setuid/setgid/sticky。
func checkMode(m os.FileMode) error {
	if !m.IsRegular() {
		return invalid("必须是普通文件")
	}
	if m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return invalid("不得带 setuid/setgid/sticky 位")
	}
	if m.Perm()&^0o440 != 0 || m.Perm()&0o400 == 0 {
		return invalid("权限必须是 0400 或 0440（当前 %04o）", m.Perm())
	}
	return nil
}

func readAll(f io.Reader) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(f, maxMasterKeyLen+1))
	if err != nil {
		Zero(buf)
		return nil, invalid("读取失败")
	}
	if len(buf) > maxMasterKeyLen {
		Zero(buf)
		return nil, invalid("文件过大")
	}
	return buf, nil
}

func parseMasterKeys(data []byte) (keys map[string][]byte, current string, err error) {
	keys = map[string][]byte{}
	var last, declared string
	for i, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		n := i + 1
		if line[0] == '@' {
			dir, arg, _ := bytes.Cut(line, []byte(" "))
			if string(dir) != "@current" || declared != "" {
				return keys, "", invalid("第 %d 行：未知或重复的指令", n)
			}
			declared = string(bytes.TrimSpace(arg))
			continue
		}
		id, enc, ok := bytes.Cut(line, []byte(":"))
		if !ok || !ValidKEKID(string(id)) {
			return keys, "", invalid("第 %d 行：格式应为 <kek_id>:<base64url 32 字节>", n)
		}
		if _, dup := keys[string(id)]; dup {
			return keys, "", invalid("第 %d 行：kek_id 重复", n)
		}
		key := make([]byte, b64.DecodedLen(len(enc)))
		m, derr := b64.Decode(key, enc)
		key = key[:m]
		if derr != nil || m != kekSize || allZero(key) {
			Zero(key)
			return keys, "", invalid("第 %d 行：主密钥必须是 32 字节随机值的 base64url（无填充），且不得全零", n)
		}
		keys[string(id)] = key
		last = string(id)
	}
	if len(keys) == 0 {
		return keys, "", invalid("文件中没有任何主密钥")
	}
	current = last
	if declared != "" {
		if _, ok := keys[declared]; !ok {
			return keys, "", invalid("@current 指向不存在的 kek_id")
		}
		current = declared
	}
	return keys, current, nil
}

func allZero(b []byte) bool {
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return acc == 0
}

// CurrentKeyID 实现 KeyProvider。
func (p *FileKeyProvider) CurrentKeyID() string {
	p.s.mu.RLock()
	defer p.s.mu.RUnlock()
	return p.s.current
}

// KeyIDs 返回已加载的全部 kek_id（升序）。kek_id 不是秘密。
func (p *FileKeyProvider) KeyIDs() []string {
	p.s.mu.RLock()
	defer p.s.mu.RUnlock()
	ids := make([]string, 0, len(p.s.keys))
	for id := range p.s.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (p *FileKeyProvider) aead(kekID string) (cipher.AEAD, error) {
	if p.s.closed {
		return nil, ErrClosed
	}
	k, ok := p.s.keys[kekID]
	if !ok {
		return nil, fmt.Errorf("%w: kek_id=%s", ErrKEKNotFound, kekID)
	}
	return newGCM(k)
}

// Wrap 实现 KeyProvider：wrapped = nonce(12) || AES-256-GCM(KEK, dek, aad)。
func (p *FileKeyProvider) Wrap(_ context.Context, kekID string, dek, aad []byte) ([]byte, error) {
	if len(dek) != dekSize {
		return nil, errors.New("secretbox: DEK 长度必须为 32 字节")
	}
	p.s.mu.RLock()
	defer p.s.mu.RUnlock()
	a, err := p.aead(kekID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize, nonceSize+dekSize+tagSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: 生成 nonce 失败: %w", err)
	}
	return a.Seal(nonce, nonce, dek, aad), nil
}

// Unwrap 实现 KeyProvider。
func (p *FileKeyProvider) Unwrap(_ context.Context, kekID string, wrapped, aad []byte) ([]byte, error) {
	if len(wrapped) < nonceSize+tagSize {
		return nil, ErrInvalidFormat
	}
	p.s.mu.RLock()
	defer p.s.mu.RUnlock()
	a, err := p.aead(kekID)
	if err != nil {
		return nil, err
	}
	dek, err := a.Open(nil, wrapped[:nonceSize], wrapped[nonceSize:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// Close 清零全部 KEK；之后所有操作返回 ErrClosed。可重复调用。
func (p *FileKeyProvider) Close() {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	for _, k := range p.s.keys {
		Zero(k)
	}
	p.s.closed = true
}

// String/GoString/Format：任何格式化只输出占位文本，绝不输出 KEK。
func (FileKeyProvider) String() string     { return "secretbox.FileKeyProvider{***}" }
func (p FileKeyProvider) GoString() string { return p.String() }
func (p FileKeyProvider) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(p.String()))
}

var _ KeyProvider = (*FileKeyProvider)(nil)
