// Package secretbox 是 LingEdge 的信封加密库（ADR-011、NFR-12/13、FR-26）：Server、Tools 共用，只依赖标准库。
//
// 加密：每个被保护值使用独立随机 DEK（32B）做 AES-256-GCM；DEK 再由 KEK（主密钥）经 KeyProvider 包装。
// 密文格式固定为 5 段（base64url 无填充）：
//
//	v1:<kek_id>:<wrapped_dek>:<nonce>:<ct+tag>
//
// AAD = 表名|列名|记录 uid，同时绑定值加密与 DEK 包装：把记录 A 的密文复制到记录 B（或别的列）解密必败。
// 主密钥经 KeyProvider 提供（M1 默认 FileKeyProvider：文件 + 权限/属主校验，经 systemd LoadCredential 注入），
// 不入库、不入日志、不入备份产物。轮换只重包装 DEK（Box.Rewrap / RewrapAll），旧 KEK 全部重包装前不可移除（CheckKEKRemovable）。
//
// 进程内保护：密钥材料一律 []byte，使用后清零（Zero）；任何错误、格式化输出都不含密钥或明文。
// 已知限制：crypto/aes 内部展开的轮密钥由 Go 运行时持有，无法清零；systemd 单元以 LimitCORE=0 禁用 core dump 作为补偿
// （要求记入 Tools 渲染清单，Story 1.32；见 docs/operations/secrets.md）。
package secretbox
