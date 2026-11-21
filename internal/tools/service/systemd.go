package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// systemd 版本门槛。tools-design §21 与 ADR-011 §9 只写「实现时核对」而没有给数字，这里的数字取自 systemd NEWS
// （各指令的引入版本，见 DirectiveSince），属本 Story 的核对结果，真实发行版上的复核登记为 ENV-LNX。
const (
	// MinSystemd 是 LingEdge 全部加固单元可用的最低 systemd 主版本：模板用到的指令中引入最晚的是
	// LockPersonality= 与 RuntimeDirectoryPreserve=（均为 235）。低于它的系统（如 RHEL 7 的 219）不在支持范围，doctor 报 fail。
	MinSystemd = 235
	// CredentialMinSystemd 是 LoadCredential= 的最低主版本（systemd 247 引入 service credentials 与 $CREDENTIALS_DIRECTORY）。
	// 低于它但不低于 MinSystemd 的系统（如 RHEL 8 的 239、Ubuntu 20.04 的 245）走 ADR-027 的降级变体。
	CredentialMinSystemd = 247
)

// DirectiveSince 列出模板用到的、引入较晚（>= 209）的指令及其引入的 systemd 主版本。
// 测试断言：模板里出现的每个指令要么在此表，要么在 oldDirectives（早于 209）；因此新增指令必须登记版本，且 MinSystemd 恒等于表中最大值。
var DirectiveSince = map[string]int{
	"SystemCallArchitectures":  209,
	"PrivateDevices":           208,
	"RestrictAddressFamilies":  211,
	"RuntimeDirectory":         211,
	"ProtectSystem":            214,
	"ProtectHome":              214,
	"AmbientCapabilities":      229,
	"StartLimitIntervalSec":    230, // [Unit] 段；此前叫 StartLimitInterval 且在 [Service] 段
	"MemoryDenyWriteExecute":   231,
	"RestrictRealtime":         231,
	"ReadWritePaths":           231,
	"ProtectKernelTunables":    232,
	"ProtectKernelModules":     232,
	"ProtectControlGroups":     232,
	"RestrictNamespaces":       233,
	"LockPersonality":          235,
	"RuntimeDirectoryPreserve": 235,
}

// Version 是 systemctl --version 解析出的版本。
type Version struct {
	Major int
	Raw   string // 第一行原文（已 trim），用于消息
}

var reVersionLine = regexp.MustCompile(`^systemd\s+v?(\d{1,6})(?:[.\s~+-].*)?$`)

// ParseVersion 解析 `systemctl --version`（或 `systemd-analyze --version`）的输出。
// 样例（均见 testdata/systemd）：`systemd 219`、`systemd 245.4-4ubuntu3.22`、`systemd 252 (252.22-1~deb12u1)`、
// `systemd 257.2-1.fc41`；第二行起是 `+PAM -SELINUX …` 特性串，忽略。取第一个非空行，不匹配即失败（不猜测）。
func ParseVersion(out string) (Version, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := reVersionLine.FindStringSubmatch(line)
		if m == nil {
			return Version{}, newErr(KindExec, "无法解析 systemd 版本：%q", truncate(line, 80))
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return Version{}, newErr(KindExec, "无法解析 systemd 版本：%q", truncate(line, 80))
		}
		return Version{Major: n, Raw: line}, nil
	}
	return Version{}, newErr(KindExec, "systemd 版本输出为空")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// SupportsUnits 报告本机 systemd 是否能运行 LingEdge 的加固单元（>= MinSystemd）。
func (v Version) SupportsUnits() bool { return v.Major >= MinSystemd }

// SupportsCredentials 报告是否支持 LoadCredential=（>= CredentialMinSystemd）。
func (v Version) SupportsCredentials() bool { return v.Major >= CredentialMinSystemd }

