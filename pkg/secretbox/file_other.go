//go:build !unix

package secretbox

func defaultAllowedUIDs() []uint32 { return nil }

// 非 Unix 平台没有 uid 属主与 0400/0440 语义，无法满足 ADR-011 的保管要求：拒绝（Server 仅支持 Linux）。
func readMasterKeyFile(string, []uint32) ([]byte, error) {
	return nil, invalid("此平台不支持文件主密钥（需要 Unix 权限与属主校验）")
}
