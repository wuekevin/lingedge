package manifest

import "fmt"

// Kind 是验证失败的稳定分类（cli 据此映射错误码与提示）。
type Kind string

// 失败分类。除 KindTrustStore/KindOptions 外都对应退出码 5。
const (
	KindSignature      Kind = "signature_invalid" // 篡改清单/签名、签名格式非法、算法不符
	KindUnknownKey     Kind = "unknown_key_id"    // key_id 不在可信公钥集
	KindKeyRevoked     Kind = "key_revoked"       // key_id 在 revoked_key_ids（或 status=revoked）
	KindKeyInvalid     Kind = "key_not_valid"     // 公钥状态/有效期窗口不允许
	KindRollback       Kind = "sequence_rollback" // sequence 小于本地已见最大值
	KindExpired        Kind = "manifest_expired"  // valid_until 已过（未经 --offline --allow-expired-manifest 放行）
	KindSchema         Kind = "schema_invalid"    // 清单/公钥集/包 META schema 校验失败
	KindChannel        Kind = "channel_mismatch"  // 清单 channel 与请求通道不符
	KindVersionRevoked Kind = "version_revoked"   // revoked_versions 命中，不可作安装/升级目标
	KindRotation       Kind = "rotation_invalid"  // key_rotation 背书不成立
	KindNoBuiltinKeys  Kind = "no_builtin_keys"   // 本构建未注入发行公钥（生产构建由发行流水线注入）
	KindPackage        Kind = "package_invalid"   // 包内签名/META 与清单不一致
	KindTrustStore     Kind = "trust_store"       // 本地信任库（keys.d、trust/state）不可读或被破坏：失败关闭
	KindOptions        Kind = "options"           // 调用参数不合法（如 --allow-expired-manifest 不带 --offline）
)

// Error 是 manifest 包的唯一错误类型。
type Error struct {
	Kind    Kind
	Msg     string
	Cause   error
	Details map[string]any
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("manifest: %s: %s: %v", e.Kind, e.Msg, e.Cause)
	}
	return fmt.Sprintf("manifest: %s: %s", e.Kind, e.Msg)
}

// Unwrap 支持 errors.Is/As。
func (e *Error) Unwrap() error { return e.Cause }

func newErr(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func (e *Error) with(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// Kinds 返回全部失败分类（供 cli 的映射表做穷尽性测试：新增分类而忘记映射退出码会被测试发现）。
func Kinds() []Kind {
	return []Kind{KindSignature, KindUnknownKey, KindKeyRevoked, KindKeyInvalid, KindRollback, KindExpired, KindSchema, KindChannel,
		KindVersionRevoked, KindRotation, KindNoBuiltinKeys, KindPackage, KindTrustStore, KindOptions}
}
