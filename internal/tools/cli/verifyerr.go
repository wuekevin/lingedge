package cli

import (
	"path/filepath"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// KeysDir 是密钥轮换落盘目录（trust/keys.d）。
func (e Env) KeysDir() string { return filepath.Join(e.withDefaults().TrustDir, "keys.d") }

// NewVerifier 用内置公钥 + trust/keys.d + trust/state 装配生产的 manifest.Verifier（§9.5 第 1 步）。
// 未注入内置公钥、信任库损坏都返回 *manifest.Error（失败关闭）；调用方直接把它作为命令错误返回即可得到退出码 5/3。
func (e Env) NewVerifier() (*manifest.Verifier, error) {
	e = e.withDefaults()
	builtin, err := e.BuiltinKeys()
	if err != nil {
		return nil, err
	}
	ts, err := manifest.LoadTrust(builtin, e.KeysDir())
	if err != nil {
		return nil, err
	}
	return &manifest.Verifier{Trust: ts, Seen: manifest.FileSequenceStore{Path: filepath.Join(e.TrustDir, "state")}, Now: e.Now}, nil
}

// VerifyError 把 internal/tools/manifest 的验证失败映射为 CLI 结构化错误：这是退出码 5（VERIFY_FAILED，AC-J-2）的真实生产者。
// 命令实现（install/update，Story 1.33/1.43）直接返回 manifest 返回的 *manifest.Error 即可，Run.finish 会调用本函数；
// 清单验签失败、未知/吊销 key、sequence 回退、清单过期、revoked_versions 命中、key_rotation 背书失败、包内签名/META 不符一律退出码 5。
// 本地信任库（trust/keys.d、trust/state）被破坏是失败关闭的环境问题，退出码 3（PRECONDITION）；调用参数不合法是用法错误（2）。
func VerifyError(me *manifest.Error) *Error {
	e := &Error{Exit: ExitVerifyFailed, Message: me.Msg, Cause: me.Cause, SystemState: "unchanged"}
	if len(me.Details) > 0 {
		e.Details = map[string]any{}
		for k, v := range me.Details {
			e.Details[k] = v
		}
	}
	switch me.Kind {
	case manifest.KindSignature:
		e.Code, e.Hint = "verify.signature_invalid", "清单或包被篡改，或签名文件损坏；请重新获取，并核对发行源；不要绕过校验"
	case manifest.KindUnknownKey:
		e.Code, e.Hint = "verify.unknown_key_id", "签名所用公钥不在可信集合；升级 Tools 到包含该公钥的版本，或经受信渠道获取带 key_rotation 的清单"
	case manifest.KindKeyRevoked:
		e.Code, e.Hint = "verify.key_revoked", "签名公钥已被吊销；该清单/包不可信，请从发行方获取由有效公钥签名的新版本"
	case manifest.KindKeyInvalid:
		e.Code, e.Hint = "verify.key_not_valid", "签名公钥的状态或有效期不允许验证该清单/包"
	case manifest.KindRollback:
		e.Code, e.Hint = "verify.sequence_rollback", "清单 sequence 小于本机已见最大值（疑似回滚攻击或使用了旧缓存）；请获取最新清单"
	case manifest.KindExpired:
		e.Code, e.Hint = "verify.manifest_expired", "清单已过 valid_until；请获取最新清单。气隙环境可经 --offline 并显式加 --allow-expired-manifest 放行（会告警）"
	case manifest.KindSchema:
		e.Code, e.Hint = "verify.schema_invalid", "清单/公钥集的格式不被本版本 Tools 接受；若提示必需字段未知，请升级 Tools"
	case manifest.KindChannel:
		e.Code, e.Hint = "verify.channel_mismatch", "清单的发行通道与 --channel 不一致"
	case manifest.KindVersionRevoked:
		e.Code, e.Hint = "verify.version_revoked", "该版本已被发行方吊销，请选择其他版本"
	case manifest.KindRotation:
		e.Code, e.Hint = "verify.rotation_invalid", "key_rotation 的新公钥缺少旧 active key 的有效背书，已拒绝整份清单"
	case manifest.KindNoBuiltinKeys:
		e.Code, e.Hint = "verify.no_builtin_keys", "本 lingedge 构建未内置发行公钥，无法验证任何清单；请使用官方发行的 lingedge"
	case manifest.KindPackage:
		e.Code, e.Hint = "verify.package_invalid", "包内签名或 META/package.json 与清单不一致"
	case manifest.KindTrustStore:
		e.Exit, e.Code = ExitPrecondition, "trust.store_invalid"
		e.Hint = "本机信任库（/var/lib/lingedge/tools/trust/）不可读或被改动：检查权限（0700/0600、属主 root）与内容；为防回滚攻击，Tools 不会把损坏的状态当作空状态继续"
	case manifest.KindOptions:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
		e.Hint = "--allow-expired-manifest 必须与 --offline 一起使用"
	default:
		e.Exit, e.Code = ExitGeneral, "verify.unknown"
	}
	return e
}
