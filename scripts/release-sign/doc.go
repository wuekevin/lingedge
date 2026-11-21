// Command release-sign 是 LingEdge 发行签名人的离线签名工具（Story 1.34 AC2、ADR-012 §1.5、tools-design §9）。
//
// 它是独立程序（独立 Go module，只依赖标准库），**只在离线机上运行**：发行私钥只存在于签名人的离线机/硬件介质，
// 绝不进入仓库、CI 或在线流水线（在线侧的 scripts/release 没有任何签名代码，且由测试保证）。
//
// 子命令：
//
//	keygen   生成新的发行密钥对：私钥文件（0600，O_EXCL，不覆盖）+ 可并入 keys.json 的公钥条目；
//	pubkey   由私钥文件导出公钥条目；
//	sign     读取在线流水线产出的待签清单 to-sign-<轮>.json，核对每个待签文件的 SHA-256 与内容，显示摘要并经人工确认后，
//	         对清单（LINGEDGE-MANIFEST-V1）与包内 SHA256SUMS（LINGEDGE-PKG-V1）签名，输出 .sig（JSON {key_id,alg,sig}）；
//	verify   用公钥集（keys.json）复核已产出的 .sig（离线自检；与 Tools 的验签是两个独立实现）。
//
// 签名输入严格遵循 tools-design §9.2/§9.3，并与 internal/tools/manifest 的实现互为独立实现：二者经 §9.6 示例向量、
// 运行时随机密钥的端到端流程以及 Ruby OpenSSL 交叉验证对账（见本目录测试与 tests/release）。
package main
