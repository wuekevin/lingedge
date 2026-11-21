// Package signing 是配置签名（信任域②）与节点 mTLS CA（信任域③）的公共验证库（Story 1.23，ADR-002 §3、ADR-003 §3、ADR-012 §2、node-protocol §7/§8）。
//
// 节点（Agent）与 Server、Tools 共用本包：只依赖标准库，不依赖 internal/*，不含任何服务端专有依赖（数据库、HTTP 框架、信封加密），
// 也不含任何私钥「加载」逻辑——本包的签名函数只接收调用方已经持有的 ed25519.PrivateKey；私钥如何存放、何时解密、何时清零是服务端的事。
//
// 字节格式（与 proto/lingedge/node/v1 与 docs/api/node-protocol.md 一致）：
//   - 修订签名：Ed25519(priv, "LINGEDGE-REVISION-V1\n" ‖ manifest_json)，记录 key_id，**不带过期时间**，验证**不读取本机时钟**（R-4）；
//   - keyset 签名：Ed25519(priv, "LINGEDGE-KEYSET-V1\n" ‖ canonical_json(version, keys, issued_at, previous_version))，
//     新 keyset（version = N+1）由 version = N 中仍为 ACTIVE/RETIRING 的 key 签名；两个域前缀互不相同，一种签名不能被当作另一种验证（域分隔）；
//   - 指纹：配置签名公钥 = SHA-256(原始 32 字节公钥) 的小写十六进制；证书（CA / Server 叶证书）= SHA-256(证书 DER) 的小写十六进制
//     （CA 的即 enroll Token 的 ca_fp，ADR-003 §7）。
package signing
