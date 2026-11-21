// Package setuptoken 实现安装向导 setup token 的生成、哈希与校验（tools-design §13、ADR-026 §1、F-8）。
//
// 分工：setup token 的**生成与哈希文件的创建**属于 Tools（步骤 18 附近：显示一次，哈希写入
// /etc/lingedge/server/setup-token.hash）；Server 的 setup mode 只**读取并校验**，并在 complete 之后删除哈希文件。
// 本包是两侧共享的无依赖原语：Generate / Hash / Verify / WriteHashFile / ReadHashFile。Story 1.35（Tools 安装联调）直接调用 Generate 与
// WriteHashFile，不需要另行实现，也不需要在 CLI 里提前注册任何假命令。
//
// 约定：token = 32 字节 crypto/rand（256 位）的 base64url（无填充，43 字符）；哈希 = SHA-256(token 的 UTF-8 字节) 的小写十六进制（64 字符）。
// 校验用常量时间比较；哈希文件内容只有该十六进制串（可带结尾换行）。
package setuptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHashPath 是哈希文件的缺省位置（tools-design §13）。
const DefaultHashPath = "/etc/lingedge/server/setup-token.hash"

const (
	tokenBytes = 32
	hashLen    = sha256.Size * 2 // 十六进制长度
	maxFile    = 256
)

// ErrNoHash：哈希文件不存在（已安装完成，或 Tools 尚未生成）。
var ErrNoHash = errors.New("setuptoken: 哈希文件不存在")

// Generate 生成一个新的 setup token 与它的 SHA-256 哈希（小写十六进制）。token 只应显示给操作者一次，不得落盘、不得写日志。
func Generate() (token, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("setuptoken: 随机数: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	for i := range b {
		b[i] = 0
	}
	return token, Hash(token), nil
}

// Hash 返回 token 的 SHA-256 小写十六进制。
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Verify 以常量时间比较 token 与哈希（hexHash 为哈希文件内容，大小写不敏感、可带首尾空白）。哈希格式非法一律视为不匹配。
func Verify(token, hexHash string) bool {
	want, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(hexHash)))
	if err != nil || len(want) != sha256.Size || token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// WriteHashFile 原子地写入哈希文件（同目录临时文件 + fsync + rename），权限 mode（tools-design §13 为 lingedge:lingedge 0600；
// 属主调整由调用方（Tools，root）在返回后 chown）。hash 必须是 Hash 的输出格式。
func WriteHashFile(path, hash string, mode os.FileMode) error {
	if len(hash) != hashLen {
		return errors.New("setuptoken: 哈希格式非法")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return errors.New("setuptoken: 哈希格式非法")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".setup-token-*")
	if err != nil {
		return fmt.Errorf("setuptoken: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(name) }
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("setuptoken: chmod: %w", err)
	}
	if _, err := tmp.WriteString(hash + "\n"); err != nil {
		cleanup()
		return fmt.Errorf("setuptoken: 写入: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("setuptoken: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("setuptoken: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("setuptoken: rename: %w", err)
	}
	return nil
}

// ReadHashFile 读取哈希文件内容（十六进制串）。文件不存在返回 ErrNoHash；非普通文件/过大/格式非法返回错误（调用方 fail-closed：拒绝所有写接口）。
func ReadHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoHash
		}
		return "", fmt.Errorf("setuptoken: 打开哈希文件: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("setuptoken: 哈希文件不是普通文件")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(b) > maxFile {
		return "", errors.New("setuptoken: 哈希文件读取失败或过大")
	}
	h := strings.TrimSpace(string(b))
	if len(h) != hashLen {
		return "", errors.New("setuptoken: 哈希文件格式非法")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("setuptoken: 哈希文件格式非法")
	}
	return strings.ToLower(h), nil
}
