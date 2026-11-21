package main

import (
	"context"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// openBoxQuiet 打开主密钥（失败返回 nil：调用方只在确实需要解密运行期凭据时才会因此报错）。返回的 close 清零密钥。
func openBoxQuiet(cfg *config.Config) (*secretbox.Box, func()) {
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: cfg.Secrets.MasterKeyFile, CredentialsDir: secretbox.CredentialsDirFromEnv()})
	if err != nil {
		return nil, func() {}
	}
	box, err := secretbox.New(kp)
	if err != nil {
		kp.Close()
		return nil, func() {}
	}
	return box, kp.Close
}

// mergeRuntime 是各子命令（lineage 等）共用的「运行期凭据合并」入口（ADR-026 §3，与 server.Run 同口径）：显式通道（环境变量、*.password_file）优先，
// 缺省才读安装向导写入的 runtime-credentials.json。needMaint 为真时才解密 maint 口令。
func mergeRuntime(ctx context.Context, cfg *config.Config, needMaint bool) error {
	box, closeBox := openBoxQuiet(cfg)
	defer closeBox()
	_, err := dbcred.Merge(ctx, cfg, box, dbcred.MergeOptions{NeedMaint: needMaint})
	return err
}
