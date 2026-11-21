package registry

import (
	"regexp"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// cleanText 对自由文本（命令行、原因）脱敏：journal 与锁文件只记路径/哈希/步骤，不记口令/Token（§12.3，复用 security，不另立规则）。
func cleanText(s string) string { return security.RedactString(s) }

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// requireClean 要求字符串经脱敏后不变；否则说明它疑似含密钥，拒绝落盘（而不是悄悄改写——改写会让 undo 的路径/值失真）。
func requireClean(field, s string) error {
	if security.RedactString(s) != s {
		return newErr(KindSecret, "%s 疑似含密钥/口令，拒绝写入（Tools 的状态与日志只记路径、哈希与步骤）", field)
	}
	return nil
}
