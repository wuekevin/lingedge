// Package manifest 实现 LingEdge Tools 的发行信任域①（ADR-012、tools-design §9）：内置公钥集 keys.json 与 trust/keys.d/ 的装载、
// 清单 manifest.json 对「原始文件字节」的 Ed25519 验签（域前缀 LINGEDGE-MANIFEST-V1）、schema 校验（未知必需字段拒绝、未知可选字段忽略）、
// sequence 防回滚、valid_until 防冻结、revoked_versions 与 revoked_key_ids 吊销、key_rotation（旧 active key 背书）、
// 包内 META/SHA256SUMS 的签名对象（LINGEDGE-PKG-V1）与 META/package.json 校验，以及 semver 约束类型。
//
// 信任域隔离：本包只验签，不含任何签名（私钥）逻辑——发行私钥离线保管；它与 Server 的配置签名（pkg/signing，LINGEDGE-REVISION-V1/KEYSET-V1）
// 与节点 mTLS CA 互不相通：不 import pkg/signing，也不共享密钥或域前缀（isolation_test.go 机器核对）。
// 本包是 Tools 叶子：只依赖标准库、internal/tools/security 与 semver 库，且不读取系统时钟（时间由调用方注入）。
//
// 所有验证失败都返回 *Error，cli 层统一映射为退出码 5（VERIFY_FAILED），本地信任库损坏为退出码 3。
// 冻结点 CF-6 = 本包定义的 manifest/keys/包 META/签名对象格式（docs/bmad/contract-changes/CF-6.md，schema/ 目录）。
package manifest