// SelectCredentialMode 按版本选择主密钥交付方式：支持 → CredentialSystemd，否则降级为 CredentialKeyFile（ADR-027）。
// 低于 MinSystemd 返回错误（单元本身都不能用，降级也无意义）。
func SelectCredentialMode(v Version) (CredentialMode, error) {
	if !v.SupportsUnits() {
		return "", newErr(KindUnsupported, "systemd %d 低于最低支持版本 %d，无法运行加固单元", v.Major, MinSystemd)
	}
	if v.SupportsCredentials() {
		return CredentialSystemd, nil
	}
	return CredentialKeyFile, nil
}

// Finding 是 doctor 检查项（形状与 tools-design §6 的 data.checks[] 一致：level 为 pass|warn|fail|skip）。
type Finding struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Level    string `json:"level"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
}

// Level 取值。
const (
	LevelPass = "pass"
	LevelWarn = "warn"
	LevelFail = "fail"
	LevelSkip = "skip"
)

// CheckSystemdVersion 把版本转成 doctor 检查项（供 1.42 使用）：
// 低于 MinSystemd → fail；低于 CredentialMinSystemd → warn（主密钥已降级为文件方式，ADR-027）；否则 pass。
func CheckSystemdVersion(v Version) Finding {
	f := Finding{ID: "service.systemd_version", Category: "service"}
	switch {
	case !v.SupportsUnits():
		f.Level = LevelFail
		f.Message = fmt.Sprintf("systemd %d 低于最低支持版本 %d：加固单元无法运行", v.Major, MinSystemd)
		f.Hint = "升级到受支持的发行版（Debian 10+/Ubuntu 18.04+/RHEL 8+）"
	case !v.SupportsCredentials():
		f.Level = LevelWarn
		f.Message = fmt.Sprintf("systemd %d 不支持 LoadCredential=（需要 %d+）：Server 主密钥已降级为 root:%s 0440 的密钥文件方式", v.Major, CredentialMinSystemd, "<运行组>")
		f.Hint = "可用但弱于 systemd credentials（运行组可读主密钥文件）；升级 systemd/发行版后重新安装单元即可恢复（ADR-027）"
	default:
		f.Level = LevelPass
		f.Message = fmt.Sprintf("systemd %d 支持 LoadCredential=", v.Major)
	}
	return f
}

// CheckCredentialConsistency 校验已安装的 Server 单元与本机版本、与主密钥文件权限计划是否一致（doctor 用）：
// 单元内容含 LoadCredential= 但本机版本不支持 → fail（服务将无法启动）；单元是降级变体而本机已支持 → warn（可重新安装以恢复更强的交付方式）。
func CheckCredentialConsistency(unitContent []byte, v Version) Finding {
	f := Finding{ID: "service.credential_mode", Category: "service"}
	hasLoad := false
	for _, ln := range strings.Split(string(unitContent), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "LoadCredential=") {
			hasLoad = true
		}
	}
	switch {
	case hasLoad && !v.SupportsCredentials():
		f.Level, f.Message = LevelFail, fmt.Sprintf("单元使用 LoadCredential=，但本机 systemd %d 不支持（需要 %d+）：Server 将无法读取主密钥", v.Major, CredentialMinSystemd)
		f.Hint = "以降级变体重新渲染 Server 单元（既有单元不会被覆盖，新内容写入 .new 待合并，FR-6；ADR-027）"
	case !hasLoad && v.SupportsCredentials():
		f.Level, f.Message = LevelWarn, fmt.Sprintf("单元为主密钥降级变体，但本机 systemd %d 已支持 LoadCredential=", v.Major)
		f.Hint = "可重新安装单元并把 master.key 改回 root:root 0400（ADR-027）"
	case hasLoad:
		f.Level, f.Message = LevelPass, "主密钥经 LoadCredential= 注入"
	default:
		f.Level, f.Message = LevelPass, "主密钥经 secrets.master_key_file 读取（降级变体，与本机 systemd 版本一致）"
	}
	return f
}
