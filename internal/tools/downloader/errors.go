package downloader

import (
	"errors"
	"fmt"
)

// Kind 是下载器失败的稳定分类；cli.DownloadError 据此映射退出码（验证类一律 5）。
type Kind string

// 失败分类。
const (
	KindInsecureURL  Kind = "insecure_url"    // 非 https 的 URL，或重定向到非 https
	KindTLS          Kind = "tls_untrusted"   // 服务器证书校验失败（不在系统根/release.ca_file 中、主机名不符、过期）
	KindSize         Kind = "size_mismatch"   // 声明/实际大小超过清单 size，或截断（不足清单 size）
	KindSHA256       Kind = "sha256_mismatch" // 外层 SHA256 与清单不符（含断点续传被篡改）
	KindExtract      Kind = "package_extract" // 解压/包结构失败（SafeExtract 拒绝、META 缺失或乱序、清单与实际不符）
	KindBundle       Kind = "bundle_invalid"  // 离线 bundle 路径不是普通文件，或 bundle 布局/索引非法、缺少清单引用的包
	KindBundleAbsent Kind = "bundle_absent"   // bundle 文件不存在（用法错误）
	KindNetwork      Kind = "network"         // 传输失败：DNS/连接/超时/空闲超时/HTTP 状态；重试耗尽
	KindOptions      Kind = "options"         // 调用参数不合法
	KindUnreleased   Kind = "not_released"    // 组件 status=unreleased（退出码 7）
	KindBuiltin      Kind = "builtin"         // 组件 status=builtin（退出码 8）
	KindNoVersion    Kind = "no_such_version" // 清单中没有该版本/变体/平台
	KindStaging      Kind = "staging"         // staging 目录创建/清理失败
)

// Kinds 返回全部分类（cli 映射表的穷尽性测试用）。
func Kinds() []Kind {
	return []Kind{KindInsecureURL, KindTLS, KindSize, KindSHA256, KindExtract, KindBundle, KindBundleAbsent, KindNetwork, KindOptions,
		KindUnreleased, KindBuiltin, KindNoVersion, KindStaging}
}

// Error 是下载器的错误类型（校验链第 2–4 步的失败返回 *manifest.Error，不在此列）。
type Error struct {
	Kind    Kind
	Msg     string
	Cause   error
	Details map[string]any
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("downloader: %s: %s: %v", e.Kind, e.Msg, e.Cause)
	}
	return fmt.Sprintf("downloader: %s: %s", e.Kind, e.Msg)
}

// Unwrap 支持 errors.Is/As。
func (e *Error) Unwrap() error { return e.Cause }

func newErr(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func wrapErr(k Kind, cause error, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...), Cause: cause}
}

func (e *Error) with(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// IsKind 报告 err 链中是否有指定分类的 *Error。
func IsKind(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}
