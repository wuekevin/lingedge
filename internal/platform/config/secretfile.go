package config

import "errors"

// ReadSecretFile 读取口令文件，检查与 `*.password_file` 配置项完全相同：绝对路径、普通文件、权限不得允许其他用户访问或同组写入、
// 非空。供不经 server.yaml 取得口令的入口使用（如 `lingedge-server migrate` 的迁移账号口令文件，ADR-026 §4）。
func ReadSecretFile(path string) (Secret, error) {
	s, reason := readSecretFile(path)
	if reason != "" {
		return Secret{}, errors.New(reason)
	}
	return s, nil
}
