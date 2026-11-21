package cli

import "github.com/wuekevin/lingedge/internal/tools/downloader"

// DownloadError 把 internal/tools/downloader 的失败映射为 CLI 结构化错误（Story 1.28）。
// 与 VerifyError 同属退出码 5（VERIFY_FAILED）的生产者：
// 重定向到非 HTTPS、服务器证书不被信任、大小超过/不足清单 size、外层 SHA256 不符、包解压/逐文件校验失败、离线 bundle 非法 → 退出码 5；
// 纯传输失败（DNS/连接/超时/空闲超时/HTTP 状态，重试耗尽）→ 退出码 1（GENERAL，网络问题而非校验问题，脚本可据此区分「重试」与「告警」）；
// bundle 文件不存在、参数不合法 → 退出码 2；组件未发布/内置 → 7/8；清单里没有该版本/变体/平台 → 2；staging 无法创建 → 3。
func DownloadError(de *downloader.Error) *Error {
	e := &Error{Exit: ExitVerifyFailed, Message: de.Msg, Cause: de.Cause, SystemState: "unchanged"}
	if len(de.Details) > 0 {
		e.Details = map[string]any{}
		for k, v := range de.Details {
			e.Details[k] = v
		}
	}
	switch de.Kind {
	case downloader.KindInsecureURL:
		e.Code, e.Hint = "verify.insecure_transport", "下载只允许 HTTPS，且不允许重定向到 HTTP；请检查发行源/镜像配置。Tools 不提供 --insecure"
	case downloader.KindTLS:
		e.Code, e.Hint = "verify.tls_untrusted", "服务器证书不被信任：企业私有 CA 请经 release.ca_file 追加根证书；不要关闭证书校验"
	case downloader.KindSize:
		e.Code, e.Hint = "verify.size_mismatch", "下载大小与清单声明的 size 不符（超出或被截断）；请重试，持续出现请核对发行源"
	case downloader.KindSHA256:
		e.Code, e.Hint = "verify.sha256_mismatch", "包的 SHA256 与已签名清单不符（包被篡改或下载损坏）；已丢弃，请重新获取并核对发行源"
	case downloader.KindExtract:
		e.Code, e.Hint = "verify.package_extract", "包内容未通过校验（逐文件 SHA256、路径/类型限制或限额）；该包不可信，已丢弃"
	case downloader.KindBundle:
		e.Code, e.Hint = "verify.bundle_invalid", "离线 bundle 不合法：须为普通文件（不得为符号链接/目录/设备），并含 manifest.json、manifest.json.sig 与 packages/ 下清单引用的包"
	case downloader.KindBundleAbsent:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
		e.Hint = "--offline 指定的 bundle 文件不存在"
	case downloader.KindNetwork:
		e.Exit, e.Code = ExitGeneral, "download.network"
		e.Hint = "网络传输失败（已按配置重试）：检查网络、代理（HTTPS_PROXY）与发行源可达性后重试"
	case downloader.KindOptions:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
		e.Hint = "下载参数不合法（URL 须为不含凭据的 https；release.ca_file 须为有效 PEM；代理须为 http/https/socks5）"
	case downloader.KindUnreleased:
		e.Exit, e.Code = ExitNotReleased, "component.not_released"
		e.Hint = "该组件尚未发布"
	case downloader.KindBuiltin:
		e.Exit, e.Code = ExitBuiltin, "component.builtin"
		e.Hint = "该组件已内置于 Server，无需单独安装"
	case downloader.KindNoVersion:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
		e.Hint = "清单中没有所请求的版本/变体/平台"
	case downloader.KindStaging:
		e.Exit, e.Code = ExitPrecondition, "precondition.staging"
		e.Hint = "无法创建或清理 staging 目录：检查磁盘空间与目录权限（0700、属主 root）"
	default:
		e.Exit, e.Code = ExitGeneral, "download.unknown"
	}
	return e
}
