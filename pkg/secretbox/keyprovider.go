package secretbox

import (
	"context"
	"errors"
	"regexp"
)

// KeyProvider 是主密钥（KEK）的提供者，即 KMS 抽象（ADR-011 §2）。实现可以是文件（FileKeyProvider）、
// 外部 KMS/HSM/云 KMS 插件（P5）。KEK 本身不经接口暴露：由实现自己完成 DEK 的包装与解包。
type KeyProvider interface {
	// CurrentKeyID 返回新写入使用的 kek_id。
	CurrentKeyID() string
	// Wrap 用 kekID 对应的 KEK 包装 dek（AES-256-GCM，aad 绑定记录）。dek 由调用方持有并负责清零。
	Wrap(ctx context.Context, kekID string, dek []byte, aad []byte) ([]byte, error)
	// Unwrap 解包 DEK；返回的切片由调用方清零。KEK 不存在返回 ErrKEKNotFound，认证失败返回 ErrDecrypt。
	Unwrap(ctx context.Context, kekID string, wrapped []byte, aad []byte) ([]byte, error)
}

// 哨兵错误。错误文本只含 kek_id 与固定说明，不含密钥、明文或密文内容。
var (
	ErrMasterKeyMissing = errors.New("secretbox: 缺少主密钥：未找到主密钥文件（配置 secrets.master_key_file 或经 systemd LoadCredential 提供 master.key），拒绝启动/解密")
	ErrMasterKeyInvalid = errors.New("secretbox: 主密钥文件被拒绝")
	ErrKEKNotFound      = errors.New("secretbox: 缺少所需的主密钥（kek_id 不在主密钥文件中；恢复旧备份需带上对应主密钥）")
	ErrNoCurrentKey     = errors.New("secretbox: KeyProvider 未给出当前 kek_id")
	ErrInvalidFormat    = errors.New("secretbox: 密文格式非法（应为 v1:<kek_id>:<wrapped_dek>:<nonce>:<ct+tag>）")
	ErrDecrypt          = errors.New("secretbox: 解密失败（密文被篡改、绑定的表/列/记录不符，或主密钥不对）")
	ErrClosed           = errors.New("secretbox: KeyProvider 已关闭（主密钥已清零）")
	ErrInvalidContext   = errors.New("secretbox: 表名/列名/记录 uid 非法（不得为空，且不得含 '|'）")
)

// kekIDPattern：kek_id 写入 VARCHAR(32) 的 enc_key_id，且是密文里的分段，故不得含 ':'。
var kekIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidKEKID 报告 kek_id 是否合法（小写字母/数字/_/-，1–32 位）。
func ValidKEKID(id string) bool { return kekIDPattern.MatchString(id) }
