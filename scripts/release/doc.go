// Command release 是 LingEdge 的在线发行流水线（Story 1.34、ADR-012 §1.5、tools-design §9、docs/operations/release.md）。
//
// 流水线分三段，**在线机/CI 只做「产出待签物与组装」，没有任何签名代码、不接触任何私钥**（测试 TestOnlinePipelineHasNoSigningCode 机器核对；
// 私钥只在离线机的 scripts/release-sign 中使用）：
//
//	build     构建各平台二进制（固定 toolchain、-trimpath、-buildvcs=false、无 lingedge_testdouble 标签）→ 同源再构建一次（干净 GOCACHE、另一目录）并逐文件比对哈希
//	          （可复现性记录 repro.json，不一致即失败）→ 取 `lingedge-server migrate plan` 的迁移预告 → 组装各组件包的 stage 树
//	          （META/package.json、META/SHA256SUMS、payload/…；构建期消除符号链接，SBOM 随包）→ 产出第 1 轮待签清单 to-sign-1.json（包内 SHA256SUMS）。
//	finalize  收到第 1 轮 .sig 后：逐个用发行公钥集验签 → 写出最终 .tar.gz（META 在前、确定性）→ 生成待签 manifest.json（含 sequence/valid_until/ports/requires/
//	          migrations 预告）→ 用 internal/tools/manifest 做 schema 自检 → 产出第 2 轮待签清单 to-sign-2.json。
//	verify    收到 manifest.json.sig 后：用与 Tools 完全相同的校验链（manifest.Verifier + downloader.Acquire 的离线 bundle 路径）对每个包做真实验证，
//	          通过后才产出 publish/ 发行目录（<通道>/manifest.json(.sig)、<组件>/<版本>/*.tar.gz、离线 bundle、SBOM、RELEASE-RECORD.json）。
//
// 为什么是两轮签名：包内 META/SHA256SUMS.sig 在 .tar.gz 里面，包的外层 sha256 又写进清单，清单再被签名——先有包内签名，才有最终包与清单。
package main
